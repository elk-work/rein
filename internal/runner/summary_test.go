package runner_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/fake"
	"github.com/elk-work/rein/internal/runner"
)

// The deliverable gate (ark:rein#83), replayed against the run that prompted
// it: arun-75a2b7d0, 2026-10-14, Elk Scout #1089. The agent opened
// elk-work/scout#2171, left a background shell watching its checks, and ended
// with a promise; when Elk's review sent that back, it ended twice with
// nothing at all, and the run went stuck with the work merged and the
// migration and deploy it needed never mentioned.

const (
	oct14Promise = "The pull request's checks are still running. I'll merge it once they pass."

	oct14Summary = "Two server fixes are merged in elk-work/scout#2171: a swiped-away lock screen card " +
		"comes back when its run has real news, and run notifications now open in their own workspace. " +
		"What is left: migration 0505 has to be applied and the two edge functions deployed; neither is " +
		"done yet.\n\nhttps://github.com/elk-work/scout/pull/2171"

	oct14Revised = "Both server fixes are merged (elk-work/scout#2171). 1. The card decision now brings a " +
		"swiped-away card back when the run has real news; migration 0505 remembers which frame was swiped. " +
		"2. Run notifications carry their workspace, so they open there. Still to do: apply 0505 and deploy " +
		"the functions.\n\nhttps://github.com/elk-work/scout/pull/2171"
)

func bashCall(id, command string) adapter.Event {
	input, _ := json.Marshal(map[string]string{"command": command})
	return adapter.Event{Kind: adapter.EventToolUse, Tool: &adapter.ToolUse{ID: id, Name: "Bash", Input: input}}
}

func ended(summary string) adapter.Result {
	return adapter.Result{Status: adapter.StatusSucceeded, Summary: summary, SessionID: "fake-session"}
}

// oct14Round1 is the first session as it streamed: the work, the pull
// request, the background check-watch, and the promise.
var oct14Round1 = []adapter.Event{
	{Kind: adapter.EventText, Text: "Pushed. Opening the pull request."},
	bashCall("t1", "gh pr create -R elk-work/scout --base main --head rein/run-run-1 --title \"A swiped run card comes back on news\""),
	bashCall("t2", "sleep 60; gh pr checks 2171 -R elk-work/scout --watch --fail-fast=false 2>&1 | tail -40"),
	{Kind: adapter.EventText, Text: oct14Promise},
}

func TestOct14ReplayNeverSubmitsAPromiseOrABlank(t *testing.T) {
	h := reviewHarness(t, askWaiting, askRevisions)
	h.agent.Rounds = []fake.Round{
		// Round 1: the promise. Rein must not submit it.
		{Script: oct14Round1, Result: ended(oct14Promise)},
		// Asked for its deliverable, the agent writes one.
		{Script: []adapter.Event{{Kind: adapter.EventText, Text: oct14Summary}}, Result: ended(oct14Summary)},
		// The review sends it back; the resumed agent runs one command and
		// ends with nothing, exactly as on the night.
		{Script: []adapter.Event{bashCall("t3", "gh pr view 2171 -R elk-work/scout --json state")}, Result: ended("")},
		// Asked again, with the review's words, it answers them.
		{Script: []adapter.Event{{Kind: adapter.EventText, Text: oct14Revised}}, Result: ended(oct14Revised)},
	}

	if err := h.run(reviewOptions()); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	subs := h.elk.CallsTo("submit_deliverable")
	if len(subs) != 2 {
		t.Fatalf("submits = %d, want 2 (the summary, then the revision)\nlog:\n%s", len(subs), h.log)
	}
	if d := subs[0].Arg("deliverable"); d != oct14Summary {
		t.Errorf("round 1 submitted something other than the summary the agent was asked for:\n%s", d)
	}
	if d := subs[1].Arg("deliverable"); d != oct14Revised {
		t.Errorf("the revision submitted something other than the agent's answer to the review:\n%s", d)
	}
	for i, s := range subs {
		if s.Arg("status") != "ready" {
			t.Errorf("submit %d status = %q, want ready", i+1, s.Arg("status"))
		}
	}

	specs := h.agent.Specs()
	if len(specs) != 4 {
		t.Fatalf("the agent ran %d times, want 4\nlog:\n%s", len(specs), h.log)
	}
	// The ask after the promise: a resume, told plainly what a deliverable is.
	ask := specs[1]
	if ask.ResumeID == "" {
		t.Error("the request for a deliverable started a cold session")
	}
	for _, want := range []string{"too short to say what was done", "what you did, and what is left", "Do not start new work"} {
		if !strings.Contains(ask.Prompt, want) {
			t.Errorf("the request for a deliverable does not say %q:\n%s", want, ask.Prompt)
		}
	}
	// The revision carries the review's own words, and says the reply
	// replaces the deliverable.
	if !strings.Contains(specs[2].Prompt, "does not say which tests were run") ||
		!strings.Contains(specs[2].Prompt, "replaces your previous deliverable") {
		t.Errorf("the revision round's prompt is missing the verdict or the replace rule:\n%s", specs[2].Prompt)
	}
	// And the second ask, after the blank, repeats the review's words.
	if !strings.Contains(specs[3].Prompt, "was empty") ||
		!strings.Contains(specs[3].Prompt, "does not say which tests were run") {
		t.Errorf("the ask after an empty revision does not carry the review's words:\n%s", specs[3].Prompt)
	}
	if !strings.Contains(h.log.String(), "not submitting yet") {
		t.Errorf("the log does not say why Rein held the submit:\n%s", h.log)
	}
}

func TestAnAgentThatWillNotSummariseGetsAnHonestAccount(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.Rounds = []fake.Round{
		{Script: oct14Round1, Result: ended(oct14Promise)},
		{Script: nil, Result: ended("")},
	}
	landing := &fakeLanding{state: runner.LandingState{
		Commits: 3, CommitsKnown: true, Pushed: true, PushKnown: true, PRsKnown: true,
	}}

	if err := h.run(runner.Options{Landing: landing}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.agent.Specs()); n != 2 {
		t.Errorf("the agent ran %d times, want 2: Rein asks once, not in a loop", n)
	}
	sub := h.submitted()
	d := sub.Arg("deliverable")
	if strings.TrimSpace(d) == "" {
		t.Fatal("an empty deliverable was submitted")
	}
	for _, want := range []string{
		"The agent finished without writing a summary, so Rein wrote this one",
		"https://github.com/elk-work/scout/pull/2171",
		"3 commit(s) beyond its base, pushed",
		"gh pr checks 2171",
		"### What the agent said last",
		"> " + oct14Promise,
	} {
		if !strings.Contains(d, want) {
			t.Errorf("the fallback deliverable lacks %q:\n%s", want, d)
		}
	}
	if !strings.HasSuffix(d, oct14Promise) {
		t.Errorf("the agent's own last words are not the deliverable's last line:\n%s", d)
	}
}

func TestARealSummaryIsSubmittedAsWritten(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.Result = ended(oct14Summary)

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.agent.Specs()); n != 1 {
		t.Errorf("the agent ran %d times, want 1: a good summary is not second-guessed", n)
	}
	if d := h.submitted().Arg("deliverable"); d != oct14Summary {
		t.Errorf("deliverable changed:\n%s", d)
	}
}

func TestAReviewWaitCutShortByAShutdownSaysSo(t *testing.T) {
	// ark:rein#81: stopping mid-review used to be logged as "Elk's review
	// asked for nothing", which was not known and was usually not true.
	h := reviewHarness(t) // always waiting
	opts := reviewOptions()
	opts.ReviewTimeout = time.Minute
	if err := h.runLoop(opts, 1500*time.Millisecond); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	log := h.log.String()
	if strings.Contains(log, "asked for nothing") {
		t.Errorf("a shutdown mid-review was reported as an approval:\n%s", log)
	}
	if !strings.Contains(log, "Rein stopped before Elk's review answered") {
		t.Errorf("the log does not say the review wait was cut short:\n%s", log)
	}
}
