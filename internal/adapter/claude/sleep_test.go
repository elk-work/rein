//go:build !windows

package claude

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
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

func (c *sleepClock) run(d time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(d)
	c.awake += d
	c.mu.Unlock()
}

// TestIdleGuardDoesNotCountSleep is ark:rein#90's regression test. The idle
// guard used to subtract a time.Unix timestamp, which is wall time, so a Mac
// asleep with its lid closed for one stretch longer than the 15-minute budget
// had its agent killed for silence the moment it woke. The guard now reads
// the session clock's awake time: a sleep of any length costs nothing, and
// the same span of awake silence still stops the session.
func TestIdleGuardDoesNotCountSleep(t *testing.T) {
	clock := &sleepClock{wall: time.Date(2026, 10, 23, 2, 29, 18, 0, time.UTC), awake: time.Hour}
	script := `printf '%s\n' '{"type":"system","subtype":"init","session_id":"asleep","apiKeySource":"none"}'
sleep 60`
	a := &Adapter{Binary: stubBinary(t, script), DisableWatchdog: true, clock: clock}
	const idle = 15 * time.Minute
	sess, err := a.Start(context.Background(), adapter.RunSpec{
		WorktreeDir:    t.TempDir(),
		Prompt:         "do the thing",
		PermissionMode: adapter.PermissionFull,
		Timeouts:       adapter.Timeouts{Idle: idle},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Interrupt(context.Background()) })

	events := make(chan adapter.Event, 64)
	go func() {
		defer close(events)
		for ev := range sess.Events() {
			events <- ev
		}
	}()
	// Wait for the session start, so the startup timeout is out of the way.
	deadline := time.After(10 * time.Second)
	for started := false; !started; {
		select {
		case _, ok := <-events:
			if !ok {
				t.Fatal("the session ended before it started")
			}
			started = sess.ID() == "asleep"
		case <-deadline:
			t.Fatal("no session start")
		}
	}

	// Twenty minutes asleep: wall time moves, awake time does not.
	clock.sleep(20 * time.Minute)
	select {
	case ev, ok := <-events:
		if !ok || ev.Kind.Terminal() {
			t.Fatalf("a 20-minute sleep ended the session: %v %q", ev.Kind, ev.Text)
		}
	case <-time.After(time.Second): // four watch ticks
	}

	// Sixteen minutes awake with nothing from the agent: that is silence.
	clock.run(16 * time.Minute)
	var last adapter.Event
	timeout := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case ev, ok := <-events:
			if !ok {
				done = true
				break
			}
			last = ev
		case <-timeout:
			t.Fatal("16 minutes of awake silence did not stop the session")
		}
	}
	res, err := sess.Wait()
	if err == nil || res.Status != adapter.StatusTimedOut {
		t.Fatalf("result %+v, %v; want timed out", res, err)
	}
	if !strings.Contains(err.Error(), "no event for "+idle.String()) {
		t.Fatalf("err = %v; want the idle guard's reason", err)
	}
	if !last.Kind.Terminal() {
		t.Fatalf("last event %v is not terminal", last.Kind)
	}
}
