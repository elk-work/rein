package runner_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// Elk Scout #816, "Fix queued Do runs that never start": two queues on one
// machine held to one slot, and what happens to the second queue's work while
// the first queue's run is with Elk's review (ark:rein#60), plus what a queue
// says on its beat when it is not starting work Elk counted (ark:rein#59).

const other = "mac-codex"

func beat(q string, n int) string {
	work := "Nothing is waiting for you."
	switch {
	case n == 1:
		work = "1 run waiting — claim with `claim_run(queue: \"" + q + "\")`."
	case n > 1:
		work = fmt.Sprintf("%d runs waiting — claim with `claim_run(queue: \"%s\")`.", n, q)
	}
	return `Heartbeat recorded for queue "` + q + `". You are online for the next 15 minutes. ` + work
}

func cleared(q string) string {
	return `Queue "` + q + `" is clear: no runs waiting in "` + space + `".`
}

// twoQueues is the harness with a second queue, mac-codex, beside mac-claude.
// Both drive the same fake agent; Elk tells them apart by the queue argument
// and the run id.
func twoQueues(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.cfg.Queues = append(h.cfg.Queues, config.Queue{Name: other, AgentKind: kind})
	if err := h.store.Set(space, other, token); err != nil {
		t.Fatal(err)
	}
	return h
}

// oneSlotOptions is a machine the gate allows one run through, whatever the box
// running the test happens to have free.
func oneSlotOptions() runner.Options {
	return runner.Options{
		MaxConcurrent:      1,
		Headroom:           func(string) runner.Headroom { return runner.Headroom{} },
		SlotInterval:       2 * time.Millisecond,
		PollInterval:       10 * time.Millisecond,
		ReviewPollInterval: 5 * time.Millisecond,
	}
}

// script answers Elk for two queues: run-a on mac-claude, flagged for Elk's
// review, and run-b on mac-codex, which appears only once run-a is submitted —
// so the test knows which run holds the one slot first.
type script struct {
	mu         sync.Mutex
	aClaimed   bool
	aSubmitted bool
	bClaimed   bool
	// revise, when set, makes the review of run-a ask for revisions once
	// run-b has its slot.
	revise  bool
	revised bool
}

func (s *script) install(h *harness) {
	h.elk.Handle("heartbeat_executor", func(args map[string]any) elktest.Reply {
		s.mu.Lock()
		defer s.mu.Unlock()
		q, _ := args["queue"].(string)
		n := 0
		switch {
		case q == queue && !s.aClaimed:
			n = 1
		case q == other && s.aSubmitted && !s.bClaimed:
			n = 1
		}
		return elktest.Reply{Text: beat(q, n)}
	})
	h.elk.Handle("claim_run", func(args map[string]any) elktest.Reply {
		s.mu.Lock()
		defer s.mu.Unlock()
		q, _ := args["queue"].(string)
		switch {
		case q == queue && !s.aClaimed:
			s.aClaimed = true
			return elktest.Reply{Text: order("run-a") + "\n" + elk.AutoReviewLine + "\n"}
		case q == other && s.aSubmitted && !s.bClaimed:
			s.bClaimed = true
			return elktest.Reply{Text: order("run-b")}
		}
		return elktest.Reply{Text: cleared(q)}
	})
	h.elk.Handle("submit_deliverable", func(args map[string]any) elktest.Reply {
		s.mu.Lock()
		defer s.mu.Unlock()
		if args["run_id"] == "run-a" {
			s.aSubmitted = true
			return elktest.Reply{Text: strings.ReplaceAll(submitReviewPending, "run-1", "run-a")}
		}
		return elktest.Reply{Text: "Deliverable saved: run `run-b` is `ready`. The user will review it in Elk."}
	})
	h.elk.Handle("ask_elk", func(map[string]any) elktest.Reply {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.revise && s.bClaimed && !s.revised {
			s.revised = true
			return elktest.Reply{Text: strings.ReplaceAll(askRevisions, "run-1", "run-a")}
		}
		return elktest.Reply{Text: askWaiting}
	})
}

// callIndex is the position of the first call matching, or -1.
func callIndex(calls []elktest.Call, from int, match func(elktest.Call) bool) int {
	for i := from; i < len(calls); i++ {
		if match(calls[i]) {
			return i
		}
	}
	return -1
}

func submitOf(run string) func(elktest.Call) bool {
	return func(c elktest.Call) bool { return c.Tool == "submit_deliverable" && c.Arg("run_id") == run }
}

// ark:rein#60: a run Elk is reviewing has no agent process — the session has
// ended, and a revision resumes it — so it gives its slot back for the wait.
// Until v0.8.3 it held the slot for the whole review window, and on a one-slot
// machine that was twenty minutes of nobody working, Wrangler cycle included.
func TestARunUnderReviewGivesItsSlotToTheNextQueue(t *testing.T) {
	h := twoQueues(t)
	s := &script{}
	s.install(h)

	opts := oneSlotOptions()
	opts.ReviewTimeout = 1500 * time.Millisecond
	if err := h.runLoop(opts, 3*time.Second); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	calls := h.elk.Calls()
	b := callIndex(calls, 0, submitOf("run-b"))
	if b < 0 {
		t.Fatalf("run-b never finished\nlog:\n%s", h.log)
	}
	// run-a was still in its review wait when run-b finished: there is a
	// review poll for it after run-b's submit. Holding the slot through the
	// wait would have kept run-b from starting until those polls had ended.
	after := callIndex(calls, b, func(c elktest.Call) bool { return c.Tool == "ask_elk" && c.Arg("run_id") == "run-a" })
	if after < 0 {
		t.Fatalf("run-b ran only after run-a's review wait was over — the slot was held through the review\nlog:\n%s", h.log)
	}
	if !strings.Contains(h.log.String(), "with the slot given back") {
		t.Errorf("the log does not say the slot was given back for the review:\n%s", h.log)
	}
}

// A run reopened for revisions while another queue's run holds the slot waits
// for it — and says so on the run, so the person who sees it `running` again
// can read why nothing is happening yet (ark:rein#59).
func TestARevisionWaitsForTheSlotAndSaysSoOnTheRun(t *testing.T) {
	h := twoQueues(t)
	s := &script{revise: true}
	s.install(h)
	// Each session takes a moment, so run-b is still holding the slot when
	// run-a's revision request arrives.
	h.agent.Script = slowScript(4)
	h.agent.Delay = 60 * time.Millisecond

	opts := oneSlotOptions()
	opts.ReviewTimeout = 800 * time.Millisecond
	if err := h.runLoop(opts, 4*time.Second); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	calls := h.elk.Calls()
	var waitReport string
	for _, c := range calls {
		if c.Tool == "report_progress" && c.Arg("run_id") == "run-a" &&
			strings.Contains(c.Arg("body"), "waiting for a slot on this machine") {
			waitReport = c.Arg("body")
			break
		}
	}
	if waitReport == "" {
		t.Fatalf("run-a waited for the slot without saying so on the run\nlog:\n%s", h.log)
	}
	if !strings.Contains(waitReport, "in use by "+other+" run run-b") {
		t.Errorf("the report does not say what holds the slot: %q", waitReport)
	}

	b := callIndex(calls, 0, submitOf("run-b"))
	first := callIndex(calls, 0, submitOf("run-a"))
	second := callIndex(calls, first+1, submitOf("run-a"))
	if b < 0 || first < 0 || second < 0 {
		t.Fatalf("submits: run-b at %d, run-a at %d and %d — want all three\nlog:\n%s", b, first, second, h.log)
	}
	if second < b {
		t.Errorf("run-a's revised submit came before run-b finished — two runs shared one slot\nlog:\n%s", h.log)
	}
	if n := len(h.agent.Specs()); n != 3 {
		t.Errorf("the agent ran %d times, want 3 (run-a, run-b, run-a's revision)", n)
	}
}

// waitingOn is the waiting_on a heartbeat carried, or "".
func waitingOn(c elktest.Call) string {
	session, _ := c.Args["session"].(map[string]any)
	s, _ := session["waiting_on"].(string)
	return s
}

func lastWaitingOn(h *harness) string {
	var last string
	for _, c := range h.elk.CallsTo("heartbeat_executor") {
		if w := waitingOn(c); w != "" {
			last = w
		}
	}
	return last
}

// ark:rein#59, the 2026-10-06 report: Elk's beat said "2 runs waiting" for
// hours while claim_run handed over other work or nothing. Claim order within
// a queue is Elk's — Rein never names a run — so Rein cannot see which runs
// were passed over. It can say that the count and the claim disagree.
func TestAQueueSaysWhenElkCountsWorkItCannotClaim(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("heartbeat_executor", beat(queue, 2))
	h.elk.Text("claim_run", cleared(queue))

	if err := h.runLoop(runner.Options{PollInterval: 10 * time.Millisecond}, 300*time.Millisecond); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	got := lastWaitingOn(h)
	for _, want := range []string{"counted 2 waiting", "claim_run handed none over", "is clear"} {
		if !strings.Contains(got, want) {
			t.Errorf("waiting_on = %q, missing %q", got, want)
		}
	}
	if n := strings.Count(h.log.String(), "handed none over"); n != 1 {
		t.Errorf("the disagreement was logged %d times over many polls, want once:\n%s", n, h.log)
	}
}

// A parked agent's claims are refused in Elk's own words. They ride the beat,
// and the log says it once rather than twice a minute.
func TestAParkedQueueSaysSoOnItsBeatAndOnceInTheLog(t *testing.T) {
	h := newHarness(t)
	const parked = `Queue "` + queue + `" is disabled: its agent was parked in Settings → Connected agents, and nothing ` +
		"is handed to a parked agent until someone enables it again. Its runs stay queued. Keep " +
		"calling `heartbeat_executor` — a parked machine should still show as parked, not absent."
	h.elk.Reply("claim_run", elktest.Reply{Text: parked, IsError: true})

	if err := h.runLoop(runner.Options{PollInterval: 10 * time.Millisecond}, 300*time.Millisecond); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if got := lastWaitingOn(h); !strings.HasPrefix(got, `claim_run refused: Queue "`+queue+`" is disabled`) {
		t.Errorf("waiting_on = %q, want Elk's own refusal", got)
	}
	if n := len(h.elk.CallsTo("claim_run")); n < 3 {
		t.Fatalf("only %d claims in the window; the test proves nothing about repetition", n)
	}
	if n := strings.Count(h.log.String(), "is disabled"); n != 1 {
		t.Errorf("the refusal was logged %d times, want once:\n%s", n, h.log)
	}
}

// Nothing waiting, nothing to explain: an idle queue's beat carries no reason.
func TestAnEmptyQueueCarriesNoWaitingReason(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("heartbeat_executor", beat(queue, 0))
	if err := h.runLoop(runner.Options{PollInterval: 10 * time.Millisecond}, 150*time.Millisecond); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if got := lastWaitingOn(h); got != "" {
		t.Errorf("an empty queue reported waiting_on %q", got)
	}
}
