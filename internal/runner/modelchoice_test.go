package runner_test

import (
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// beatWith makes the harness's heartbeat carry structured content, the way an
// Elk that lets a person set a queue's model and effort answers.
func (h *harness) beatWith(structured map[string]any) {
	h.elk.Reply("heartbeat_executor", elktest.Reply{
		Text: `Heartbeat recorded for queue "` + queue + `". You are online for the next 15 minutes. 1 run waiting — ` +
			"claim with `claim_run(queue: \"" + queue + "\")`.",
		Structured: structured,
	})
}

func TestTheModelAndEffortSetInElkWinOverConfig(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Model = "claude-sonnet-5-5"
	h.cfg.Queues[0].Effort = "medium"
	h.beatWith(map[string]any{"queue": queue, "waiting": 1, "model": "claude-opus-5-5", "effort": "high"})
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spec := h.agent.Specs()[0]
	if spec.Model != "claude-opus-5-5" || spec.Effort != "high" {
		t.Errorf("the run got model %q effort %q; Elk's choice should win", spec.Model, spec.Effort)
	}
	log := h.log.String()
	if !strings.Contains(log, "model claude-sonnet-5-5 (config.toml), effort medium (config.toml)") {
		t.Errorf("start-up does not say what config.toml chose:\n%s", log)
	}
	want := "model and effort for the next run changed: model claude-opus-5-5 (Elk), effort high (Elk)"
	if strings.Count(log, want) != 1 {
		t.Errorf("want the change logged exactly once, naming Elk as the source:\n%s", log)
	}
}

func TestConfigFillsWhatElkLeavesUnset(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Model = "claude-sonnet-5-5"
	h.cfg.Queues[0].Effort = "medium"
	// Elk sets the effort only; the model stays the config's.
	h.beatWith(map[string]any{"queue": queue, "waiting": 1, "model": nil, "effort": "max"})
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spec := h.agent.Specs()[0]
	if spec.Model != "claude-sonnet-5-5" || spec.Effort != "max" {
		t.Errorf("model %q effort %q; want config's model and Elk's effort", spec.Model, spec.Effort)
	}
	if !strings.Contains(h.log.String(), "model claude-sonnet-5-5 (config.toml), effort max (Elk)") {
		t.Errorf("the change line does not name each source:\n%s", h.log)
	}
}

// TestAnElkWithNoStructuredContentChangesNothing is the compatibility case:
// an Elk that has not deployed the structured reply behaves exactly as Rein
// did before it could set a model.
func TestAnElkWithNoStructuredContentChangesNothing(t *testing.T) {
	h := newHarness(t) // the harness's heartbeat is text only
	h.cfg.Queues[0].Model = "claude-sonnet-5-5"
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spec := h.agent.Specs()[0]
	if spec.Model != "claude-sonnet-5-5" || spec.Effort != "" {
		t.Errorf("model %q effort %q; want config.toml's model and the CLI's effort", spec.Model, spec.Effort)
	}
	log := h.log.String()
	if strings.Contains(log, "changed:") || strings.Contains(log, "(Elk)") {
		t.Errorf("nothing changed, and the log says otherwise:\n%s", log)
	}
	if !strings.Contains(log, "model claude-sonnet-5-5 (config.toml), effort the CLI default") {
		t.Errorf("start-up does not say what applies:\n%s", log)
	}
}

func TestAMalformedChoiceFromElkIsRefusedAndConfigApplies(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Model = "claude-sonnet-5-5"
	h.cfg.Queues[0].Effort = "medium"
	h.beatWith(map[string]any{"queue": queue, "waiting": 1, "model": "not a model; rm -rf", "effort": "High Effort"})
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spec := h.agent.Specs()[0]
	if spec.Model != "claude-sonnet-5-5" || spec.Effort != "medium" {
		t.Errorf("model %q effort %q; a value that fails the shape check must not reach the CLI", spec.Model, spec.Effort)
	}
	log := h.log.String()
	for _, want := range []string{"not using what Elk set for this queue", `model "not a model; rm -rf"`, `effort "High Effort"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not say %q:\n%s", want, log)
		}
	}
}

func TestResolveModelPrecedence(t *testing.T) {
	q := config.Queue{Model: "cfg-model", Effort: "low"}
	for _, tc := range []struct {
		name                string
		q                   config.Queue
		elkModel, elkEffort string
		model, from         string
		effort, effortFrom  string
	}{
		{"Elk wins", q, "elk-model", "high", "elk-model", runner.FromElk, "high", runner.FromElk},
		{"config is the fallback", q, "", "", "cfg-model", runner.FromConfig, "low", runner.FromConfig},
		{"each is resolved on its own", q, "elk-model", "", "elk-model", runner.FromElk, "low", runner.FromConfig},
		{"neither set leaves the CLI's default", config.Queue{}, "", "", "", runner.FromCLIDefault, "", runner.FromCLIDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := runner.ResolveModel(tc.q, tc.elkModel, tc.elkEffort)
			if c.Model != tc.model || c.ModelFrom != tc.from || c.Effort != tc.effort || c.EffortFrom != tc.effortFrom {
				t.Errorf("got %+v", c)
			}
		})
	}
}
