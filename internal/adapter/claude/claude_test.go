package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

func TestRegisteredUnderItsKind(t *testing.T) {
	a, err := adapter.Get(Kind)
	if err != nil {
		t.Fatalf("importing the package must register it: %v", err)
	}
	if a.Name() != Kind {
		t.Errorf("Name() = %q, want %q", a.Name(), Kind)
	}
	if a.Manifest().Kind != a.Name() {
		t.Errorf("manifest kind %q disagrees with Name() %q", a.Manifest().Kind, a.Name())
	}
}

func TestManifestIsValidAndHonest(t *testing.T) {
	m := New().Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest does not validate: %v", err)
	}

	// Every capability in the vocabulary must be declared, including the ones
	// declared partial: an omission and a refusal fail identically, but only
	// one of them reads as a decision.
	for _, c := range adapter.Capabilities() {
		if _, ok := m.Capabilities[c]; !ok {
			t.Errorf("capability %q is undeclared; declare it explicitly", c)
		}
	}

	if got := m.Supports(adapter.CapApprovals); got != adapter.SupportPartial {
		t.Errorf("approvals = %q, want partial: `claude -p` auto-denies and cannot be answered", got)
	}
	if got := m.Supports(adapter.CapWorktree); got != adapter.SupportPartial {
		t.Errorf("worktree = %q, want partial: under full, bypassPermissions writes outside the worktree (ark:rein#12)", got)
	}
	if missing := m.Satisfies([]string{"worktree"}); len(missing) != 1 || missing[0] != "worktree" {
		t.Errorf("a packet requiring worktree must fail closed, missing = %v", missing)
	}
	for _, c := range []adapter.Capability{
		adapter.CapGit, adapter.CapFileEdit,
		adapter.CapShell, adapter.CapMCP, adapter.CapResume, adapter.CapStructuredEvents,
	} {
		if !m.Has(c) {
			t.Errorf("capability %q should be yes", c)
		}
	}

	for _, goos := range []string{"darwin", "linux", "windows"} {
		if !m.SupportsPlatform(goos, "amd64") {
			t.Errorf("platform %s should be supported", goos)
		}
	}
	if m.Notes == "" {
		t.Error("a partial capability needs a note saying what is missing")
	}
}

// TestApprovalsPartialClosesTwoModes is the consequence the run loop has to
// plan around: because approvals is partial, the shared fail-closed gate
// refuses `ask` AND `accept_edits`, leaving read_only and full.
func TestApprovalsPartialClosesTwoModes(t *testing.T) {
	m := New().Manifest()
	base := adapter.RunSpec{WorktreeDir: t.TempDir(), Prompt: "go"}

	for _, mode := range []adapter.PermissionMode{adapter.PermissionAsk, adapter.PermissionAcceptEdits} {
		spec := base
		spec.PermissionMode = mode
		err := adapter.CheckSupport(m, spec)
		if err == nil {
			t.Errorf("mode %q was accepted; approvals=partial must refuse it", mode)
			continue
		}
		if !errors.Is(err, adapter.ErrCapability) {
			t.Errorf("mode %q: error = %v, want ErrCapability", mode, err)
		}
	}

	for _, mode := range []adapter.PermissionMode{adapter.PermissionReadOnly, adapter.PermissionFull} {
		spec := base
		spec.PermissionMode = mode
		if err := adapter.CheckSupport(m, spec); err != nil {
			t.Errorf("mode %q should be serviceable, got %v", mode, err)
		}
	}
}

func TestPermissionFlagMapping(t *testing.T) {
	cases := map[adapter.PermissionMode]string{
		adapter.PermissionReadOnly:    "plan",
		adapter.PermissionAcceptEdits: "acceptEdits",
		adapter.PermissionAsk:         "manual",
		adapter.PermissionFull:        "bypassPermissions",
	}
	for mode, want := range cases {
		got, err := permissionFlag(mode)
		if err != nil {
			t.Errorf("mode %q: %v", mode, err)
			continue
		}
		if got != want {
			t.Errorf("mode %q mapped to %q, want %q", mode, got, want)
		}
	}

	// The zero value has no mapping. `claude -p` picks its own mode when not
	// told, so guessing here is exactly the silent downgrade the contract
	// exists to prevent.
	if _, err := permissionFlag(adapter.PermissionUnset); !errors.Is(err, adapter.ErrNotSupported) {
		t.Errorf("PermissionUnset: err = %v, want ErrNotSupported", err)
	}
}

func testSession(t *testing.T) *Session {
	t.Helper()
	return &Session{tmpDir: t.TempDir(), hookEvents: make(chan hookPayload, 4)}
}

func TestBuildArgsCoreCommandLine(t *testing.T) {
	a := &Adapter{DisableWatchdog: true}
	spec := adapter.RunSpec{
		WorktreeDir:    t.TempDir(),
		Prompt:         "do the thing",
		PermissionMode: adapter.PermissionFull,
	}
	args, err := a.buildArgs(spec, "bypassPermissions", testSession(t))
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}

	for _, want := range [][]string{
		{"-p"},
		{"--output-format", "stream-json"},
		{"--input-format", "stream-json"},
		{"--verbose"},
		{"--permission-mode", "bypassPermissions"},
	} {
		if !containsSeq(args, want) {
			t.Errorf("argv missing %v\ngot: %v", want, args)
		}
	}

	// --bare disables subscription auth. It must never appear.
	if slices.Contains(args, "--bare") {
		t.Error("argv contains --bare, which disables subscription auth")
	}
	// The prompt travels on stdin, not argv: that is what keeps Send possible.
	if slices.Contains(args, spec.Prompt) {
		t.Error("the prompt was put on argv; it must go over stdin as stream-json")
	}
}

func TestBuildArgsOptionalFlags(t *testing.T) {
	a := &Adapter{DisableWatchdog: true}
	spec := adapter.RunSpec{
		WorktreeDir:    t.TempDir(),
		Prompt:         "go",
		PermissionMode: adapter.PermissionReadOnly,
		Model:          "claude-fable-5",
		SystemPrompt:   "you are a runner",
		AllowedTools:   []string{"Read", "Bash(git *)"},
		DeniedTools:    []string{"WebFetch"},
		ResumeID:       "9f0c2e1a-0000-4000-8000-000000000000",
	}
	args, err := a.buildArgs(spec, "plan", testSession(t))
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}

	for _, want := range [][]string{
		{"--model", "claude-fable-5"},
		{"--append-system-prompt", "you are a runner"},
		{"--allowedTools", "Read", "Bash(git *)"},
		{"--disallowedTools", "WebFetch"},
		{"--resume", spec.ResumeID},
	} {
		if !containsSeq(args, want) {
			t.Errorf("argv missing %v\ngot: %v", want, args)
		}
	}
}

func TestBuildArgsMCPIsStrict(t *testing.T) {
	a := &Adapter{DisableWatchdog: true}
	spec := adapter.RunSpec{
		WorktreeDir:    t.TempDir(),
		Prompt:         "go",
		PermissionMode: adapter.PermissionFull,
		MCPServers: map[string]any{
			"elk": map[string]any{"command": "elk-mcp", "args": []string{"serve"}},
		},
	}
	s := testSession(t)
	args, err := a.buildArgs(spec, "bypassPermissions", s)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}

	if !slices.Contains(args, "--mcp-config") {
		t.Fatalf("argv has no --mcp-config: %v", args)
	}
	// Without --strict-mcp-config the run would also load whatever MCP servers
	// happen to be configured on that laptop, which is not what the packet
	// asked for.
	if !slices.Contains(args, "--strict-mcp-config") {
		t.Error("argv has --mcp-config without --strict-mcp-config")
	}

	i := slices.Index(args, "--mcp-config")
	blob := readFile(t, args[i+1])
	if !strings.Contains(blob, `"mcpServers"`) || !strings.Contains(blob, "elk-mcp") {
		t.Errorf("the MCP config file does not carry the packet's servers: %s", blob)
	}
}

// TestBuildArgsWithoutMCPIsStillIsolated is the regression test for
// ark:rein#21. This test previously asserted the opposite — that a spec with no
// MCP servers "must not silence the developer's own config" — and that reading
// was the bug: it let an ordinary run inherit ~/.claude.json, so the agent came
// up holding the Elk connector logged in as the person who queued the work.
// "No servers" is an empty set, not an absent opinion.
func TestBuildArgsWithoutMCPIsStillIsolated(t *testing.T) {
	a := &Adapter{DisableWatchdog: true}
	spec := adapter.RunSpec{
		WorktreeDir:    t.TempDir(),
		Prompt:         "go",
		PermissionMode: adapter.PermissionFull,
	}
	s := testSession(t)
	args, err := a.buildArgs(spec, "bypassPermissions", s)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}

	if !slices.Contains(args, "--strict-mcp-config") {
		t.Fatalf("a spec with no MCP servers must still be isolated: %v", args)
	}
	i := slices.Index(args, "--mcp-config")
	if i < 0 {
		t.Fatalf("--strict-mcp-config without --mcp-config gives the agent nothing to be strict about: %v", args)
	}

	// The empty set has to be written out explicitly, because
	// --strict-mcp-config alone is not what excludes the user's servers.
	var cfg struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	blob := readFile(t, args[i+1])
	if err := json.Unmarshal([]byte(blob), &cfg); err != nil {
		t.Fatalf("the MCP config is not valid JSON: %v", err)
	}
	if cfg.MCPServers == nil {
		t.Errorf("the MCP config has no mcpServers key at all: %s", blob)
	}
	if len(cfg.MCPServers) != 0 {
		t.Errorf("the MCP config is not empty: %s", blob)
	}
}

// TestBuildArgsCutsTheDevelopersSettings covers the other half of ark:rein#21:
// --strict-mcp-config stops MCP servers leaking, and nothing else. The
// developer's hooks, plugins and skills arrive through the settings sources.
func TestBuildArgsCutsTheDevelopersSettings(t *testing.T) {
	spec := adapter.RunSpec{
		WorktreeDir:    t.TempDir(),
		Prompt:         "go",
		PermissionMode: adapter.PermissionFull,
	}

	cases := []struct {
		name    string
		adapter *Adapter
		want    string
	}{{
		name:    "by default the repository's own settings are kept",
		adapter: &Adapter{DisableWatchdog: true},
		want:    "project",
	}, {
		name:    "and a deployment can drop those too",
		adapter: &Adapter{DisableWatchdog: true, SettingSources: NoSettingSources},
		want:    "",
	}, {
		name:    "an explicit policy is passed through",
		adapter: &Adapter{DisableWatchdog: true, SettingSources: "project,local"},
		want:    "project,local",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := tc.adapter.buildArgs(spec, "bypassPermissions", testSession(t))
			if err != nil {
				t.Fatalf("buildArgs: %v", err)
			}
			i := slices.Index(args, "--setting-sources")
			if i < 0 {
				t.Fatalf("argv never limits the setting sources: %v", args)
			}
			if got := args[i+1]; got != tc.want {
				t.Errorf("--setting-sources = %q, want %q", got, tc.want)
			}
			// `user` is the developer's own ~/.claude — the one source that
			// must never come back by default.
			if tc.adapter.SettingSources == "" && strings.Contains(args[i+1], "user") {
				t.Error("the default loads the developer's own user settings")
			}
		})
	}
}

func TestStartRefusesBeforeSpawning(t *testing.T) {
	a := &Adapter{DisableWatchdog: true}
	dir := t.TempDir()

	t.Run("unset permission mode", func(t *testing.T) {
		_, err := a.Start(context.Background(), adapter.RunSpec{WorktreeDir: dir, Prompt: "go"})
		if err == nil {
			t.Fatal("a spec with no permission mode was accepted")
		}
	})

	t.Run("mode needing approvals", func(t *testing.T) {
		_, err := a.Start(context.Background(), adapter.RunSpec{
			WorktreeDir: dir, Prompt: "go", PermissionMode: adapter.PermissionAsk,
		})
		if !errors.Is(err, adapter.ErrCapability) {
			t.Fatalf("err = %v, want ErrCapability", err)
		}
	})

	t.Run("required capability we do not have", func(t *testing.T) {
		_, err := a.Start(context.Background(), adapter.RunSpec{
			WorktreeDir: dir, Prompt: "go", PermissionMode: adapter.PermissionFull,
			RequiredCapabilities: []string{"approvals"},
		})
		if !errors.Is(err, adapter.ErrCapability) {
			t.Fatalf("err = %v, want ErrCapability", err)
		}
	})

	t.Run("unknown required capability", func(t *testing.T) {
		_, err := a.Start(context.Background(), adapter.RunSpec{
			WorktreeDir: dir, Prompt: "go", PermissionMode: adapter.PermissionFull,
			RequiredCapabilities: []string{"telepathy"},
		})
		if !errors.Is(err, adapter.ErrCapability) {
			t.Fatalf("a name outside the vocabulary must fail closed; err = %v", err)
		}
	})

	// Setting an API key would move the session off the developer's
	// subscription and onto metered billing — a change of auth model, which is
	// exactly the silent downgrade the contract forbids.
	t.Run("an API key in Env", func(t *testing.T) {
		_, err := a.Start(context.Background(), adapter.RunSpec{
			WorktreeDir: dir, Prompt: "go", PermissionMode: adapter.PermissionFull,
			Env: map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
		})
		if !errors.Is(err, adapter.ErrNotPlanAuth) {
			t.Fatalf("err = %v, want ErrNotPlanAuth", err)
		}
	})

	// The same keys reaching it through the repository's own settings.
	for name, settings := range map[string]string{
		"an apiKeyHelper in project settings": `{"apiKeyHelper": "/usr/local/bin/print-key"}`,
		"an auth token in the settings env":   `{"env": {"ANTHROPIC_AUTH_TOKEN": "x", "FOO": "bar"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			wt := t.TempDir()
			if err := os.MkdirAll(filepath.Join(wt, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, ".claude", "settings.json"), []byte(settings), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := a.Start(context.Background(), adapter.RunSpec{
				WorktreeDir: wt, Prompt: "go", PermissionMode: adapter.PermissionFull,
			})
			if !errors.Is(err, adapter.ErrNotPlanAuth) || !strings.Contains(err.Error(), ".claude/settings.json") {
				t.Fatalf("err = %v, want ErrNotPlanAuth naming the settings file", err)
			}
		})
	}

	// Local settings are not loaded by default, so they are not checked.
	t.Run("an apiKeyHelper in unloaded local settings", func(t *testing.T) {
		wt := t.TempDir()
		_ = os.MkdirAll(filepath.Join(wt, ".claude"), 0o755)
		_ = os.WriteFile(filepath.Join(wt, ".claude", "settings.local.json"), []byte(`{"apiKeyHelper":"x"}`), 0o600)
		if err := checkSettingsAuth(wt, DefaultSettingSources); err != nil {
			t.Errorf("refused over a file the session does not load: %v", err)
		}
		if err := checkSettingsAuth(wt, "project,local"); !errors.Is(err, adapter.ErrNotPlanAuth) {
			t.Errorf("did not refuse a loaded local file: %v", err)
		}
	})
}

func TestBuildArgsPassesEffort(t *testing.T) {
	a := &Adapter{DisableWatchdog: true}
	spec := adapter.RunSpec{WorktreeDir: t.TempDir(), Prompt: "go", PermissionMode: adapter.PermissionFull,
		Model: "claude-opus-5-5", Effort: "xhigh"}
	args, err := a.buildArgs(spec, "bypassPermissions", testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	if !containsSeq(args, []string{"--effort", "xhigh"}) || !containsSeq(args, []string{"--model", "claude-opus-5-5"}) {
		t.Errorf("argv missing the queue's model or effort: %v", args)
	}
	spec.Model, spec.Effort = "", ""
	args, _ = a.buildArgs(spec, "bypassPermissions", testSession(t))
	for _, f := range args {
		if f == "--effort" || f == "--model" {
			t.Errorf("an unset model/effort still passed %s; the CLI default should apply", f)
		}
	}
}

func TestRespondIsRefused(t *testing.T) {
	s := &Session{}
	err := s.Respond(context.Background(), adapter.PermissionResponse{ID: "x", Allow: true})
	if !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("Respond err = %v, want ErrNotSupported", err)
	}
}

func TestChildEnvOverridesInherited(t *testing.T) {
	t.Setenv("REIN_TEST_KEY", "inherited")

	env := childEnv(os.Environ(), map[string]string{"REIN_TEST_KEY": "override", "REIN_TEST_NEW": "added"})

	var seen int
	for _, kv := range env {
		if strings.HasPrefix(kv, "REIN_TEST_KEY=") {
			seen++
			if kv != "REIN_TEST_KEY=override" {
				t.Errorf("inherited value survived: %q", kv)
			}
		}
	}
	if seen != 1 {
		t.Errorf("REIN_TEST_KEY appears %d times, want exactly 1", seen)
	}
	if !slices.Contains(env, "REIN_TEST_NEW=added") {
		t.Error("a new key was not added to the child environment")
	}
}

// containsSeq reports whether want appears as a contiguous run in args.
func containsSeq(args, want []string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		if slices.Equal(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}
