package runner_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/control"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
)

// `parallel` in a queue's config.toml block (Elk #2121's follow-up): a queue
// may drive more than one of its own runs at once, when the machine can afford
// it. These tests are about the two limits staying two limits — what the
// machine affords and what the queue allows — and about everything that
// already ran beside the serve loop (a review wait, a revision, a restart, a
// drain) still doing so when the runs themselves do.

// book is a scripted Elk with a line of runs per queue. It answers the way
// Elk does: the heartbeat counts what is waiting, claim_run hands over the
// queue's next run and never one already handed out, and a run flagged for
// review is parked at its submit.
type book struct {
	mu      sync.Mutex
	waiting map[string][]string
	// review is the runs Elk's review pass takes at their submit, and
	// verdicts what it then answers each poll, in order; askWaiting after.
	review   map[string]bool
	verdicts map[string][]string
	// reviseWhen, when set, makes the review of a run ask for revisions
	// once, at the first poll where it reports true. Called unlocked.
	reviseWhen map[string]func() bool
	claims     []string
}

func newBook() *book {
	return &book{waiting: map[string][]string{}, review: map[string]bool{}, verdicts: map[string][]string{},
		reviseWhen: map[string]func() bool{}}
}

func (b *book) add(q string, runs ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.waiting[q] = append(b.waiting[q], runs...)
}

// claimed is every run handed out so far, in order.
func (b *book) claimed() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.claims...)
}

func (b *book) install(h *harness) {
	h.elk.Handle("heartbeat_executor", func(args map[string]any) elktest.Reply {
		b.mu.Lock()
		defer b.mu.Unlock()
		q, _ := args["queue"].(string)
		return elktest.Reply{Text: beat(q, len(b.waiting[q]))}
	})
	h.elk.Handle("claim_run", func(args map[string]any) elktest.Reply {
		b.mu.Lock()
		defer b.mu.Unlock()
		q, _ := args["queue"].(string)
		if len(b.waiting[q]) == 0 {
			return elktest.Reply{Text: cleared(q)}
		}
		id := b.waiting[q][0]
		b.waiting[q] = b.waiting[q][1:]
		b.claims = append(b.claims, id)
		text := order(id)
		if b.review[id] {
			text += "\n" + elk.AutoReviewLine + "\n"
		}
		return elktest.Reply{Text: text}
	})
	h.elk.Handle("submit_deliverable", func(args map[string]any) elktest.Reply {
		b.mu.Lock()
		defer b.mu.Unlock()
		id, _ := args["run_id"].(string)
		if b.review[id] {
			return elktest.Reply{Text: strings.ReplaceAll(submitReviewPending, "run-1", id)}
		}
		return elktest.Reply{Text: "Deliverable saved: run `" + id + "` is `ready`. The user will review it in Elk."}
	})
	h.elk.Handle("report_progress", func(map[string]any) elktest.Reply {
		return elktest.Reply{Text: "Progress recorded. Lease refreshed."}
	})
	h.elk.Handle("ask_elk", func(args map[string]any) elktest.Reply {
		id, _ := args["run_id"].(string)
		b.mu.Lock()
		when := b.reviseWhen[id]
		b.mu.Unlock()
		if when != nil && when() {
			b.mu.Lock()
			delete(b.reviseWhen, id)
			b.mu.Unlock()
			return elktest.Reply{Text: strings.ReplaceAll(askRevisions, "run-1", id)}
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if v := b.verdicts[id]; len(v) > 0 {
			b.verdicts[id] = v[1:]
			return elktest.Reply{Text: strings.ReplaceAll(v[0], "run-1", id)}
		}
		return elktest.Reply{Text: askWaiting}
	})
}

// roomyOptions is a machine with memory and disk to spare, so the only limits
// in play are the ceiling the test sets and the queue's own `parallel`.
func roomyOptions(ceiling int) runner.Options {
	const gib = 1 << 30
	return runner.Options{
		MaxConcurrent: ceiling,
		Headroom: func(string) runner.Headroom {
			return runner.Headroom{FreeRAM: 512 * gib, RAMKnown: true, FreeDisk: 4096 * gib, DiskKnown: true}
		},
		SlotInterval:       2 * time.Millisecond,
		PollInterval:       10 * time.Millisecond,
		ReviewPollInterval: 5 * time.Millisecond,
	}
}

// slow makes every agent session last long enough that two started close
// together are certainly both alive at once.
func slow(h *harness) {
	h.agent.Script = slowScript(6)
	h.agent.Delay = 25 * time.Millisecond
}

// sessionStart reports whether c is Rein saying an agent is about to work a
// run: the opening report of a fresh run, or the report that a revision round
// has a slot and is starting.
func sessionStart(c elktest.Call) bool {
	if c.Tool != "report_progress" {
		return false
	}
	body := c.Arg("body")
	return strings.HasPrefix(body, "Claimed by Rein on queue") || strings.Contains(body, "Working them now")
}

// overlap reads Elk's own record of the calls and returns the most runs that
// were between "an agent is starting" and that run's submit at once, and the
// runs that were when that peak was first reached. Every such stretch is
// inside the time the run held a slot, so a peak above the machine's slots is
// a machine driving more than it was allowed.
func overlap(calls []elktest.Call) (peak int, at []string) {
	open := map[string]bool{}
	for _, c := range calls {
		id := c.Arg("run_id")
		switch {
		case sessionStart(c):
			open[id] = true
		case c.Tool == "submit_deliverable":
			delete(open, id)
		}
		if len(open) > peak {
			peak = len(open)
			at = at[:0]
			for id := range open {
				at = append(at, id)
			}
		}
	}
	return peak, at
}

func submits(h *harness) int { return len(h.elk.CallsTo("submit_deliverable")) }

// maxSlotsUsed is the most slots any heartbeat said were in use.
func maxSlotsUsed(h *harness) int {
	most := 0
	for _, c := range h.elk.CallsTo("heartbeat_executor") {
		if n, ok := slotsUsed(c); ok && n > most {
			most = n
		}
	}
	return most
}

// The point of the setting: two independent tasks on one queue are worked at
// the same time when the queue says it may and the machine has the slots.
func TestAQueueSetToParallelWorksIndependentRunsAtTheSameTime(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Parallel = 2
	slow(h)
	b := newBook()
	b.add(queue, "run-a", "run-b", "run-c")
	b.install(h)

	ok, err := h.runLoopUntil(roomyOptions(4), 20*time.Second, func() bool { return submits(h) >= 3 })
	if err != nil || !ok {
		t.Fatalf("the three runs did not finish: %v\nlog:\n%s", err, h.log)
	}
	peak, at := overlap(h.elk.Calls())
	if peak != 2 {
		t.Fatalf("at most %d run(s) were being worked at once (%v), want exactly 2: the machine had four slots "+
			"and the queue allows two\nlog:\n%s", peak, at, h.log)
	}
	// Each run in a worktree of its own, each cleaned up.
	created, reaped := h.wts.counts()
	if created != 3 || reaped != 3 {
		t.Errorf("worktrees created=%d reaped=%d, want 3 and 3", created, reaped)
	}
	seen := map[string]bool{}
	for _, s := range h.agent.Specs() {
		if seen[s.RunID] {
			t.Errorf("run %s was started twice", s.RunID)
		}
		seen[s.RunID] = true
		if filepath.Base(s.WorktreeDir) != s.RunID {
			t.Errorf("run %s was started in %s, not a worktree of its own", s.RunID, s.WorktreeDir)
		}
	}
	if !strings.Contains(h.log.String(), "parallel: up to 2 runs at once on this queue") {
		t.Errorf("start-up does not say what the queue allows:\n%s", h.log)
	}
}

// Absent, or 1, is the loop as it was: one run at a time, however many slots
// the machine has free.
func TestAQueueWithoutParallelStillWorksOneRunAtATime(t *testing.T) {
	for _, set := range []int{0, 1} {
		t.Run(fmt.Sprintf("parallel=%d", set), func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Queues[0].Parallel = set
			slow(h)
			b := newBook()
			b.add(queue, "run-a", "run-b", "run-c")
			b.install(h)

			ok, err := h.runLoopUntil(roomyOptions(4), 20*time.Second, func() bool { return submits(h) >= 3 })
			if err != nil || !ok {
				t.Fatalf("the three runs did not finish: %v\nlog:\n%s", err, h.log)
			}
			if peak, at := overlap(h.elk.Calls()); peak != 1 {
				t.Fatalf("%d runs were being worked at once (%v) on a queue that did not opt in\nlog:\n%s", peak, at, h.log)
			}
			// Serial means the next claim waits for the submit before it.
			calls := h.elk.Calls()
			for i, id := range []string{"run-a", "run-b"} {
				next := []string{"run-b", "run-c"}[i]
				sub := callIndex(calls, 0, submitOf(id))
				start := callIndex(calls, 0, func(c elktest.Call) bool { return sessionStart(c) && c.Arg("run_id") == next })
				if start < sub {
					t.Errorf("%s started before %s was submitted", next, id)
				}
			}
			if used := maxSlotsUsed(h); used > 1 {
				t.Errorf("a heartbeat reported %d slots in use; this queue drives one run at a time", used)
			}
			if !strings.Contains(h.log.String(), "parallel: one run at a time on this queue") {
				t.Errorf("start-up does not say the queue is serial:\n%s", h.log)
			}
		})
	}
}

// The queue's permission is not a slot. A machine whose ceiling is one, and a
// machine with a higher ceiling that can afford only one, both stay at one
// run however much the queue allows.
func TestParallelNeverExceedsWhatTheMachineAllows(t *testing.T) {
	const gib = 1 << 30
	cases := []struct {
		name string
		opts func() runner.Options
		log  string
	}{
		{"a ceiling of one", func() runner.Options { return roomyOptions(1) },
			"config.toml says parallel = 3, held to this machine's ceiling of 1"},
		{"headroom for one", func() runner.Options {
			o := roomyOptions(4)
			// 5 GiB free at 4 GiB a run: one slot, whatever the ceiling.
			o.Headroom = func(string) runner.Headroom {
				return runner.Headroom{FreeRAM: 5 * gib, RAMKnown: true, FreeDisk: 4096 * gib, DiskKnown: true}
			}
			return o
		}, "parallel: up to 3 runs at once on this queue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Queues[0].Parallel = 3
			slow(h)
			b := newBook()
			b.add(queue, "run-a", "run-b", "run-c")
			b.install(h)

			ok, err := h.runLoopUntil(tc.opts(), 20*time.Second, func() bool { return submits(h) >= 3 })
			if err != nil || !ok {
				t.Fatalf("the three runs did not finish: %v\nlog:\n%s", err, h.log)
			}
			if peak, at := overlap(h.elk.Calls()); peak != 1 {
				t.Fatalf("%d runs were being worked at once (%v) on a machine that allows one\nlog:\n%s", peak, at, h.log)
			}
			if used := maxSlotsUsed(h); used > 1 {
				t.Errorf("a heartbeat reported %d slots in use on a one-slot machine", used)
			}
			if !strings.Contains(h.log.String(), tc.log) {
				t.Errorf("the log does not say %q:\n%s", tc.log, h.log)
			}
		})
	}
}

// A queue at its own bound says so, in words that are not about slots: the
// machine has two free, and the reason nothing more starts is config.toml.
func TestAQueueAtItsParallelLimitSaysItIsTheQueuesLimit(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Parallel = 2
	h.agent.Script = slowScript(8)
	h.agent.Delay = 40 * time.Millisecond
	b := newBook()
	b.add(queue, "run-a", "run-b", "run-c")
	b.install(h)

	said := func() string {
		for _, c := range h.elk.CallsTo("heartbeat_executor") {
			if w := waitingOn(c); strings.Contains(w, "parallel setting") {
				return w
			}
		}
		return ""
	}
	ok, err := h.runLoopUntil(roomyOptions(4), 20*time.Second, func() bool { return said() != "" && submits(h) >= 3 })
	if err != nil || !ok {
		t.Fatalf("no beat named the queue's own limit: %v\nlog:\n%s", err, h.log)
	}
	w := said()
	for _, want := range []string{"driving 2 runs", "config.toml allows (2)", "not the machine's"} {
		if !strings.Contains(w, want) {
			t.Errorf("waiting_on %q does not say %q", w, want)
		}
	}
	if strings.Contains(w, "waiting for a slot") {
		t.Errorf("waiting_on blames a slot for the queue's own limit: %q", w)
	}
	if n := strings.Count(h.log.String(), "as many as its parallel setting"); n < 1 {
		t.Errorf("the log never says why the third run waited:\n%s", h.log)
	}
}

// Fair admission across queues: a parallel queue with a long line of its own
// work joins the back of the slot line for each claim, so another queue's
// waiting claim is not starved by it.
func TestAParallelQueueDoesNotStarveAnotherQueue(t *testing.T) {
	h := twoQueues(t)
	h.cfg.Queues[0].Parallel = 2
	slow(h)
	b := newBook()
	b.add(queue, "c-1", "c-2", "c-3", "c-4", "c-5", "c-6")
	b.add(other, "x-1", "x-2")
	b.install(h)

	ok, err := h.runLoopUntil(roomyOptions(2), 30*time.Second, func() bool { return submits(h) >= 8 })
	if err != nil || !ok {
		t.Fatalf("the eight runs did not finish: %v\nlog:\n%s", err, h.log)
	}
	if peak, at := overlap(h.elk.Calls()); peak > 2 {
		t.Fatalf("%d runs were being worked at once (%v) on a two-slot machine\nlog:\n%s", peak, at, h.log)
	}
	if used := maxSlotsUsed(h); used > 2 {
		t.Errorf("a heartbeat reported %d slots in use on a two-slot machine", used)
	}
	// The other queue's two runs were both handed out before the parallel
	// queue reached the end of its own line of six.
	order := b.claimed()
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if pos["x-1"] > 2 {
		t.Errorf("the other queue's first run was claim %d of 8 (%v): it waited behind more than the two slots", pos["x-1"]+1, order)
	}
	if pos["x-2"] > pos["c-6"] {
		t.Errorf("the other queue's second run was claimed after all six of the parallel queue's (%v)", order)
	}
}

// A run Elk's review sends back is worked again while the queue drives other
// runs, takes a slot like any session, and is never started twice at once.
func TestARevisionOnAParallelQueueCountsAgainstTheMachinesSlots(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Parallel = 2
	slow(h)
	b := newBook()
	b.review["run-a"] = true
	// The review answers while two of the queue's other runs are being
	// worked, which is every slot the machine has: the revision has to wait.
	b.reviseWhen["run-a"] = func() bool {
		open := map[string]bool{}
		for _, c := range h.elk.Calls() {
			switch id := c.Arg("run_id"); {
			case id == "run-a":
			case sessionStart(c):
				open[id] = true
			case c.Tool == "submit_deliverable":
				delete(open, id)
			}
		}
		return len(open) == 2
	}
	b.add(queue, "run-a", "run-b", "run-c", "run-d")
	b.install(h)

	opts := roomyOptions(2)
	opts.ReviewTimeout = time.Hour
	// run-a submits twice: its first deliverable and its revision.
	ok, err := h.runLoopUntil(opts, 30*time.Second, func() bool { return submits(h) >= 5 })
	if err != nil || !ok {
		t.Fatalf("the runs and the revision did not finish (%d submits): %v\nlog:\n%s", submits(h), err, h.log)
	}
	if peak, at := overlap(h.elk.Calls()); peak > 2 {
		t.Fatalf("%d sessions were being worked at once (%v) on a two-slot machine: the revision did not "+
			"take a slot\nlog:\n%s", peak, at, h.log)
	}
	if used := maxSlotsUsed(h); used > 2 {
		t.Errorf("a heartbeat reported %d slots in use on a two-slot machine", used)
	}
	a := 0
	for _, s := range h.agent.Specs() {
		if s.RunID == "run-a" {
			a++
		}
	}
	// Twice at least: the first session and the revision. (The fake agent
	// says the same thing every time, so the deliverable gate asks it once
	// more for a deliverable that differs from the one sent back.)
	if a < 2 {
		t.Errorf("run-a's agent started %d times, want its first session and its revision\nlog:\n%s", a, h.log)
	}
	if !strings.Contains(h.log.String(), "run run-a: waiting for a slot on this machine") {
		t.Errorf("the revision did not wait for a slot while both were in use:\n%s", h.log)
	}
	if !strings.Contains(h.log.String(), "run run-a: Elk asked for revisions; running the agent again (round 2)") {
		t.Errorf("the revision was not worked:\n%s", h.log)
	}
}

// Elk handing over a run this queue is already working must not start a second
// agent on it: neither while a fork is driving it, nor while it is with Elk's
// review after a fork handed it off.
func TestAParallelQueueNeverStartsARunItAlreadyHolds(t *testing.T) {
	for _, phase := range []string{"being driven", "with review"} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Queues[0].Parallel = 3
			h.agent.Script = slowScript(6)
			h.agent.Delay = 30 * time.Millisecond
			var (
				mu     sync.Mutex
				handed int
				parked bool
			)
			review := phase == "with review"
			h.elk.Handle("heartbeat_executor", func(map[string]any) elktest.Reply {
				return elktest.Reply{Text: beat(queue, 1)}
			})
			h.elk.Handle("claim_run", func(map[string]any) elktest.Reply {
				mu.Lock()
				defer mu.Unlock()
				// With review: hand it over again only once it is parked.
				if handed > 0 && review && !parked {
					return elktest.Reply{Text: cleared(queue)}
				}
				handed++
				text := order("run-a")
				if review {
					text += "\n" + elk.AutoReviewLine + "\n"
				}
				return elktest.Reply{Text: text}
			})
			h.elk.Handle("submit_deliverable", func(map[string]any) elktest.Reply {
				mu.Lock()
				defer mu.Unlock()
				if review {
					parked = true
					return elktest.Reply{Text: strings.ReplaceAll(submitReviewPending, "run-1", "run-a")}
				}
				return elktest.Reply{Text: "Deliverable saved: run `run-a` is `ready`. The user will review it in Elk."}
			})
			h.elk.Handle("ask_elk", func(map[string]any) elktest.Reply { return elktest.Reply{Text: askWaiting} })

			want := "run run-a is already being driven on this queue; not starting it again"
			if review {
				want = "run run-a is already with Elk's review on this queue; not starting it again"
			}
			opts := roomyOptions(4)
			opts.ReviewTimeout = time.Hour
			ok, err := h.runLoopUntil(opts, 20*time.Second, func() bool {
				mu.Lock()
				defer mu.Unlock()
				// The log is read after the loop has stopped, not here: by
				// the fourth hand-over at least two repeats were answered.
				return handed >= 4
			})
			if err != nil || !ok {
				t.Fatalf("Elk was not asked again: %v\nlog:\n%s", err, h.log)
			}
			if !strings.Contains(h.log.String(), want) {
				t.Fatalf("the repeated claim was not refused\nlog:\n%s", h.log)
			}
			// While it was being driven every repeat was refused; once it has
			// been submitted with no review it is over, and a claim that
			// names it again is a new claim. So count sessions alive at once.
			if peak, at := overlap(h.elk.Calls()); peak > 1 {
				t.Fatalf("%d sessions were at work on run-a at once (%v)\nlog:\n%s", peak, at, h.log)
			}
			if review {
				if n := len(h.agent.Specs()); n != 1 {
					t.Fatalf("the agent started %d times on a run that was only ever with Elk's review, want 1\nlog:\n%s", n, h.log)
				}
			}
		})
	}
}

// Startup: runs Elk already holds for this queue (a runner that died) are
// taken back and worked side by side, and a run being worked is not taken
// back a second time although Elk still lists it as held.
func TestAParallelQueueTakesBackHeldRunsOnceEach(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Parallel = 2
	slow(h)
	var (
		mu    sync.Mutex
		taken = map[string]int{}
		done  = map[string]bool{}
	)
	h.elk.Handle("heartbeat_executor", func(map[string]any) elktest.Reply {
		return elktest.Reply{Text: beat(queue, 0)}
	})
	h.elk.Handle("claim_run", func(args map[string]any) elktest.Reply {
		mu.Lock()
		defer mu.Unlock()
		if id, _ := args["run_id"].(string); id != "" {
			taken[id]++
			return elktest.Reply{Text: handedBack(id)}
		}
		var held []string
		for _, id := range []string{"run-a", "run-b"} {
			if !done[id] {
				held = append(held, id)
			}
		}
		return elktest.Reply{Text: clearHolding(held...)}
	})
	h.elk.Handle("submit_deliverable", func(args map[string]any) elktest.Reply {
		mu.Lock()
		defer mu.Unlock()
		id, _ := args["run_id"].(string)
		done[id] = true
		return elktest.Reply{Text: "Deliverable saved: run `" + id + "` is `ready`. The user will review it in Elk."}
	})

	opts := roomyOptions(4)
	opts.ReclaimInterval = 5 * time.Millisecond
	ok, err := h.runLoopUntil(opts, 20*time.Second, func() bool { return submits(h) >= 2 })
	if err != nil || !ok {
		t.Fatalf("the held runs were not finished: %v\nlog:\n%s", err, h.log)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"run-a", "run-b"} {
		if taken[id] != 1 {
			t.Errorf("%s was taken back %d times, want once\nlog:\n%s", id, taken[id], h.log)
		}
	}
	if n := len(h.agent.Specs()); n != 2 {
		t.Errorf("the agent started %d times, want once per held run", n)
	}
	if peak, _ := overlap(h.elk.Calls()); peak != 2 {
		t.Errorf("the two held runs were worked %d at a time, want side by side", peak)
	}
}

// Drain: a runner told to restart claims nothing more, lets every run it is
// driving finish, and only then returns.
func TestDrainWaitsForEveryRunAParallelQueueIsDriving(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Parallel = 2
	h.agent.Script = slowScript(8)
	h.agent.Delay = 30 * time.Millisecond
	b := newBook()
	b.add(queue, "run-a", "run-b", "run-c", "run-d")
	b.install(h)

	r, err := runner.New(h.prepare(roomyOptions(4)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Both runs have an agent before the drain starts.
	deadline := time.Now().Add(15 * time.Second)
	for len(h.agent.Specs()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("two runs never started\nlog:\n%s", h.log)
		}
		time.Sleep(2 * time.Millisecond)
	}
	r.DrainForTest("test: a newer Rein is installed")

	select {
	case err := <-done:
		if !errors.Is(err, runner.ErrRestartForUpgrade) {
			t.Fatalf("Run returned %v, want the restart\nlog:\n%s", err, h.log)
		}
	case <-ctx.Done():
		t.Fatalf("the drain never finished\nlog:\n%s", h.log)
	}
	if got := b.claimed(); len(got) != 2 {
		t.Errorf("claimed %v; a draining runner must claim nothing more", got)
	}
	if n := submits(h); n != 2 {
		t.Errorf("%d runs were submitted before the restart, want both that were being driven\nlog:\n%s", n, h.log)
	}
	created, reaped := h.wts.counts()
	if created != 2 || reaped != 2 {
		t.Errorf("worktrees created=%d reaped=%d, want 2 and 2", created, reaped)
	}
}

// Shutdown: stopping Rein while a parallel queue drives runs and holds one
// with Elk's review returns promptly, and the review is still there for the
// next start to pick up.
func TestShutdownOfAParallelQueueKeepsItsPendingReview(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Parallel = 2
	b := newBook()
	b.review["run-a"] = true
	b.add(queue, "run-a")
	b.install(h)

	opts := roomyOptions(4)
	opts.StateDir = t.TempDir()
	logs, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts.Logs = logs
	opts.ReviewTimeout = time.Hour
	pending := func() []string {
		files, _ := filepath.Glob(filepath.Join(opts.StateDir, "reviews", "*.json"))
		return files
	}
	ok, err := h.runLoopUntil(opts, 20*time.Second, func() bool {
		return len(h.elk.CallsTo("ask_elk")) >= 2 && len(pending()) == 1
	})
	if err != nil || !ok {
		t.Fatalf("run-a was never parked for review: %v\nlog:\n%s", err, h.log)
	}
	if n := len(pending()); n != 1 {
		t.Fatalf("%d pending reviews on disk after the stop, want run-a's\nlog:\n%s", n, h.log)
	}
	if _, reaped := h.wts.counts(); reaped != 0 {
		t.Errorf("the worktree of a run still under review was removed at shutdown")
	}

	// The next start resumes the wait, on a parallel queue as on a serial
	// one, and works the revision the review then asks for.
	b.mu.Lock()
	b.verdicts["run-a"] = []string{askRevisions}
	b.mu.Unlock()
	ok, err = h.runLoopUntil(opts, 20*time.Second, func() bool { return submits(h) >= 2 })
	if err != nil || !ok {
		t.Fatalf("the restarted runner did not work the revision: %v\nlog:\n%s", err, h.log)
	}
	if got := b.claimed(); len(got) != 1 {
		t.Errorf("claimed %v; the resumed review is not a new claim", got)
	}
}

// `rein status` is told which runs have an agent at work, which are parked
// with Elk's review holding no slot, and how many runs the queue may drive.
func TestStatusTellsAgentSessionsFromReviewWaits(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Parallel = 2
	h.agent.Script = slowScript(40)
	h.agent.Delay = 25 * time.Millisecond
	b := newBook()
	b.review["run-a"] = true
	b.add(queue, "run-a")
	b.install(h)

	opts := roomyOptions(4)
	opts.ReviewTimeout = time.Hour
	// run-a's own session is short, so it is parked before the others start.
	h.agent.Rounds = nil
	r, err := runner.New(h.prepare(opts))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() { cancel(); <-done }()

	find := func(st control.Status, id string) string {
		for _, q := range st.Queues {
			for _, run := range q.Runs {
				if run.RunID == id {
					return run.State
				}
			}
		}
		return ""
	}
	wait := func(what string, cond func(control.Status) bool) control.Status {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			st := r.StatusForTest()
			if cond(st) {
				return st
			}
			if time.Now().After(deadline) {
				t.Fatalf("status never showed %s; last %+v\nlog:\n%s", what, st, h.log)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	st := wait("run-a with an agent session", func(st control.Status) bool {
		return find(st, "run-a") == runner.ActivitySession
	})
	if st.SlotsUsed != 1 || st.SlotsCeiling != 4 {
		t.Errorf("slots used=%d ceiling=%d while one agent works, want 1 and 4", st.SlotsUsed, st.SlotsCeiling)
	}
	if len(st.Queues) != 1 || st.Queues[0].Parallel != 2 || st.Queues[0].Driving != 1 {
		t.Errorf("queue status = %+v, want parallel 2 and one run being driven", st.Queues)
	}

	// Parked with Elk's review: still this queue's run, but no slot and no
	// agent, and no longer counted against what the queue may drive.
	st = wait("run-a with Elk's review", func(st control.Status) bool {
		return find(st, "run-a") == runner.ActivityReview
	})
	if st.SlotsUsed != 0 {
		t.Errorf("slots used = %d while the only run is with Elk's review, want 0", st.SlotsUsed)
	}
	if st.Queues[0].Driving != 0 {
		t.Errorf("driving = %d while the only run is with Elk's review, want 0", st.Queues[0].Driving)
	}

	// Two more arrive: both get agent sessions beside the review wait.
	b.add(queue, "run-b", "run-c")
	st = wait("run-b and run-c in session beside the review", func(st control.Status) bool {
		return find(st, "run-b") == runner.ActivitySession && find(st, "run-c") == runner.ActivitySession &&
			find(st, "run-a") == runner.ActivityReview
	})
	if st.SlotsUsed != 2 || st.Queues[0].Driving != 2 {
		t.Errorf("slots used=%d driving=%d with two agents at work, want 2 and 2", st.SlotsUsed, st.Queues[0].Driving)
	}
}

// The log line a Wrangler queue writes when its place at the front was spent
// on a build used to say its next claim waits "behind other queues", on
// machines that serve no other queue.
func TestTheLentPlaceIsDescribedForTheQueuesThisMachineServes(t *testing.T) {
	h := wranglerHarness(t, "wrangler")
	h.elk.Text("claim_run", order("run-1", "git"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	log := h.log.String()
	if strings.Contains(log, "behind other queues") {
		t.Errorf("a machine serving one queue is told about other queues:\n%s", log)
	}
	if !strings.Contains(log, "No other queue is served on this machine") {
		t.Errorf("the log does not say what lending the place means here:\n%s", log)
	}
}
