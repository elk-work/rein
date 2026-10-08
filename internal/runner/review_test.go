package runner_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// The exact replies Elk gives around its auto-review pass. They are literals
// here on purpose: Rein reads all three by their wording, so a change on the
// server side should break a test rather than a dogfood.
const (
	// submit_deliverable's reply when the run is parked for review
	// (elk-mcp/queue.ts:2075). This, not the packet, is what Rein acts on.
	submitReviewPending = "Deliverable saved: run `run-1` is `ready`. " +
		"Auto-review by Elk is set on this run, so it is recorded as `ready` while Elk reviews the " +
		"deliverable before the user sees it. If Elk requests revisions the run reopens under your claim " +
		"and the request arrives as a `question` event — poll `ask_elk` for a while to catch it, then resubmit."

	// ask_elk while the run sits at `ready` waiting for the pass. Plain text,
	// no isError — the SAME shape a cancelled run produces, which is the whole
	// trap.
	askWaiting = "Not recorded: This run is `ready`, not running."

	// ask_elk once the pass has asked for changes (elk-mcp/queue.ts:1855).
	askRevisions = "Elk reviewed your deliverable on `run-1` and needs revisions before the user sees it:\n\n" +
		"The deliverable does not say which tests were run.\n\n" +
		"The run is `running` again under your claim: make the revisions and finish with " +
		"`submit_deliverable` on this same run id (it will go through Elk's review again)."

	askCancelled = "Not recorded: The user cancelled this run — STOP work on it now."
)

// reviewHarness wires a run whose submit reply parks it for review, and lets a
// test script what successive ask_elk polls answer.
func reviewHarness(t *testing.T, polls ...string) *harness {
	t.Helper()
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1")+"\n"+elk.AutoReviewLine+"\n")
	h.elk.Text("submit_deliverable", submitReviewPending)

	var n int
	var mu sync.Mutex
	h.elk.Handle("ask_elk", func(map[string]any) elktest.Reply {
		mu.Lock()
		defer mu.Unlock()
		// Once the script runs out, the answer is "still waiting" — which is
		// what really happens: a revision request is delivered once, and after
		// the agent resubmits the run is back at `ready`.
		reply := askWaiting
		if n < len(polls) {
			reply = polls[n]
		}
		n++
		return elktest.Reply{Text: reply}
	})
	return h
}

func reviewOptions() runner.Options {
	return runner.Options{
		ReviewPollInterval: time.Millisecond,
		ReviewTimeout:      150 * time.Millisecond,
	}
}

func TestAutoReviewRevisionsAreWorkedAndResubmitted(t *testing.T) {
	h := reviewHarness(t, askWaiting, askRevisions)

	if err := h.run(reviewOptions()); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	// Two submits: the original, then the revised one. Exiting after the first
	// is what stranded the run in the dogfood.
	subs := h.elk.CallsTo("submit_deliverable")
	if len(subs) != 2 {
		t.Fatalf("submits = %d, want 2 — Rein must stay for the review round\nlog:\n%s", len(subs), h.log)
	}

	// The agent was run again, resuming rather than starting over, and given
	// Elk's verdict as its direction.
	specs := h.agent.Specs()
	if len(specs) != 2 {
		t.Fatalf("the agent ran %d times, want 2", len(specs))
	}
	if specs[1].ResumeID == "" {
		t.Error("the revision round started a cold session; the fake declares resume")
	}
	if !strings.Contains(specs[1].Prompt, "does not say which tests were run") {
		t.Errorf("the revision round did not carry Elk's verdict:\n%s", specs[1].Prompt)
	}
	if strings.Contains(specs[1].Prompt, "Port the due-date sheet") {
		t.Error("a resumed session was handed the whole work order again; it already has the context")
	}
}

func TestAutoReviewApprovalIsSilenceAndTimesOut(t *testing.T) {
	// An approved run has auto_review cleared and stays `ready`, which through
	// the poll is indistinguishable from a pass that has not run yet. So the
	// settlement is a timeout, and it must not produce a second submit or a
	// stuck.
	h := reviewHarness(t) // always "waiting"

	if err := h.run(reviewOptions()); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.elk.CallsTo("submit_deliverable")); n != 1 {
		t.Errorf("submits = %d, want 1", n)
	}
	if n := len(h.elk.CallsTo("ask_elk")); n == 0 {
		t.Fatal("the review was never polled")
	}
	if strings.Contains(h.submitted().Arg("status"), "stuck") {
		t.Error("a silent review was reported as stuck")
	}
}

func TestAReadyRunIsNotMistakenForACancelledOne(t *testing.T) {
	// The trap this whole path exists to avoid. `ask_elk` on a run waiting for
	// review answers with the same "Not recorded:" sentence a cancelled run
	// does; only the status differs. Reading it as a cancellation would
	// discard finished work on every reviewed run.
	nr := &elk.NotRunningError{RunID: "run-1", Status: "ready"}
	if !nr.AwaitingReview() {
		t.Error("a ready run is not recognised as awaiting review")
	}
	cancelled := &elk.NotRunningError{RunID: "run-1", Status: "cancelled", Cancelled: true}
	if cancelled.AwaitingReview() {
		t.Error("a cancelled run was treated as merely awaiting review")
	}

	// End to end: waiting, then revisions. If the first "Not recorded" were
	// read as a cancellation there would be no second submit.
	h := reviewHarness(t, askWaiting, askWaiting, askRevisions)
	if err := h.run(reviewOptions()); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.elk.CallsTo("submit_deliverable")); n != 2 {
		t.Fatalf("submits = %d, want 2 — the ready poll was read as a cancellation\nlog:\n%s", n, h.log)
	}
}

func TestAutoReviewCancellationDiscards(t *testing.T) {
	h := reviewHarness(t, askWaiting, askCancelled)

	if err := h.run(reviewOptions()); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.elk.CallsTo("submit_deliverable")); n != 1 {
		t.Errorf("submits = %d, want 1 — a cancelled run gets nothing further", n)
	}
	if !strings.Contains(h.log.String(), "cancelled while Elk was reviewing") {
		t.Errorf("the log does not say what happened:\n%s", h.log)
	}
	if _, reaped := h.wts.counts(); reaped != 1 {
		t.Error("the worktree was not reaped after a cancellation during review")
	}
}

func TestAutoReviewIsBoundedThenStuck(t *testing.T) {
	// A verdict that has not been satisfied after the cap is one a person
	// should read, not one to keep looping on.
	h := reviewHarness(t)
	// A review that is never satisfied: every poll asks for changes again.
	h.elk.Text("ask_elk", askRevisions)
	opts := reviewOptions()
	opts.MaxReviewRounds = 2

	if err := h.run(opts); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	subs := h.elk.CallsTo("submit_deliverable")
	// Two rounds of work, then the stuck that ends the conversation.
	if len(subs) != 3 {
		t.Fatalf("submits = %d, want 3 (two rounds, then the stuck)\nlog:\n%s", len(subs), h.log)
	}
	last := subs[len(subs)-1]
	if last.Arg("status") != "stuck" {
		t.Errorf("final status = %q, want stuck", last.Arg("status"))
	}
	d := last.Arg("deliverable")
	if !strings.Contains(d, "after 2 rounds") {
		t.Errorf("the stuck reason does not say how many rounds were tried:\n%s", d)
	}
	// The last thing Elk asked for has to travel with it, or the person
	// reading it has to go and find the review themselves.
	if !strings.Contains(d, "does not say which tests were run") {
		t.Errorf("the stuck reason does not carry Elk's last verdict:\n%s", d)
	}
}

func TestARunWithoutAutoReviewDoesNotWait(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(reviewOptions()); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.elk.CallsTo("ask_elk")); n != 0 {
		t.Errorf("%d review polls on a run Elk did not park for review", n)
	}
}

func TestRevisionRoundWithoutResumeCarriesTheWholeWorkOrder(t *testing.T) {
	// An agent that cannot resume starts cold, so it needs the original work
	// order back or it has no idea what it was doing.
	h := reviewHarness(t, askRevisions)
	h.agent.Capabilities.Capabilities[adapter.CapResume] = adapter.SupportNo
	h.agent.Result.SessionID = "" // a cold adapter has nothing to resume into

	if err := h.run(reviewOptions()); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	specs := h.agent.Specs()
	if len(specs) < 2 {
		t.Fatalf("the agent ran %d times, want at least 2\nlog:\n%s", len(specs), h.log)
	}
	if specs[1].ResumeID != "" {
		t.Error("a resume was asked of an adapter that declares resume: no")
	}
	for _, want := range []string{"Port the due-date sheet", "does not say which tests were run"} {
		if !strings.Contains(specs[1].Prompt, want) {
			t.Errorf("the cold revision round is missing %q:\n%s", want, specs[1].Prompt)
		}
	}
}

func TestAutoReviewLineIsPinnedToElksOwn(t *testing.T) {
	// Rein reads the packet's intent for logging, but acts on the submit
	// reply. The literal is pinned to elk-mcp/queue.ts:137, which that file
	// says must stay byte-identical to harness/packet.mjs.
	if !strings.HasPrefix(elk.AutoReviewLine, "Auto-review by Elk is ON for this run:") {
		t.Error("AutoReviewLine has drifted from Elk's AUTO_REVIEW_LINE")
	}
	if !strings.Contains(elk.AutoReviewLine, "expect Elk to review the deliverable before the user sees it") {
		t.Error("AutoReviewLine is truncated or reworded")
	}
}
