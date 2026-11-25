package runner_test

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/fake"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// The agent's "Person next:" line is what the run leaves a person. Rein sends
// it as person_next and takes it out of the report (personnext.go).

func runWithSummary(t *testing.T, h *harness, summary string) {
	t.Helper()
	h.elk.Text("claim_run", order("run-1"))
	h.agent.Rounds = []fake.Round{
		{Script: []adapter.Event{{Kind: adapter.EventText, Text: summary}}, Result: ended(summary)},
	}
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
}

func TestAStatedOutcomeGoesToElkAndLeavesTheReport(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("submit_deliverable", "Deliverable saved: run `run-1` is `ready`.")
	runWithSummary(t, h, fullSummary+"\n\nPerson next: check")

	subs := h.elk.CallsTo("submit_deliverable")
	if len(subs) != 1 {
		t.Fatalf("submits = %d, want 1\nlog:\n%s", len(subs), h.log)
	}
	if got := subs[0].Arg("person_next"); got != "check" {
		t.Errorf("person_next = %q, want check", got)
	}
	d := subs[0].Arg("deliverable")
	if strings.Contains(d, "Person next") {
		t.Errorf("the line was left in the deliverable:\n%s", d)
	}
	if !strings.Contains(d, fullSummary) {
		t.Errorf("the deliverable lost the agent's report:\n%s", d)
	}
}

func TestNoStatedOutcomeSendsNone(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("submit_deliverable", "Deliverable saved: run `run-1` is `ready`.")
	runWithSummary(t, h, fullSummary)

	subs := h.elk.CallsTo("submit_deliverable")
	if len(subs) != 1 {
		t.Fatalf("submits = %d, want 1\nlog:\n%s", len(subs), h.log)
	}
	if _, sent := subs[0].Args["person_next"]; sent {
		t.Errorf("person_next sent with no line in the report: %v", subs[0].Args["person_next"])
	}
}

func TestARefusedOutcomeIsSubmittedAgainWithoutIt(t *testing.T) {
	h := newHarness(t)
	var seen atomic.Int32
	h.elk.Handle("submit_deliverable", func(args map[string]any) elktest.Reply {
		if seen.Add(1) == 1 {
			return elktest.Reply{IsError: true, Text: "This run says `person_next: \"check\"` — the person " +
				"has something to do. The deliverable lacks \"What you do:\". Write it in this shape and submit again:\n" +
				"Needs you: <the check>."}
		}
		return elktest.Reply{Text: "Deliverable saved: run `run-1` is `ready`."}
	})
	runWithSummary(t, h, fullSummary+"\n\nPerson next: check")

	subs := h.elk.CallsTo("submit_deliverable")
	if len(subs) != 2 {
		t.Fatalf("submits = %d, want 2 (refused, then again without person_next)\nlog:\n%s", len(subs), h.log)
	}
	if subs[0].Arg("person_next") != "check" {
		t.Errorf("first submit person_next = %q, want check", subs[0].Arg("person_next"))
	}
	if _, sent := subs[1].Args["person_next"]; sent {
		t.Errorf("the resubmit still carried person_next %v", subs[1].Args["person_next"])
	}
	if subs[1].Arg("deliverable") != subs[0].Arg("deliverable") {
		t.Error("the resubmit changed the deliverable")
	}
	if !strings.Contains(h.log.String(), "submitting again without it") {
		t.Errorf("the fallback was not logged:\n%s", h.log)
	}
}
