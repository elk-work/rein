package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runner"
)

// withholdManifest is a minimal adapter declaration for the declared list.
var withholdManifest = adapter.Manifest{
	Kind:         "claude",
	Capabilities: map[adapter.Capability]adapter.Support{adapter.CapGit: adapter.SupportYes},
}

// assertWithheld checks a withheld name from both sides Elk sees: it is not
// declared, and a packet requiring it is refused here.
func assertWithheld(t *testing.T, h *runner.HostCapabilities, name string) {
	t.Helper()
	if h.Has(name) {
		t.Errorf("%s is withheld but still held: %v", name, h.Names())
	}
	for _, d := range runner.DeclaredCapabilities(withholdManifest, h) {
		if d == name || d == "tool:"+name {
			t.Errorf("%s is withheld but declared to Elk as %q", name, d)
		}
	}
	for _, req := range []string{name, "tool:" + name} {
		missing := runner.MissingQueueRequirements([]string{req}, withholdManifest, h, nil)
		if strings.Join(missing, ",") != req {
			t.Errorf("a packet requiring %s matched a host that withholds it (missing = %v)", req, missing)
		}
	}
}

func TestWithholdCapabilitiesMachineWide(t *testing.T) {
	t.Setenv(runner.EnvCapabilities, "github-cli, xcode")
	cfg := config.Config{
		// Listing a name and withholding it: the withholding wins.
		Capabilities:         []string{"xcode"},
		WithholdCapabilities: []string{"xcode"},
	}
	h := runner.DetectHostCapabilities(context.Background(), cfg)
	assertWithheld(t, h, "xcode")
	if !h.Has("github-cli") {
		t.Errorf("withholding xcode lost github-cli: %v", h.Names())
	}
	// Every queue inherits the machine's ruling, even one that lists xcode.
	q := config.Queue{Name: "mac-claude", AgentKind: "claude", Capabilities: []string{"xcode"}}
	assertWithheld(t, h.ForQueue(cfg, q), "xcode")
}

func TestWithholdCapabilitiesPerQueue(t *testing.T) {
	t.Setenv(runner.EnvCapabilities, "github-cli, xcode")
	cfg := config.Config{Queues: []config.Queue{
		{Name: "no-apps", AgentKind: "claude", WithholdCapabilities: []string{"xcode"}},
		{Name: "apps", AgentKind: "claude"},
	}}
	h := runner.DetectHostCapabilities(context.Background(), cfg)
	if !h.Has("xcode") {
		t.Fatalf("the machine should still hold xcode: %v", h.Names())
	}
	assertWithheld(t, h.ForQueue(cfg, cfg.Queues[0]), "xcode")

	other := h.ForQueue(cfg, cfg.Queues[1])
	if !other.Has("xcode") {
		t.Errorf("one queue's withholding leaked into another: %v", other.Names())
	}
	if missing := runner.MissingQueueRequirements([]string{"xcode"}, withholdManifest, other, nil); len(missing) != 0 {
		t.Errorf("the queue that does not withhold xcode refused it: %v", missing)
	}
}

func TestWithholdingANameTheHostDoesNotHoldIsHarmless(t *testing.T) {
	t.Setenv(runner.EnvCapabilities, "github-cli")
	cfg := config.Config{WithholdCapabilities: []string{"xcode"}}
	h := runner.DetectHostCapabilities(context.Background(), cfg)

	want := "elk-connector,git,github-cli,worktree"
	if got := strings.Join(h.Names(), ","); got != want {
		t.Errorf("capabilities = %q, want %q", got, want)
	}
	assertWithheld(t, h, "xcode")
}
