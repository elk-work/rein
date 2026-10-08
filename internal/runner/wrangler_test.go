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
	for _, want := range []string{"This run is a Wrangler cycle", "Wrangler queue is authorized", "supabase/README.md rule 5", "verify by object", "scripts/deploy-functions.sh", "between refresh passes", "-- HOLD:", "Never merge anything in elk-work/website", "docs/wrangler.md", "Never capture the screen"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, bad := range []string{"rewrite history, touch production", "history, touching production", "docs/pm-driver.md", "PM queue", "PM loop", "PM rule"} {
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
