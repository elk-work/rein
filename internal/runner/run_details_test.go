package runner_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// Where Rein's run details go (ark:rein#77). Elk's review reads a deliverable
// from its end as closely as its start: a work order that says "end with the
// Wrangler step" is checked against the last line, and a block of Rein's after
// that line sent every such run back until the round limit made it stuck.

// wranglerEnding is an agent account that ends the way Elk Scout work orders
// ask: with the exact Wrangler step on its own last line.
const wranglerEnding = "Merged the fix and checked it on the live site.\n\nWrangler: nothing"

func TestTheAgentsLastLineIsTheDeliverablesLastLine(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.Result.Summary = wranglerEnding

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	sub := h.submitted()
	if d := sub.Arg("deliverable"); d != wranglerEnding {
		t.Errorf("the deliverable is not exactly the agent's account:\n%s", d)
	}

	// The details are the timeline's last step before the deliverable, so a
	// person reading the run finds the branch beside the outcome.
	calls := h.elk.Calls()
	var lastReport elktest.Call
	for _, c := range calls {
		if c.Tool == "submit_deliverable" {
			break
		}
		if c.Tool == "report_progress" {
			lastReport = c
		}
	}
	if !strings.HasPrefix(lastReport.Arg("body"), "**Run details**") {
		t.Errorf("the last step before the deliverable is not the run details: %q", lastReport.Arg("body"))
	}
	if !strings.Contains(lastReport.Arg("body"), "- Branch: `") {
		t.Errorf("the run details do not name the branch:\n%s", lastReport.Arg("body"))
	}
	// The final usage delta stays on the submit, where Elk's contract asks
	// for it, rather than riding on the details step.
	if _, ok := lastReport.Args["usage"]; ok {
		t.Errorf("the run details step carried usage: %#v", lastReport.Args["usage"])
	}
	if _, ok := sub.Args["usage"].(map[string]any); !ok {
		t.Errorf("the submit lost its usage: %#v", sub.Args["usage"])
	}
}

func TestRunDetailsGoFirstWhenTheTimelineRefusesThem(t *testing.T) {
	// The branch is the one line a person cannot reconstruct once the worktree
	// is gone, so a refused details step moves the block into the
	// deliverable — at the top, never after the agent's last line.
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.Result.Summary = wranglerEnding
	h.elk.Handle("report_progress", func(args map[string]any) elktest.Reply {
		if body, _ := args["body"].(string); strings.HasPrefix(body, "**Run details**") {
			return elktest.Reply{Text: "Elk could not record that just now.", IsError: true}
		}
		return elktest.Reply{Text: "Progress recorded on `run-1`. Lease refreshed."}
	})

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	sub := h.submitted()
	if sub.Arg("status") != "ready" {
		t.Errorf("status = %q, want ready", sub.Arg("status"))
	}
	d := sub.Arg("deliverable")
	if !strings.HasPrefix(d, "**Run details**\n") || !strings.Contains(d, "- Branch: `") {
		t.Errorf("the refused details are not at the top of the deliverable:\n%s", d)
	}
	if !strings.HasSuffix(d, "\n---\n\n"+wranglerEnding) {
		t.Errorf("the agent's account is not the end of the deliverable, after a rule:\n%s", d)
	}
	if !strings.Contains(h.log.String(), "did not reach the timeline") {
		t.Errorf("the local log does not say where the details went:\n%s", h.log)
	}
}

func TestAFailedRunEndsWithWhatTheAgentSaid(t *testing.T) {
	// Rein's own stuck deliverable: the explanation first, the details after
	// it, and the agent's partial account last.
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.Script = []adapter.Event{
		{Kind: adapter.EventText, Text: "Pushed the branch.\n\nWrangler: nothing"},
	}
	h.agent.WaitErr = errors.New("the agent's process exited 1")

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	sub := h.submitted()
	if sub.Arg("status") != "stuck" {
		t.Fatalf("status = %q, want stuck\n%s", sub.Arg("status"), sub.Arg("deliverable"))
	}
	d := sub.Arg("deliverable")
	why := strings.Index(d, "exited 1")
	details := strings.Index(d, "**Run details**")
	said := strings.Index(d, "### What the agent had said before it stopped")
	if !strings.HasPrefix(d, "## ") || why < 0 || details < why || said < details {
		t.Errorf("want the explanation, then the run details, then the agent's text:\n%s", d)
	}
	if !strings.HasSuffix(strings.TrimRight(d, "\n"), "Wrangler: nothing") {
		t.Errorf("something follows the agent's last line:\n%s", d)
	}
}

func TestASilentStallKeepsItsDetailsLast(t *testing.T) {
	// No agent text, so there is nothing to keep last: the explanation and
	// the details are all of it, and the "no output" sentence belongs with
	// the explanation rather than trailing the details.
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.Delay = 10 * time.Second // far past the watchdog below
	h.agent.Capabilities.Capabilities[adapter.CapResume] = adapter.SupportPartial

	if err := h.run(runner.Options{StallAfter: 50 * time.Millisecond}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	d := h.submitted().Arg("deliverable")
	noOutput := strings.Index(d, "The agent produced no output before it stopped.")
	details := strings.Index(d, "**Run details**")
	if noOutput < 0 || details < noOutput {
		t.Errorf("want the explanation, then the run details:\n%s", d)
	}
	if strings.Contains(d, "### What the agent had said") {
		t.Errorf("an empty account was given a heading:\n%s", d)
	}
}
