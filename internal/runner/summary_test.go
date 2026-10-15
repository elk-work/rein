package runner_test

import (
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/runner"
)

// These are ark:rein#83: a run must not hand in a deliverable until it has a
// real summary. They replay the Oct 14 run (arun-75a2b7d0, Elk Scout #1089):
// the agent's first session ended on "checks are still running, I'll merge
// once they pass", and the two sessions Elk's review sent it back to ended on
// nothing at all.

const (
	deferral    = "The pull request's checks are still running. I'll merge it once they pass."
	realSummary = "I changed the server so a swiped run card comes back on real news, and tagged run " +
		"notifications with their workspace. Both landed in elk-work/scout#2171, merged as d6f1395."
)

// sessions scripts the fake agent session by session: each entry is what one
// session ends on. An empty string is a session that ends without a word.
func sessions(h *harness, closing ...string) {
	h.agent.ScriptFor = func(n int, spec adapter.RunSpec) ([]adapter.Event, adapter.Result) {
		text := ""
		if n < len(closing) {
			text = closing[n]
		}
		var script []adapter.Event
		if text != "" {
			script = append(script, adapter.Event{Kind: adapter.EventText, Text: text})
		}
		return script, adapter.Result{Status: adapter.StatusSucceeded, Summary: text, SessionID: "sess-1"}
	}
}

// TestReplayDeferralIsAskedForASummary is the first session of Oct 14. Rein
// must not submit the promise; it resumes the agent once, says why, and submits
// the report the agent then writes.
func TestReplayDeferralIsAskedForASummary(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	sessions(h, deferral, realSummary)

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	specs := h.agent.Specs()
	if len(specs) != 2 {
		t.Fatalf("the agent ran %d times, want 2: once, then once asked for a summary\nlog:\n%s", len(specs), h.log)
	}
	if specs[1].ResumeID != "sess-1" {
		t.Errorf("the summary request did not resume the session: ResumeID = %q", specs[1].ResumeID)
	}
	for _, want := range []string{"There is no later turn", "I'll merge it once they pass", "left undone", "pull request URL"} {
		if !strings.Contains(specs[1].Prompt, want) {
			t.Errorf("the summary request does not say %q:\n%s", want, specs[1].Prompt)
		}
	}

	sub := h.submitted()
	if got := sub.Arg("deliverable"); got != realSummary {
		t.Errorf("deliverable = %q, want the agent's report", got)
	}
	if sub.Arg("status") != string(elk.StatusReady) {
		t.Errorf("status = %q, want ready", sub.Arg("status"))
	}
}

// TestReplayOct14CannotHandInWithoutASummary is the whole Oct 14 run: the
// promise, then nothing when asked. Before ark:rein#83 that went to Elk as a
// ready deliverable three times and ended stuck on the review's round limit;
// now it goes once, as stuck, saying why, with what the agent did say kept.
// Elk does not review a stuck run, so this needs no review harness.
func TestReplayOct14CannotHandInWithoutASummary(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	sessions(h, deferral, "", "", "", "", "")

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	sub := h.submitted()
	if sub.Arg("status") != string(elk.StatusStuck) {
		t.Errorf("status = %q, want stuck — a run with no report is not ready", sub.Arg("status"))
	}
	body := sub.Arg("deliverable")
	for _, want := range []string{"without a finished report", "did not write one when Rein asked", deferral} {
		if !strings.Contains(body, want) {
			t.Errorf("deliverable does not say %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(sub.Arg("title"), "Finished without a report") {
		t.Errorf("title = %q", sub.Arg("title"))
	}
	if n := len(h.agent.Specs()); n != 2 {
		t.Errorf("the agent ran %d times, want 2 — one ask, never a loop", n)
	}
}

// TestReplayEmptyRevisionIsAskedForASummary is Oct 14's review rounds: Elk
// asks for changes, the resumed session ends without a word. Rein asks once
// before resubmitting, so the review sees the report and not an empty card.
func TestReplayEmptyRevisionIsAskedForASummary(t *testing.T) {
	h := reviewHarness(t, askWaiting, askRevisions)
	sessions(h, realSummary, "", realSummary+" The tests that ran: harness and Deno suites.")

	if err := h.run(reviewOptions()); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	specs := h.agent.Specs()
	if len(specs) != 3 {
		t.Fatalf("the agent ran %d times, want 3: the work, the revision, the summary ask\nlog:\n%s", len(specs), h.log)
	}
	if !strings.Contains(specs[1].Prompt, "does not say which tests were run") {
		t.Errorf("the revision round did not carry Elk's verdict word for word:\n%s", specs[1].Prompt)
	}
	if !strings.Contains(specs[2].Prompt, "wrote no closing message") {
		t.Errorf("the summary ask does not say why:\n%s", specs[2].Prompt)
	}
	subs := h.elk.CallsTo("submit_deliverable")
	if len(subs) != 2 {
		t.Fatalf("submits = %d, want 2", len(subs))
	}
	if got := subs[1].Arg("deliverable"); !strings.Contains(got, "The tests that ran") {
		t.Errorf("the resubmitted deliverable is not the agent's report:\n%s", got)
	}
}

// TestAFinishedReportIsNotQuestioned: a real report goes straight in. The ask
// is for the runs that need it, not a toll on every run.
func TestAFinishedReportIsNotQuestioned(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	sessions(h, realSummary)

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.agent.Specs()); n != 1 {
		t.Errorf("the agent ran %d times, want 1", n)
	}
	if got := h.submitted().Arg("deliverable"); got != realSummary {
		t.Errorf("deliverable = %q", got)
	}
}
