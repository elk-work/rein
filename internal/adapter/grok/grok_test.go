package grok

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	got, err := adapter.Get(Kind)
	if err != nil {
		t.Fatalf("adapter.Get(%q): %v", Kind, err)
	}
	if got.Name() != Kind {
		t.Errorf("registry holds %q under %q", got.Name(), Kind)
	}

	for _, c := range adapter.Capabilities() {
		if _, declared := m.Capabilities[c]; !declared {
			t.Errorf("capability %q is undeclared; declare a value rather than leaving a gap", c)
		}
	}
	// The two honest admissions. If either is ever promoted to yes it should
	// be because a probe verified it, and this test is where that shows up.
	if m.Supports(adapter.CapApprovals) != adapter.SupportPartial {
		t.Errorf("approvals = %q; it is `partial` until a session/request_permission round trip is observed",
			m.Supports(adapter.CapApprovals))
	}
	if m.Supports(adapter.CapWorktree) != adapter.SupportPartial {
		t.Errorf("worktree = %q; it is `partial` until `full` runs under a sandbox too",
			m.Supports(adapter.CapWorktree))
	}
	if m.Notes == "" {
		t.Error("a manifest with `partial` declarations must say in Notes what is missing")
	}
}

// TestPartialApprovalsFailClosed is the point of the tri-state: `partial` and
// `no` are the same refusal, so a run that needs approvals never reaches an
// adapter that cannot serve them.
func TestPartialApprovalsFailClosed(t *testing.T) {
	m := New().Manifest()
	for _, mode := range []adapter.PermissionMode{adapter.PermissionAsk, adapter.PermissionAcceptEdits} {
		err := adapter.CheckSupport(m, adapter.RunSpec{
			WorktreeDir: "/tmp/x", Prompt: "hi", PermissionMode: mode,
		})
		if !errors.Is(err, adapter.ErrCapability) {
			t.Errorf("CheckSupport(%s) = %v, want ErrCapability", mode, err)
		}
	}
	missing := m.Satisfies([]string{"worktree", "shell"})
	if len(missing) != 1 || missing[0] != "worktree" {
		t.Errorf("Satisfies = %v, want worktree to be the only miss", missing)
	}
}

func TestPermissionSettings(t *testing.T) {
	ro, err := permissionSettings(adapter.PermissionReadOnly)
	if err != nil {
		t.Fatalf("read_only: %v", err)
	}
	if ro.sandboxProfile != sandboxReadOnly || !ro.yolo {
		t.Errorf("read_only → %+v, want the read-only sandbox with auto-approve inside it", ro)
	}
	full, err := permissionSettings(adapter.PermissionFull)
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if full.sandboxProfile != "" || !full.yolo {
		t.Errorf("full → %+v, want yolo and no sandbox", full)
	}
	for _, mode := range []adapter.PermissionMode{adapter.PermissionAsk, adapter.PermissionAcceptEdits} {
		if _, err := permissionSettings(mode); !errors.Is(err, adapter.ErrNotSupported) {
			t.Errorf("%s → %v, want ErrNotSupported", mode, err)
		}
	}
	if _, err := permissionSettings(adapter.PermissionUnset); err == nil {
		t.Error("the unset mode was mapped; it must be rejected, not guessed at")
	}
}

func TestSpecWithSandboxRespectsAnExplicitEnv(t *testing.T) {
	spec := adapter.RunSpec{Env: map[string]string{sandboxEnv: "strict", "OTHER": "1"}}
	got := specWithSandbox(spec, permissionMode{sandboxProfile: sandboxReadOnly})
	if got.Env[sandboxEnv] != "strict" {
		t.Errorf("%s = %q; an explicit profile in RunSpec.Env must win", sandboxEnv, got.Env[sandboxEnv])
	}
	if got.Env["OTHER"] != "1" {
		t.Errorf("the rest of the environment was lost: %v", got.Env)
	}

	empty := specWithSandbox(adapter.RunSpec{}, permissionMode{sandboxProfile: sandboxReadOnly})
	if empty.Env[sandboxEnv] != sandboxReadOnly {
		t.Errorf("%s = %q, want %q", sandboxEnv, empty.Env[sandboxEnv], sandboxReadOnly)
	}
	unchanged := specWithSandbox(adapter.RunSpec{Env: map[string]string{"A": "1"}}, permissionMode{})
	if _, set := unchanged.Env[sandboxEnv]; set {
		t.Error("a mode with no profile must not set the sandbox variable")
	}
}

func TestMCPServersRejectStdio(t *testing.T) {
	// `grok agent` reports mcpCapabilities http and sse and no stdio; a stdio
	// entry earns a bare "Invalid params" in the middle of the handshake,
	// which is a bad way to find out.
	_, err := mcpServers(map[string]any{
		"elk": map[string]any{"command": "npx", "args": []any{"-y", "elk-mcp"}},
	})
	if !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("stdio server → %v, want ErrNotSupported", err)
	}
	if !strings.Contains(err.Error(), "elk") {
		t.Errorf("the error does not name the server: %v", err)
	}

	got, err := mcpServers(map[string]any{
		"elk": map[string]any{"type": "http", "url": "https://example.invalid/mcp"},
	})
	if err != nil {
		t.Fatalf("http server: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d servers, want 1", len(got))
	}
	entry := got[0].(map[string]any)
	if entry["name"] != "elk" {
		t.Errorf("the map key must become the name: %v", entry)
	}
	if _, ok := entry["headers"]; !ok {
		t.Errorf("headers must be present; the agent's McpServer union requires it: %v", entry)
	}

	empty, err := mcpServers(nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("no servers should give an empty array, not nil: %v / %v", empty, err)
	}
}

func TestStartRefusesRatherThanDegrades(t *testing.T) {
	a := New()
	a.spawn = func(context.Context, adapter.RunSpec) (*process, error) {
		t.Fatal("Start spawned a process for a spec it should have refused")
		return nil, nil
	}

	cases := []struct {
		name string
		spec adapter.RunSpec
		want error
	}{
		{"no permission mode", adapter.RunSpec{WorktreeDir: "/tmp/x", Prompt: "hi"}, nil},
		{"no worktree", adapter.RunSpec{Prompt: "hi", PermissionMode: adapter.PermissionFull}, nil},
		{"ask needs approvals", adapter.RunSpec{WorktreeDir: "/tmp/x", Prompt: "hi",
			PermissionMode: adapter.PermissionAsk}, adapter.ErrCapability},
		{"tool allow list", adapter.RunSpec{WorktreeDir: "/tmp/x", Prompt: "hi",
			PermissionMode: adapter.PermissionFull, AllowedTools: []string{"bash"}}, adapter.ErrNotSupported},
		{"worktree is required", adapter.RunSpec{WorktreeDir: "/tmp/x", Prompt: "hi",
			PermissionMode: adapter.PermissionFull, RequiredCapabilities: []string{"worktree"}}, adapter.ErrCapability},
		{"stdio mcp", adapter.RunSpec{WorktreeDir: "/tmp/x", Prompt: "hi",
			PermissionMode: adapter.PermissionFull,
			MCPServers:     map[string]any{"x": map[string]any{"command": "npx"}}}, adapter.ErrNotSupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := a.Start(context.Background(), c.spec)
			if err == nil {
				t.Fatalf("the spec was accepted")
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestPreflightSaysWhatIsWrong(t *testing.T) {
	a := New()
	a.binary = "grok-that-is-not-installed"
	err := a.Preflight(context.Background())
	if !errors.Is(err, adapter.ErrPreflight) {
		t.Fatalf("Preflight → %v, want ErrPreflight", err)
	}
	if !strings.Contains(err.Error(), "grok-that-is-not-installed") {
		t.Errorf("the message does not name the binary: %v", err)
	}
	if !strings.Contains(err.Error(), "PATH") {
		t.Errorf("the message does not say what to do: %v", err)
	}
}

func TestSandboxFailedReadsTheWarning(t *testing.T) {
	tl := newTail(4)
	if _, bad := tl.sandboxFailed(); bad {
		t.Error("an empty tail reported a sandbox failure")
	}
	tl.lines = []string{"some unrelated line",
		"warning: sandbox could not be applied: hook source path contains a symlink component"}
	line, bad := tl.sandboxFailed()
	if !bad {
		t.Fatal("the warning was not recognised")
	}
	if !strings.Contains(line, "symlink") {
		t.Errorf("the offending line was not returned: %q", line)
	}
	tl2 := newTail(4)
	tl2.lines = []string{"warning: continuing without enforcement of the sandbox"}
	if _, bad := tl2.sandboxFailed(); !bad {
		t.Error("the `without enforcement` wording was not recognised")
	}
}

func TestPickOption(t *testing.T) {
	opts := []permissionOption{
		{OptionID: "r", Kind: optRejectOnce},
		{OptionID: "a", Kind: optAllowOnce},
	}
	got, ok := pickOption(opts, optAllowOnce, optAllowAlways)
	if !ok || got.OptionID != "a" {
		t.Errorf("pickOption allow = %+v/%v", got, ok)
	}
	got, ok = pickOption(opts, optRejectOnce, optRejectAlways)
	if !ok || got.OptionID != "r" {
		t.Errorf("pickOption reject = %+v/%v", got, ok)
	}
	if _, ok := pickOption(nil, optAllowOnce); ok {
		t.Error("an empty option list produced a choice")
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
