package power

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock moves wall time and awake time separately, which is the whole of
// what a sleep is.
type fakeClock struct {
	mu    sync.Mutex
	wall  time.Time
	awake time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{wall: time.Date(2026, 10, 24, 5, 20, 40, 0, time.UTC), awake: time.Hour}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *fakeClock) Awake() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.awake
}

// run passes d with the machine awake: both clocks move.
func (c *fakeClock) run(d time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(d)
	c.awake += d
	c.mu.Unlock()
}

// sleep passes d with the machine asleep: only wall time moves.
func (c *fakeClock) sleep(d time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(d)
	c.mu.Unlock()
}

func TestSleepWatchSeesASleepAndNothingElse(t *testing.T) {
	c := newFakeClock()
	w := NewSleepWatch(c)
	wakes, stop := w.Subscribe()
	defer stop()

	if _, ok := w.Observe(); ok {
		t.Fatal("the first reading is a baseline, not a wake")
	}
	c.run(2 * time.Second)
	if _, ok := w.Observe(); ok {
		t.Fatal("two awake seconds read as a sleep")
	}
	// The 2026-10-24 shape: awake for a moment, then asleep for 18 minutes
	// until the next maintenance wake.
	c.run(time.Second)
	c.sleep(18 * time.Minute)
	wake, ok := w.Observe()
	if !ok || wake.Slept != 18*time.Minute {
		t.Fatalf("Observe = %+v, %v; want an 18m sleep", wake, ok)
	}
	select {
	case got := <-wakes:
		if got.Slept != 18*time.Minute {
			t.Fatalf("subscriber got %s", got.Slept)
		}
	default:
		t.Fatal("the subscriber was not told")
	}
	// Small drift is not a sleep.
	c.run(2 * time.Second)
	c.sleep(3 * time.Second)
	if _, ok := w.Observe(); ok {
		t.Fatal("3s of drift read as a sleep")
	}
	// A clock stepped backwards is not a sleep either.
	c.run(2 * time.Second)
	c.sleep(-10 * time.Minute)
	if _, ok := w.Observe(); ok {
		t.Fatal("a clock stepped backwards read as a sleep")
	}
}

func TestSleepWatchFoldsWakesASubscriberHasNotRead(t *testing.T) {
	c := newFakeClock()
	w := NewSleepWatch(c)
	wakes, stop := w.Subscribe()
	w.Observe()
	c.sleep(5 * time.Minute)
	w.Observe()
	c.sleep(7 * time.Minute)
	w.Observe()
	got := <-wakes
	if got.Slept != 12*time.Minute {
		t.Fatalf("folded wake = %s, want 12m", got.Slept)
	}
	stop()
	c.sleep(time.Hour)
	w.Observe()
	select {
	case got := <-wakes:
		t.Fatalf("a stopped subscription still got %+v", got)
	default:
	}
}

func TestAssertionIsCountedAcrossRuns(t *testing.T) {
	var starts, stops int
	a := NewAssertionWith(func() (func(), error) {
		starts++
		return func() { stops++ }, nil
	}, nil)

	a.Hold()
	a.Hold()
	if starts != 1 || !a.Held() {
		t.Fatalf("two holds: starts = %d, held = %v; want one assertion in place", starts, a.Held())
	}
	a.Release()
	if stops != 0 || !a.Held() {
		t.Fatalf("one release of two let the assertion go")
	}
	a.Release()
	if stops != 1 || a.Held() {
		t.Fatalf("the last release: stops = %d, held = %v", stops, a.Held())
	}
	a.Release() // unmatched: ignored
	if stops != 1 {
		t.Fatal("an unmatched release stopped something")
	}
	a.Hold()
	if starts != 2 {
		t.Fatal("a hold after the last release did not take the assertion again")
	}
	a.Release()
}

func TestAssertionFailureIsLoggedOnceAndNotFatal(t *testing.T) {
	var logged []string
	a := NewAssertionWith(func() (func(), error) {
		return nil, errors.New("no caffeinate here")
	}, func(f string, args ...any) { logged = append(logged, f) })
	for i := 0; i < 3; i++ {
		a.Hold()
		a.Release()
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "a sleep will pause the run instead") {
		t.Fatalf("logged %q", logged)
	}
	if a.Held() {
		t.Fatal("a failed assertion reports itself held")
	}
}

func TestNilAssertionDoesNothing(t *testing.T) {
	var a *Assertion
	a.Hold()
	a.Release()
	if a.Held() {
		t.Fatal("a nil assertion is held")
	}
	NewAssertionWith(nil, nil).Hold() // no start: counts and holds nothing
}

// The awake clock never runs backwards and does move. Windows reads it at a
// coarse tick, so the test waits for movement rather than an amount.
func TestSystemAwakeClockAdvances(t *testing.T) {
	start := System.Awake()
	prev := start
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		now := System.Awake()
		if now < prev {
			t.Fatalf("the awake clock ran backwards: %s then %s", prev, now)
		}
		if now > start {
			return
		}
		prev = now
	}
	t.Fatal("the awake clock did not move in two seconds")
}
