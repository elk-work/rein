package codex

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

func TestManifestIsWellFormedAndRegistered(t *testing.T) {
	a := New()
	m := a.Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest does not validate: %v", err)
	}
	if a.Name() != m.Kind || m.Kind != Kind {
		t.Errorf("Name %q, manifest kind %q, package Kind %q must all agree", a.Name(), m.Kind, Kind)
	}
	if !hostSupported(m) {
		t.Errorf("the manifest does not cover this host; it claims %v", m.Platforms)
	}

	// The registry entry comes from init, so importing the package is the
	// whole wiring (docs/adapters.md).
	got, err := adapter.Get(Kind)
	if err != nil {
		t.Fatalf("adapter.Get(%q): %v", Kind, err)
	}
	if got.Name() != Kind {
		t.Errorf("registry holds %q under %q", got.Name(), Kind)
	}

	for _, c := range adapter.Capabilities() {
		if _, declared := m.Capabilities[c]; !declared {
			t.Errorf("capability %q is undeclared; declare a `no` rather than leaving a gap", c)
		}
	}
	for _, want := range []adapter.Capability{
		adapter.CapGit, adapter.CapWorktree, adapter.CapFileEdit, adapter.CapShell,
		adapter.CapMCP, adapter.CapResume, adapter.CapStructuredEvents, adapter.CapApprovals,
	} {
		if !m.Has(want) {
			t.Errorf("capability %q is not declared yes", want)
		}
	}
}

func TestPermissionFlagsCoverEveryMode(t *testing.T) {
	cases := []struct {
		mode     adapter.PermissionMode
		sandbox  sandboxMode
		approval askForApproval
	}{
		{adapter.PermissionReadOnly, sandboxReadOnly, approvalNever},
		{adapter.PermissionAcceptEdits, sandboxWorkspaceWrite, approvalOnRequest},
		{adapter.PermissionAsk, sandboxWorkspaceWrite, approvalUntrusted},
		{adapter.PermissionFull, sandboxFullAccess, approvalNever},
	}
	for _, c := range cases {
		sb, ap, err := permissionFlags(c.mode)
		if err != nil {
			t.Errorf("%s: %v", c.mode, err)
			continue
		}
		if sb != c.sandbox || ap != c.approval {
			t.Errorf("%s → %q/%q, want %q/%q", c.mode, sb, ap, c.sandbox, c.approval)
		}
	}
	if _, _, err := permissionFlags(adapter.PermissionUnset); err == nil {
		t.Error("the unset mode was mapped; it must be rejected, not guessed at")
	}
}

func TestStartRefusesRatherThanDegrades(t *testing.T) {
	a := New()
	a.spawn = func(context.Context, adapter.RunSpec) (*process, error) {
		t.Fatal("Start spawned a process for a spec it should have refused")
		return nil, nil
	}

	t.Run("no permission mode", func(t *testing.T) {
		_, err := a.Start(context.Background(), adapter.RunSpec{
			WorktreeDir: t.TempDir(), Prompt: "hi",
		})
		if err == nil {
			t.Fatal("a spec with no permission mode was accepted")
		}
	})

	t.Run("no worktree", func(t *testing.T) {
		_, err := a.Start(context.Background(), adapter.RunSpec{
			Prompt: "hi", PermissionMode: adapter.PermissionFull,
		})
		if err == nil {
			t.Fatal("a spec with no worktree was accepted")
		}
	})

	t.Run("tool allow list", func(t *testing.T) {
		_, err := a.Start(context.Background(), adapter.RunSpec{
			WorktreeDir: t.TempDir(), Prompt: "hi",
			PermissionMode: adapter.PermissionFull,
			AllowedTools:   []string{"shell"},
		})
		if !errors.Is(err, adapter.ErrNotSupported) {
			t.Fatalf("AllowedTools → %v, want ErrNotSupported; dropping it silently is the downgrade the contract forbids", err)
		}
	})

	t.Run("unknown required capability", func(t *testing.T) {
		_, err := a.Start(context.Background(), adapter.RunSpec{
			WorktreeDir: t.TempDir(), Prompt: "hi",
			PermissionMode:       adapter.PermissionFull,
			RequiredCapabilities: []string{"telepathy"},
		})
		if !errors.Is(err, adapter.ErrCapability) {
			t.Fatalf("unknown capability → %v, want ErrCapability", err)
		}
	})
}

func TestPreflightSaysWhatIsWrong(t *testing.T) {
	a := New()
	a.binary = "codex-that-is-not-installed"
	err := a.Preflight(context.Background())
	if !errors.Is(err, adapter.ErrPreflight) {
		t.Fatalf("Preflight → %v, want ErrPreflight", err)
	}
	if !strings.Contains(err.Error(), "codex-that-is-not-installed") {
		t.Errorf("the message does not name the binary: %v", err)
	}
	if !strings.Contains(err.Error(), "PATH") {
		t.Errorf("the message does not say what to do: %v", err)
	}
}

func TestMergeEnvOverrides(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2"}, map[string]string{"B": "3", "C": "4"})
	seen := map[string]string{}
	for _, kv := range got {
		i := strings.IndexByte(kv, '=')
		seen[kv[:i]] = kv[i+1:]
	}
	if seen["A"] != "1" || seen["B"] != "3" || seen["C"] != "4" {
		t.Errorf("mergeEnv = %v, want A=1 B=3 C=4", seen)
	}
	if len(got) != 3 {
		t.Errorf("mergeEnv kept a shadowed entry: %v", got)
	}
}

func TestOrDefault(t *testing.T) {
	if orDefault(0, time.Second) != time.Second {
		t.Error("zero should mean the default")
	}
	if orDefault(2*time.Second, time.Second) != 2*time.Second {
		t.Error("a set value should win")
	}
}

// A start-up that times out has to say why on its own: the message ends up as
// the whole of a `stuck` deliverable. Before ark:rein#38 it read "initialize:
// context deadline exceeded", and weeks of Codex re-indexing its history
// before it would answer looked exactly like a protocol break.
func TestStartupTimeoutSaysWhatHappened(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, stdinR) }()
	// Says one thing, then never answers — the shape of a Codex that is busy
	// before it will talk.
	go func() {
		_, _ = io.WriteString(stdoutW, `{"method":"remoteControl/status/changed","params":{"status":"disabled"}}`+"\n")
	}()
	done := make(chan struct{})
	var once sync.Once
	kill := func() {
		once.Do(func() {
			close(done)
			_ = stdoutW.Close()
			_ = stdinR.Close()
		})
	}
	t.Cleanup(kill)

	a := New()
	a.spawn = func(context.Context, adapter.RunSpec) (*process, error) {
		return &process{
			stdin: stdinW, stdout: stdoutR, stderrTail: newTail(4),
			wait:     func() error { <-done; return nil },
			kill:     kill,
			exitCode: func() int { return -1 },
		}, nil
	}

	_, err := a.Start(context.Background(), adapter.RunSpec{
		WorktreeDir: t.TempDir(), Prompt: "hi",
		PermissionMode: adapter.PermissionFull,
		Timeouts:       adapter.Timeouts{Startup: 300 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("Start succeeded against an app server that never answered")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the timeout is not in the error chain: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"did not answer initialize within the 300ms startup timeout", // which step, how long
		"remoteControl/status/changed",                               // what it said before stalling
		"app-server stderr: (nothing)",                               // and that stderr was empty
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the startup error does not say %q:\n%s", want, msg)
		}
	}
}
