package runner_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/elk/elktest"
)

// ark:scout#4728, Casey's ruling of 2026-11-10: a run Elk's review holds for a
// person's judgment must not count against its agent's slot, while a run that
// is actually working must.
//
// The two tests in queue_wait_test.go prove the ORDER — another queue's run
// starts, and this queue claims its next run, while an earlier run waits on
// review. Neither reads the COUNT, and the count is what the fleet and the
// person see: the reading on Casey's laptop on 2026-11-09 said `state:
// running` with its one slot used while nothing was running at all (Elk
// #1690), which is what made a held run look like a busy agent.
//
// One queue, one slot, and nothing else to claim, so the only thing that can
// put the count above zero is the held run itself.

// slotsUsed reads host.runner.slots_used off a heartbeat.
func slotsUsed(c elktest.Call) (int, bool) {
	host, ok := c.Args["host"].(map[string]any)
	if !ok {
		return 0, false
	}
	rn, ok := host["runner"].(map[string]any)
	if !ok {
		return 0, false
	}
	n, ok := rn["slots_used"].(float64)
	return int(n), ok
}

// beatSession reads the session half's state and run id off a heartbeat.
func beatSession(c elktest.Call) (state, runID string) {
	s, ok := c.Args["session"].(map[string]any)
	if !ok {
		return "", ""
	}
	state, _ = s["state"].(string)
	runID, _ = s["run_id"].(string)
	return state, runID
}

func TestAHeldRunDoesNotCountAgainstTheSlotButAWorkingOneDoes(t *testing.T) {
	h := newHarness(t)
	var (
		mu      sync.Mutex
		claimed bool
	)
	h.elk.Handle("heartbeat_executor", func(args map[string]any) elktest.Reply {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		if !claimed {
			n = 1
		}
		return elktest.Reply{Text: beat(queue, n)}
	})
	h.elk.Handle("claim_run", func(map[string]any) elktest.Reply {
		mu.Lock()
		defer mu.Unlock()
		if !claimed {
			claimed = true
			return elktest.Reply{Text: order("run-a", "git") + "\n" + elk.AutoReviewLine + "\n"}
		}
		return elktest.Reply{Text: cleared(queue)}
	})
	h.elk.Handle("submit_deliverable", func(map[string]any) elktest.Reply {
		return elktest.Reply{Text: strings.ReplaceAll(submitReviewPending, "run-1", "run-a")}
	})
	// Elk's review never answers: this is the state the run was found in.
	h.elk.Handle("ask_elk", func(map[string]any) elktest.Reply {
		return elktest.Reply{Text: askWaiting}
	})

	opts := oneSlotOptions()
	// The wait must not end on its own while the test is reading beats.
	opts.ReviewTimeout = time.Hour
	// Several beats land inside the session, so the working half is read from
	// a beat rather than from a gap between them.
	h.agent.Script = slowScript(6)
	h.agent.Delay = 40 * time.Millisecond

	// Every heartbeat recorded after the submit that parked run-a for review.
	beatsAfterSubmit := func() []elktest.Call {
		calls := h.elk.Calls()
		s := callIndex(calls, 0, submitOf("run-a"))
		if s < 0 {
			return nil
		}
		var out []elktest.Call
		for _, c := range calls[s+1:] {
			if c.Tool == "heartbeat_executor" {
				out = append(out, c)
			}
		}
		return out
	}

	ok, err := h.runLoopUntil(opts, 15*time.Second, func() bool {
		return len(beatsAfterSubmit()) >= 5
	})
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if !ok {
		t.Fatalf("run-a was never parked for review, or no beats followed it\nlog:\n%s", h.log)
	}

	calls := h.elk.Calls()
	submit := callIndex(calls, 0, submitOf("run-a"))

	// The run really is still held: Elk's review is still being polled.
	if callIndex(calls, submit, func(c elktest.Call) bool {
		return c.Tool == "ask_elk" && c.Arg("run_id") == "run-a"
	}) < 0 {
		t.Fatalf("run-a was not still with Elk's review after the submit\nlog:\n%s", h.log)
	}

	// A held run does not: every beat after the submit reports nothing used.
	for i, c := range beatsAfterSubmit() {
		used, okUsed := slotsUsed(c)
		if !okUsed {
			t.Fatalf("beat %d after the submit carried no slots_used: %#v", i, c.Args)
		}
		if used != 0 {
			state, runID := beatSession(c)
			t.Fatalf("beat %d after the submit reports %d slot(s) used (session %q on %q) while run-a is "+
				"only held for review and nothing is working\nlog:\n%s", i, used, state, runID, h.log)
		}
	}
}
