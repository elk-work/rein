package runner_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/power"
	"github.com/elk-work/rein/internal/runner"
	"github.com/elk-work/rein/internal/worktree"
)

// sleepClock moves wall time and awake time separately, which is the whole of
// what a sleep is.
type sleepClock struct {
	mu    sync.Mutex
	wall  time.Time
	awake time.Duration
}

func (c *sleepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *sleepClock) Awake() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.awake
}

func (c *sleepClock) sleep(d time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(d)
	c.mu.Unlock()
}

// stalledWorktrees holds Create until released: the worktree step of
// ark:rein#90, which ran across 20 to 30 minutes of a Mac's sleep.
type stalledWorktrees struct {
	*fakeWorktrees
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *stalledWorktrees) Create(ctx context.Context, req worktree.Request) (*worktree.Worktree, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.fakeWorktrees.Create(ctx, req)
}

// countingAssertion is a power assertion that spawns nothing.
func countingAssertion() (*power.Assertion, *int, *int) {
	var mu sync.Mutex
	starts, stops := new(int), new(int)
	a := power.NewAssertionWith(func() (func(), error) {
		mu.Lock()
		*starts++
		mu.Unlock()
		return func() { mu.Lock(); *stops++; mu.Unlock() }, nil
	}, nil)
	return a, starts, stops
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestARunKeepsItsLeaseAcrossASleep(t *testing.T) {
	old := *runner.WakeRenewRetry
	*runner.WakeRenewRetry = 10 * time.Millisecond
	t.Cleanup(func() { *runner.WakeRenewRetry = old })

	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))

	// The first report after the wake fails, as one can while the network is
	// still coming back; the renewal tries again.
	var mu sync.Mutex
	sleptReports, failed := 0, false
	h.elk.Handle("report_progress", func(args map[string]any) elktest.Reply {
		body, _ := args["body"].(string)
		if strings.HasPrefix(body, "This machine slept for") {
			mu.Lock()
			defer mu.Unlock()
			sleptReports++
			if !failed {
				failed = true
				return elktest.Reply{Text: "upstream unavailable", IsError: true}
			}
		}
		return elktest.Reply{Text: "Progress recorded on `run-1`. Lease refreshed."}
	})

	clock := &sleepClock{wall: time.Date(2026, 10, 24, 5, 20, 40, 0, time.UTC), awake: time.Hour}
	watch := power.NewSleepWatch(clock)
	assertion, starts, stops := countingAssertion()
	wts := &stalledWorktrees{fakeWorktrees: h.wts, entered: make(chan struct{}), release: make(chan struct{})}

	opts := h.prepare(runner.Options{SleepWatch: watch, Assertion: assertion})
	opts.Once = true
	opts.Worktrees = wts
	r, err := runner.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- r.Run(ctx) }()

	select {
	case <-wts.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("the run never reached its worktree\nlog:\n%s", h.log)
	}
	if !assertion.Held() {
		t.Fatal("no power assertion while the run builds its worktree")
	}
	beatsBefore := len(h.elk.CallsTo("heartbeat_executor"))

	// Twenty minutes asleep, past Elk's 15-minute lease.
	watch.Observe()
	clock.sleep(20 * time.Minute)
	watch.Observe()

	waitFor(t, "the lease renewal after the wake", func() bool {
		for _, c := range h.elk.CallsTo("report_progress") {
			if c.Arg("run_id") == "run-1" && c.Arg("body") ==
				"This machine slept for 20m. Rein paused the run's clocks while it slept, and the run carries on." {
				mu.Lock()
				defer mu.Unlock()
				return sleptReports == 2
			}
		}
		return false
	})
	waitFor(t, "a heartbeat after the wake", func() bool {
		return len(h.elk.CallsTo("heartbeat_executor")) > beatsBefore
	})

	close(wts.release)
	if err := <-errc; err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if got := h.submitted().Arg("status"); got != "ready" {
		t.Fatalf("submitted %q, want ready\nlog:\n%s", got, h.log)
	}
	if assertion.Held() || *starts != 1 || *stops != 1 {
		t.Fatalf("assertion after the run: held %v, starts %d, stops %d; want released once",
			assertion.Held(), *starts, *stops)
	}
	if !strings.Contains(h.log.String(), "run run-1: this machine slept for 20m") {
		t.Errorf("the service log does not record the sleep\nlog:\n%s", h.log)
	}
}

// An empty claim takes no assertion: an idle queue polls every thirty seconds,
// and holding caffeinate for each poll would be a process per poll.
func TestAnEmptyClaimTakesNoAssertion(t *testing.T) {
	h := newHarness(t)
	assertion, starts, _ := countingAssertion()
	if err := h.run(runner.Options{Assertion: assertion}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if *starts != 0 {
		t.Fatalf("an empty claim took the assertion %d times", *starts)
	}
}
