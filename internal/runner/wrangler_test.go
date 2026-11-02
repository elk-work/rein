package runner

import (
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
)

func TestWranglerQueuePrompt(t *testing.T) {
	if runSystemPrompt(config.Queue{}, false) != SystemPrompt {
		t.Fatal("ordinary prompt changed")
	}
	// The old spelling, `pm = true`, renders the identical prompt.
	if runSystemPrompt(config.Queue{PM: true}, true) != runSystemPrompt(config.Queue{Wrangler: true}, true) {
		t.Fatal("pm = true and wrangler = true render different prompts")
	}
	p := runSystemPrompt(config.Queue{Wrangler: true}, true)
	for _, want := range []string{"This run is a Wrangler cycle", "Follow the playbook in this run's work order", "Never force-push or rewrite history", "Never send mail or messages", "Never install\nor run unfamiliar scripts", "-- HOLD:", "Hold anything gated", "Never capture the screen"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, bad := range []string{"rewrite history, touch production", "history, touching production", "docs/pm-driver.md", "PM queue", "PM loop", "PM rule", "supabase/README.md", "deploy-functions.sh", "Signal", "elk-work/website", "docs/wrangler.md", "elk-pm"} {
		if strings.Contains(p, bad) {
			t.Errorf("Wrangler prompt still says %q", bad)
		}
	}
}

// TestOnlyAWranglerCycleGetsTheWranglerRule is ark:rein#50: the rule is
// granted per run, to a packet that requires `pm` on a queue that opted in,
// and to nothing else.
func TestOnlyAWranglerCycleGetsTheWranglerRule(t *testing.T) {
	// Any other run on a Wrangler queue gets the prompt a queue without the
	// opt-in gives it, byte for byte, under either spelling.
	for _, q := range []config.Queue{{Wrangler: true}, {PM: true}} {
		if runSystemPrompt(q, false) != SystemPrompt {
			t.Errorf("%+v: a run that is not a Wrangler cycle did not get the ordinary prompt", q)
		}
	}
	for _, land := range []string{config.LandPR, config.LandBranch} {
		if runSystemPrompt(config.Queue{Wrangler: true, Land: land}, false) != runSystemPrompt(config.Queue{Land: land}, false) {
			t.Errorf("land %s: an ordinary run on a Wrangler queue got a different prompt from a plain queue", land)
		}
	}
	// A cycle flag without the opt-in grants nothing.
	if runSystemPrompt(config.Queue{}, true) != SystemPrompt {
		t.Error("the Wrangler rule was spliced into a queue that never opted in")
	}
}

func TestWranglerCycleNeedsTheOptInAndAPMRequirement(t *testing.T) {
	wrangler, pm, plain := config.Queue{Wrangler: true}, config.Queue{PM: true}, config.Queue{}
	for _, tc := range []struct {
		name     string
		q        config.Queue
		required []string
		want     bool
	}{
		{"cycle on a wrangler queue", wrangler, []string{"pm"}, true},
		{"cycle under the old spelling", pm, []string{"pm"}, true},
		{"cycle among other requirements", wrangler, []string{"git", " pm ", "github-cli"}, true},
		{"build run on a wrangler queue", wrangler, nil, false},
		{"build run needing the elk connector", wrangler, []string{"mcp:elk", "git"}, false},
		{"wrangler is not the wire name", wrangler, []string{"wrangler"}, false},
		{"pm packet on a plain queue", plain, []string{"pm"}, false},
		{"build run on a plain queue", plain, []string{"git"}, false},
	} {
		if got := wranglerCycle(tc.q, tc.required); got != tc.want {
			t.Errorf("%s: wranglerCycle = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ark:rein#68: a scoped Wrangler cycle gets the Wrangler rule and the scoped
// secrets paragraph; a scoped run that is not a cycle gets only the paragraph.
func TestAScopedWranglerCycleGetsTheRuleAndTheScopedParagraph(t *testing.T) {
	q := config.Queue{Wrangler: true, AgentKind: "claude", ScopedSecrets: true, Secrets: map[string]string{"POSTHOG_API_KEY": "acme/posthog"}}
	cycle := runSystemPrompt(q, true)
	for _, want := range []string{"This run is a Wrangler cycle", "Follow the playbook in this run's work order", "This queue's credentials are scoped", "`POSTHOG_API_KEY`"} {
		if !strings.Contains(cycle, want) {
			t.Errorf("scoped Wrangler cycle is missing %q", want)
		}
	}
	if strings.Contains(cycle, vaultParagraphStart) {
		t.Error("scoped Wrangler cycle still carries the vault paragraph")
	}
	plain := runSystemPrompt(q, false)
	if strings.Contains(plain, "This run is a Wrangler cycle") {
		t.Error("a scoped run that is not a cycle got the Wrangler rule")
	}
	if !strings.Contains(plain, "This queue's credentials are scoped") || strings.Contains(plain, vaultParagraphStart) {
		t.Error("a scoped run that is not a cycle lost its scoped paragraph")
	}
}

// The production rule: a Wrangler cycle is told to ask Elk's door before any
// production step, and an ordinary run on the same queue is not. Scoped
// Wrangler cycles get it too.
func TestWranglerCycleSendsProductionStepsToTheDoor(t *testing.T) {
	q := config.Queue{Wrangler: true, Production: true}
	for _, want := range []string{"wrangler_production_step", "Before any production step", "treat the workspace as Hold"} {
		if !strings.Contains(runSystemPrompt(q, true), want) {
			t.Errorf("Wrangler cycle prompt is missing %q", want)
		}
	}
	if strings.Contains(runSystemPrompt(q, false), "wrangler_production_step") {
		t.Error("an ordinary run on a Wrangler queue was told about the production door")
	}
	scoped := config.Queue{Wrangler: true, AgentKind: "claude", ScopedSecrets: true, Secrets: map[string]string{"POSTHOG_API_KEY": "acme/posthog"}}
	p := runSystemPrompt(scoped, true)
	if !strings.Contains(p, "wrangler_production_step") || !strings.Contains(p, "This queue's credentials are scoped") {
		t.Error("a scoped Wrangler cycle must get the production rule and the scoped paragraph")
	}
}
