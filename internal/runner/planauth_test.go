package runner_test

import (
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/runner"
)

// Per-queue model and effort, the named environment passthrough, and the
// stuck report of a run stopped for not being on the plan login.

func TestTheQueuesModelAndEffortReachTheSession(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Model = "claude-opus-5-5"
	h.cfg.Queues[0].Effort = "xhigh"
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{HostCapabilities: machineHolds()}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spec := h.agent.Specs()[0]
	if spec.Model != "claude-opus-5-5" || spec.Effort != "xhigh" {
		t.Errorf("spec model %q effort %q; want the queue's", spec.Model, spec.Effort)
	}
	if !strings.Contains(h.log.String(), "model claude-opus-5-5, effort xhigh") {
		t.Errorf("the startup log does not name the queue's model and effort:\n%s", h.log)
	}
}

func TestAQueueWithoutModelOrEffortLeavesTheCLIDefault(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{HostCapabilities: machineHolds()}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if spec := h.agent.Specs()[0]; spec.Model != "" || spec.Effort != "" {
		t.Errorf("spec model %q effort %q; want both empty", spec.Model, spec.Effort)
	}
}

func TestInheritEnvIsPassedAndNothingElse(t *testing.T) {
	h := newHarness(t)
	t.Setenv("REIN_TEST_SENTRY_ORG", "elk")
	t.Setenv("REIN_TEST_STRAY", "x")
	h.cfg.InheritEnv = []string{"REIN_TEST_SENTRY_ORG"}
	h.cfg.Queues[0].InheritEnv = []string{"REIN_TEST_QUEUE_ONLY"}
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{HostCapabilities: machineHolds("docker")}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spec := h.agent.Specs()[0]
	if strings.Join(spec.PassEnv, ",") != "REIN_TEST_QUEUE_ONLY,REIN_TEST_SENTRY_ORG" {
		t.Errorf("PassEnv = %v", spec.PassEnv)
	}
	env := strings.Join(spec.InheritedEnv(), "\n")
	if !strings.Contains(env, "REIN_TEST_SENTRY_ORG=elk") || strings.Contains(env, "REIN_TEST_STRAY=") {
		t.Errorf("inherited environment is wrong:\n%s", env)
	}
}

func TestARunNotOnThePlanLoginIsReportedStuckAsSuch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
	}{
		{"refused at start", func(h *harness) {
			h.agent.StartErr = &adapter.PlanAuthError{Because: "the run's environment sets ANTHROPIC_API_KEY"}
		}},
		{"stopped at its start-up line", func(h *harness) {
			err := &adapter.PlanAuthError{Because: `Claude Code's start-up line reports apiKeySource "ANTHROPIC_API_KEY"`}
			h.agent.Script = []adapter.Event{{Kind: adapter.EventError, Err: err, Text: err.Error(),
				Result: &adapter.Result{Status: adapter.StatusFailed}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)
			h.elk.Text("claim_run", order("run-1"))
			if err := h.run(runner.Options{HostCapabilities: machineHolds()}); err != nil {
				t.Fatalf("%v\nlog:\n%s", err, h.log)
			}
			sub := h.submitted()
			if sub.Arg("status") != "stuck" {
				t.Errorf("status = %q; want stuck", sub.Arg("status"))
			}
			d := sub.Arg("deliverable")
			if !strings.Contains(d, "not on the plan login") || !strings.Contains(d, "ANTHROPIC_API_KEY") {
				t.Errorf("the deliverable does not say why the run stopped:\n%s", d)
			}
		})
	}
}
