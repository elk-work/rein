package runner_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// Elk's own answers (withClaimDeadline, heldNote and rehand,
// elk-mcp/queue.ts).
const claimDeadline = "claim_run did not finish within 5 seconds. It may still have claimed a run for you. " +
	"Call claim_run again with the same arguments: a retry hands back a run you already hold, " +
	"or claims one if this call did not."

func clearHolding(runIDs ...string) string {
	text := `Queue "` + queue + `" is clear: no runs waiting in "` + space + `".`
	if len(runIDs) == 0 {
		return text
	}
	quoted := make([]string, len(runIDs))
	for i, id := range runIDs {
		quoted[i] = "`" + id + "`"
	}
	return text + "\n\nYou hold 1 run on a live lease: " + strings.Join(quoted, ", ") +
		". If a claim_run reply never reached you, call claim_run with that run_id to read its work order again."
}

func handedBack(runID string) string {
	return "You already hold run `" + runID + "`: it was claimed under your name and its lease is live, " +
		"so this is its work order again (a claim whose reply was lost still counts). " +
		"If another session of yours is already working it, stop here.\n\n" + order(runID)
}

// elkHolding is Elk after a claim whose reply was lost: the run is claimed
// for this queue on a live lease, so a queue claim finds nothing waiting and
// lists it, and a claim naming it hands its work order back. The first
// `timeouts` calls answer Elk's deadline instead.
func elkHolding(h *harness, runID string, timeouts int) {
	var mu sync.Mutex
	calls := 0
	taken := false
	h.elk.Handle("claim_run", func(args map[string]any) elktest.Reply {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= timeouts {
			return elktest.Reply{Text: claimDeadline, IsError: true}
		}
		if id, _ := args["run_id"].(string); id == runID && !taken {
			taken = true
			return elktest.Reply{Text: handedBack(runID)}
		}
		if taken {
			return elktest.Reply{Text: clearHolding()}
		}
		return elktest.Reply{Text: clearHolding(runID)}
	})
}

func TestATimedOutClaimIsRetriedAndTheRunElkHoldsIsStarted(t *testing.T) {
	// Elk #1772: claim_run timed out after claiming a run, Rein moved on, and
	// the run sat `running` with no agent while the heartbeat said nothing
	// was waiting.
	h := newHarness(t)
	emptyQueue(h)
	elkHolding(h, "run-1", 1)

	if err := h.run(runner.Options{ClaimRetryBackoff: time.Millisecond}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	claims := h.elk.CallsTo("claim_run")
	if len(claims) != 3 {
		t.Fatalf("claims = %d, want 3 (timed out, retried, taken back)\nlog:\n%s", len(claims), h.log)
	}
	// The retry asks with exactly the same arguments.
	if a, b := claims[0].Args, claims[1].Args; len(a) != len(b) || a["queue"] != b["queue"] || a["workspace"] != b["workspace"] || b["run_id"] != nil {
		t.Errorf("the retry's arguments differ: %#v then %#v", a, b)
	}
	if claims[2].Arg("run_id") != "run-1" {
		t.Errorf("the held run was not asked for by its id: %#v", claims[2].Args)
	}
	if h.submitted().Arg("status") != "ready" {
		t.Error("the run Elk held was not driven to completion")
	}
	if specs := h.agent.Specs(); len(specs) != 1 || strings.Contains(specs[0].Prompt, "already hold") {
		t.Errorf("the agent was not started on the run's own work order: %d sessions", len(specs))
	}
	log := h.log.String()
	for _, want := range []string{"may have claimed a run without answering (attempt 1 of 4)", "took back run run-1"} {
		if !strings.Contains(log, want) {
			t.Errorf("log is missing %q:\n%s", want, log)
		}
	}
}

func TestClaimRetriesAreBoundedAndSayWhyTheyStopped(t *testing.T) {
	h := newHarness(t)
	h.elk.Reply("claim_run", elktest.Reply{Text: claimDeadline, IsError: true})

	err := h.run(runner.Options{ClaimRetryBackoff: time.Millisecond})
	if err == nil {
		t.Fatalf("a claim that never answered reported success\nlog:\n%s", h.log)
	}
	if n := len(h.elk.CallsTo("claim_run")); n != runner.ClaimAttempts {
		t.Fatalf("claims = %d, want %d", n, runner.ClaimAttempts)
	}
	if !strings.Contains(h.log.String(), "stopped retrying claim_run after 4 attempts: claim_run did not finish within 5 seconds") {
		t.Errorf("the log does not say why the retries stopped:\n%s", h.log)
	}
	if len(h.agent.Specs()) != 0 {
		t.Error("an agent started with no work order")
	}
}

func TestOnStartupARunElkAlreadyHoldsIsTakenBack(t *testing.T) {
	// A Rein that died mid-run leaves its run on a live lease: not waiting,
	// so not counted, and not handed out by a queue claim. Startup asks.
	h := newHarness(t)
	emptyQueue(h)
	elkHolding(h, "run-1", 0)

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	claims := h.elk.CallsTo("claim_run")
	if len(claims) != 2 || claims[1].Arg("run_id") != "run-1" {
		t.Fatalf("claims = %#v, want a queue claim then run-1 by id\nlog:\n%s", claims, h.log)
	}
	if h.submitted().Arg("status") != "ready" {
		t.Error("the held run was not driven to completion")
	}
}

func TestAfterAClaimTimeoutTheNextPollLooksForHeldRuns(t *testing.T) {
	// Every attempt of the first poll times out. The heartbeat then says
	// nothing is waiting, and the sweep is an hour away; the next poll must
	// still ask, and start the run Elk holds.
	h := newHarness(t)
	emptyQueue(h)
	elkHolding(h, "run-1", runner.ClaimAttempts)

	if ok, err := h.runLoopUntil(runner.Options{
		PollInterval:      time.Millisecond,
		ReclaimInterval:   time.Hour,
		ClaimRetryBackoff: time.Millisecond,
	}, 30*time.Second, func() bool { return len(h.elk.CallsTo("submit_deliverable")) == 1 }); !ok {
		t.Fatalf("the held run was never started (err %v)\nlog:\n%s", err, h.log)
	}
	if n := len(h.agent.Specs()); n != 1 {
		t.Errorf("agent sessions = %d, want 1", n)
	}
}

func TestWithNothingHeldTheLoopTrustsTheDepthAgain(t *testing.T) {
	// With nothing held, startup's look costs one claim and the loop goes
	// back to trusting the depth.
	h := newHarness(t)
	emptyQueue(h)
	h.elk.Text("claim_run", clearHolding())
	if ok, _ := h.runLoopUntil(runner.Options{
		PollInterval:    time.Millisecond,
		ReclaimInterval: time.Hour,
	}, 30*time.Second, func() bool { return len(h.elk.CallsTo("heartbeat_executor")) >= 3 }); !ok {
		t.Fatalf("the loop never reached a third heartbeat\nlog:\n%s", h.log)
	}
	if n := len(h.elk.CallsTo("claim_run")); n != 1 {
		t.Errorf("claims = %d, want 1", n)
	}
}
