package adapter_test

import (
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

// full is a manifest declaring everything, so a test can subtract exactly the
// thing it is asserting about.
func full() adapter.Manifest {
	caps := map[adapter.Capability]adapter.Support{}
	for _, c := range adapter.Capabilities() {
		caps[c] = adapter.SupportYes
	}
	return adapter.Manifest{
		Kind:         "test",
		Capabilities: caps,
		Platforms:    []adapter.Platform{adapter.AnyArch(runtime.GOOS)},
	}
}

func TestSatisfiesEmpty(t *testing.T) {
	if missing := full().Satisfies(nil); missing != nil {
		t.Fatalf("Satisfies(nil) = %v, want nil", missing)
	}
	if missing := full().Satisfies([]string{}); missing != nil {
		t.Fatalf("Satisfies([]) = %v, want nil", missing)
	}
}

func TestSatisfiesDeclaredYes(t *testing.T) {
	m := full()
	req := []string{"git", "worktree", "file_edit", "shell", "mcp", "resume", "structured_events", "approvals"}
	if missing := m.Satisfies(req); len(missing) != 0 {
		t.Fatalf("Satisfies(everything) = %v, want none missing", missing)
	}
}

// The rule Rein borrows from Loom: only "yes" satisfies. "partial" and "no"
// fail identically, and so does an omission.
func TestSatisfiesFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(m *adapter.Manifest)
	}{
		{"partial fails closed", func(m *adapter.Manifest) {
			m.Capabilities[adapter.CapShell] = adapter.SupportPartial
		}},
		{"no fails closed", func(m *adapter.Manifest) {
			m.Capabilities[adapter.CapShell] = adapter.SupportNo
		}},
		{"undeclared fails closed", func(m *adapter.Manifest) {
			delete(m.Capabilities, adapter.CapShell)
		}},
		{"a value outside the tri-state fails closed", func(m *adapter.Manifest) {
			m.Capabilities[adapter.CapShell] = adapter.Support("maybe")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := full()
			tc.set(&m)
			missing := m.Satisfies([]string{"git", "shell"})
			if !reflect.DeepEqual(missing, []string{"shell"}) {
				t.Fatalf("Satisfies = %v, want [shell]", missing)
			}
		})
	}
}

// A name this build has never heard of is a miss, not a pass. A typo in an Elk
// packet must stop the run, not satisfy nothing quietly.
func TestSatisfiesUnknownCapability(t *testing.T) {
	m := full()
	missing := m.Satisfies([]string{"git", "telepathy", "worktre"})
	want := []string{"telepathy", "worktre"}
	if !reflect.DeepEqual(missing, want) {
		t.Fatalf("Satisfies = %v, want %v", missing, want)
	}
}

// Even if an adapter declares an unknown name "yes", requiring it still misses:
// the vocabulary is the gate, not the map.
func TestSatisfiesUnknownDeclaredYesStillMisses(t *testing.T) {
	m := full()
	m.Capabilities[adapter.Capability("telepathy")] = adapter.SupportYes
	if missing := m.Satisfies([]string{"telepathy"}); !reflect.DeepEqual(missing, []string{"telepathy"}) {
		t.Fatalf("Satisfies = %v, want [telepathy]", missing)
	}
	// …and Validate rejects the declaration outright.
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted a manifest declaring an unknown capability")
	}
}

func TestSatisfiesPreservesOrderAndDedupes(t *testing.T) {
	m := adapter.Manifest{Kind: "test", Capabilities: map[adapter.Capability]adapter.Support{}}
	missing := m.Satisfies([]string{"shell", "git", "shell", "  ", "git", "mcp"})
	want := []string{"shell", "git", "mcp"}
	if !reflect.DeepEqual(missing, want) {
		t.Fatalf("Satisfies = %v, want %v — order of first appearance, deduped", missing, want)
	}
}

func TestSupportsAndHas(t *testing.T) {
	m := full()
	m.Capabilities[adapter.CapResume] = adapter.SupportPartial
	delete(m.Capabilities, adapter.CapMCP)

	if got := m.Supports(adapter.CapGit); got != adapter.SupportYes {
		t.Errorf("Supports(git) = %q", got)
	}
	if got := m.Supports(adapter.CapResume); got != adapter.SupportPartial {
		t.Errorf("Supports(resume) = %q", got)
	}
	// An undeclared capability reads as "no", not as a zero-value blank.
	if got := m.Supports(adapter.CapMCP); got != adapter.SupportNo {
		t.Errorf("Supports(mcp) = %q, want no", got)
	}
	if m.Has(adapter.CapResume) || m.Has(adapter.CapMCP) {
		t.Error("Has returned true for a non-yes capability")
	}
	if !m.Has(adapter.CapGit) {
		t.Error("Has(git) = false")
	}
}

func TestDeclared(t *testing.T) {
	m := adapter.Manifest{
		Kind: "test",
		Capabilities: map[adapter.Capability]adapter.Support{
			adapter.CapShell:    adapter.SupportYes,
			adapter.CapGit:      adapter.SupportYes,
			adapter.CapResume:   adapter.SupportPartial,
			adapter.CapMCP:      adapter.SupportNo,
			adapter.CapFileEdit: adapter.SupportYes,
		},
	}
	want := []string{"file_edit", "git", "shell"}
	if got := m.Declared(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Declared = %v, want %v — only yes, sorted", got, want)
	}
}

func TestPlatforms(t *testing.T) {
	m := adapter.Manifest{
		Kind:      "test",
		Platforms: []adapter.Platform{{OS: "darwin", Arch: "arm64"}, adapter.AnyArch("linux")},
	}
	for _, tc := range []struct {
		goos, goarch string
		want         bool
	}{
		{"darwin", "arm64", true},
		{"darwin", "amd64", false}, // pinned arch does not widen
		{"linux", "amd64", true},   // empty arch matches every arch
		{"linux", "arm64", true},
		{"windows", "amd64", false},
	} {
		if got := m.SupportsPlatform(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("SupportsPlatform(%s/%s) = %v, want %v", tc.goos, tc.goarch, got, tc.want)
		}
	}
	if (adapter.Platform{OS: "darwin", Arch: "arm64"}).String() != "darwin/arm64" {
		t.Error("Platform.String with an arch")
	}
	if adapter.AnyArch("linux").String() != "linux" {
		t.Error("Platform.String without an arch")
	}
	// No platforms means nowhere — fail closed here too.
	if (adapter.Manifest{Kind: "x"}).SupportsPlatform("linux", "amd64") {
		t.Error("an empty platform list matched a platform")
	}
}

func TestManifestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    adapter.Manifest
		want string
	}{
		{"valid", full(), ""},
		{"no kind", adapter.Manifest{Platforms: []adapter.Platform{adapter.AnyArch("linux")}}, "no kind"},
		{
			"kind with a slash",
			adapter.Manifest{Kind: "a/b", Platforms: []adapter.Platform{adapter.AnyArch("linux")}},
			"no spaces or slashes",
		},
		{
			"no platforms",
			adapter.Manifest{Kind: "test"},
			"declares no platforms",
		},
		{
			"platform with no OS",
			adapter.Manifest{Kind: "test", Platforms: []adapter.Platform{{Arch: "amd64"}}},
			"platform with no OS",
		},
		{
			"unknown capability name",
			adapter.Manifest{
				Kind:         "test",
				Capabilities: map[adapter.Capability]adapter.Support{"telepathy": adapter.SupportYes},
				Platforms:    []adapter.Platform{adapter.AnyArch("linux")},
			},
			"unknown capability",
		},
		{
			"support value outside the tri-state",
			adapter.Manifest{
				Kind:         "test",
				Capabilities: map[adapter.Capability]adapter.Support{adapter.CapGit: "sometimes"},
				Platforms:    []adapter.Platform{adapter.AnyArch("linux")},
			},
			"want yes, partial or no",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestSupportValid(t *testing.T) {
	for _, s := range []adapter.Support{adapter.SupportYes, adapter.SupportPartial, adapter.SupportNo} {
		if !s.Valid() {
			t.Errorf("%q.Valid() = false", s)
		}
	}
	for _, s := range []adapter.Support{"", "maybe", "YES"} {
		if adapter.Support(s).Valid() {
			t.Errorf("%q.Valid() = true", s)
		}
	}
}

func TestKnownCapability(t *testing.T) {
	if len(adapter.Capabilities()) != 9 {
		t.Fatalf("the vocabulary has %d entries; docs/adapters.md documents 9", len(adapter.Capabilities()))
	}
	for _, c := range adapter.Capabilities() {
		if !adapter.KnownCapability(c) {
			t.Errorf("KnownCapability(%q) = false", c)
		}
	}
	for _, c := range []string{"", "telepathy", "Git", "worktree "} {
		if adapter.KnownCapability(adapter.Capability(c)) {
			t.Errorf("KnownCapability(%q) = true", c)
		}
	}
	// Capabilities() must hand back a copy, not the package's own slice.
	got := adapter.Capabilities()
	got[0] = "clobbered"
	if adapter.Capabilities()[0] == "clobbered" {
		t.Fatal("Capabilities() exposed the package's own slice")
	}
}

func TestCheckSupport(t *testing.T) {
	base := adapter.RunSpec{
		RunID:          "run-1",
		WorktreeDir:    t.TempDir(),
		Prompt:         "do the thing",
		PermissionMode: adapter.PermissionFull,
	}

	t.Run("passes when everything is declared", func(t *testing.T) {
		spec := base
		spec.RequiredCapabilities = []string{"git", "file_edit"}
		if err := adapter.CheckSupport(full(), spec); err != nil {
			t.Fatalf("CheckSupport = %v, want nil", err)
		}
	})

	t.Run("refuses a missing required capability", func(t *testing.T) {
		m := full()
		m.Capabilities[adapter.CapShell] = adapter.SupportPartial
		spec := base
		spec.RequiredCapabilities = []string{"shell"}
		err := adapter.CheckSupport(m, spec)
		if !errors.Is(err, adapter.ErrCapability) {
			t.Fatalf("err = %v, want ErrCapability", err)
		}
		var ce *adapter.CapabilityError
		if !errors.As(err, &ce) || !reflect.DeepEqual(ce.Missing, []string{"shell"}) {
			t.Fatalf("err = %v, want a CapabilityError naming shell", err)
		}
		if !strings.Contains(err.Error(), "shell") {
			t.Errorf("the message does not name the capability: %v", err)
		}
	})

	t.Run("refuses an ask mode without approvals", func(t *testing.T) {
		m := full()
		m.Capabilities[adapter.CapApprovals] = adapter.SupportNo
		spec := base
		spec.PermissionMode = adapter.PermissionAsk
		err := adapter.CheckSupport(m, spec)
		if !errors.Is(err, adapter.ErrCapability) {
			t.Fatalf("err = %v, want ErrCapability", err)
		}
		if !strings.Contains(err.Error(), "permission mode") {
			t.Errorf("the message does not say why: %v", err)
		}
	})

	t.Run("refuses a resume without resume", func(t *testing.T) {
		m := full()
		m.Capabilities[adapter.CapResume] = adapter.SupportNo
		spec := base
		spec.ResumeID = "sess-1"
		if err := adapter.CheckSupport(m, spec); !errors.Is(err, adapter.ErrCapability) {
			t.Fatalf("err = %v, want ErrCapability", err)
		}
	})

	t.Run("refuses MCP servers without mcp", func(t *testing.T) {
		m := full()
		m.Capabilities[adapter.CapMCP] = adapter.SupportNo
		spec := base
		spec.MCPServers = map[string]any{"elk": map[string]any{"url": "https://example.test"}}
		if err := adapter.CheckSupport(m, spec); !errors.Is(err, adapter.ErrCapability) {
			t.Fatalf("err = %v, want ErrCapability", err)
		}
	})

	t.Run("refuses a platform it does not run on", func(t *testing.T) {
		m := full()
		m.Platforms = []adapter.Platform{{OS: "plan9"}}
		err := adapter.CheckSupport(m, base)
		if !errors.Is(err, adapter.ErrPlatform) {
			t.Fatalf("err = %v, want ErrPlatform", err)
		}
		if !strings.Contains(err.Error(), "plan9") {
			t.Errorf("the message does not name what is supported: %v", err)
		}
	})
}

func TestRunSpecValidate(t *testing.T) {
	ok := adapter.RunSpec{WorktreeDir: "/w", Prompt: "p", PermissionMode: adapter.PermissionFull}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
	for _, tc := range []struct {
		name string
		spec adapter.RunSpec
		want string
	}{
		{"no worktree", adapter.RunSpec{Prompt: "p", PermissionMode: adapter.PermissionFull}, "worktree"},
		{"no prompt and no resume", adapter.RunSpec{WorktreeDir: "/w", PermissionMode: adapter.PermissionFull}, "neither a prompt nor a resume"},
		{"unset permission mode", adapter.RunSpec{WorktreeDir: "/w", Prompt: "p"}, "permission mode"},
		{"bogus permission mode", adapter.RunSpec{WorktreeDir: "/w", Prompt: "p", PermissionMode: "yolo"}, "permission mode"},
		{"negative timeout", adapter.RunSpec{WorktreeDir: "/w", Prompt: "p", PermissionMode: adapter.PermissionFull,
			Timeouts: adapter.Timeouts{Idle: -1}}, "negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	// A resume with no prompt is legal: continuing a session needs no new text.
	resume := adapter.RunSpec{WorktreeDir: "/w", ResumeID: "s", PermissionMode: adapter.PermissionReadOnly}
	if err := resume.Validate(); err != nil {
		t.Fatalf("a resume-only spec was rejected: %v", err)
	}
}

func TestPermissionMode(t *testing.T) {
	for _, m := range []adapter.PermissionMode{
		adapter.PermissionReadOnly, adapter.PermissionAsk,
		adapter.PermissionAcceptEdits, adapter.PermissionFull,
	} {
		if !m.Valid() {
			t.Errorf("%q.Valid() = false", m)
		}
	}
	// The zero value must not be valid: `claude -p` hangs on its first edit in
	// Manual mode, so a spec that forgot to say is a bug, not a default.
	if adapter.PermissionUnset.Valid() {
		t.Error("PermissionUnset.Valid() = true")
	}
	if adapter.PermissionMode("full-auto").Valid() {
		t.Error("an undefined mode reported valid")
	}
	if !adapter.PermissionAsk.NeedsApprovals() || !adapter.PermissionAcceptEdits.NeedsApprovals() {
		t.Error("ask/accept_edits should need approvals")
	}
	if adapter.PermissionReadOnly.NeedsApprovals() || adapter.PermissionFull.NeedsApprovals() {
		t.Error("read_only/full should not need approvals")
	}
}
