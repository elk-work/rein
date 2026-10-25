package runner_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// ask_elk on a `ready` run, as elk-mcp/queue.ts's notRunningWhy words it once
// the review pass has let the deliverable through, and while it is still
// thinking. Literals on purpose, like the replies in review_test.go.
const (
	askReviewPassed = "Not recorded: This run is `ready`, not running. " +
		"It is with the person for review now; there is nothing more to do on it."
	askReviewHeld = "Not recorded: This run is `ready`, not running. Elk's review of your deliverable has " +
		"not finished — this means wait, not stop: keep calling `ask_elk` with no question at least every " +
		"10 minutes until the reply changes."
)

func TestReviewPassedLineIsPinnedToElksOwn(t *testing.T) {
	if !strings.Contains(askReviewPassed, elk.ReviewPassedLine) {
		t.Error("ReviewPassedLine has drifted from notRunningWhy in elk-mcp/queue.ts")
	}
	passed := &elk.NotRunningError{RunID: "run-1", Status: "ready", Text: askReviewPassed}
	if !passed.ReviewPassed() {
		t.Error("an approved ready run is not recognised as passed")
	}
	for _, text := range []string{askWaiting, askReviewHeld} {
		nr := &elk.NotRunningError{RunID: "run-1", Status: "ready", Text: text}
		if nr.ReviewPassed() {
			t.Errorf("%q was read as a passed review", text)
		}
	}
	cancelled := &elk.NotRunningError{RunID: "run-1", Status: "cancelled", Cancelled: true, Text: askReviewPassed}
	if cancelled.ReviewPassed() {
		t.Error("a cancelled run was read as a passed review")
	}
}

// ark:rein#78: the hosted proof waited twenty minutes for an approval Elk gave
// 56 seconds after the submit. An approval Elk has said out loud is heard on
// the next poll, not at the timeout.
func TestAnApprovalElkHasGivenIsHeardOnTheNextPoll(t *testing.T) {
	h := reviewHarness(t, askReviewHeld, askReviewPassed)
	opts := reviewOptions()
	opts.ReviewTimeout = time.Hour

	start := time.Now()
	if err := h.run(opts); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("the run took %s — the approval was not heard", took)
	}
	if n := len(h.elk.CallsTo("ask_elk")); n != 2 {
		t.Errorf("ask_elk polls = %d, want 2: held, then passed", n)
	}
	if n := len(h.elk.CallsTo("submit_deliverable")); n != 1 {
		t.Errorf("submits = %d, want 1", n)
	}
	if !strings.Contains(h.log.String(), "asked for nothing") {
		t.Errorf("the settlement was not logged\nlog:\n%s", h.log)
	}
}

// ark:rein#78: a hosted run leaves right after the submit instead of idling in
// its billed container while Elk reviews.
func TestHostedRunExitsRightAfterSubmitWithoutWaitingForReview(t *testing.T) {
	h, opts := hostedHarness(t)
	h.elk.Text("claim_run", strings.ReplaceAll(order("run-1"), "repo: scout", "repo: acme/scout")+"\n"+elk.AutoReviewLine+"\n")
	h.elk.Text("submit_deliverable", submitReviewPending)
	h.elk.Handle("ask_elk", func(map[string]any) elktest.Reply { return elktest.Reply{Text: askReviewHeld} })
	opts.ReviewPollInterval = time.Millisecond
	opts.ReviewTimeout = time.Hour

	r, err := runner.New(h.prepare(opts))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if ctx.Err() != nil {
		t.Fatalf("the hosted run waited for the review\nlog:\n%s", h.log)
	}
	if n := len(h.elk.CallsTo("submit_deliverable")); n != 1 {
		t.Errorf("submits = %d, want 1", n)
	}
	if n := len(h.elk.CallsTo("ask_elk")); n != 0 {
		t.Errorf("ask_elk polls = %d, want 0 — a hosted run does not wait for the review", n)
	}
	if !strings.Contains(h.log.String(), "a revision is taken by a fresh run") {
		t.Errorf("the early exit was not explained\nlog:\n%s", h.log)
	}
}
