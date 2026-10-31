package runner_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/fake"
	"github.com/elk-work/rein/internal/runner"
)

// loginThatComesBack is the fake adapter with a Preflight that says the login
// is gone for its first `failures` calls and fine after that, counted safely:
// the queue loop calls it from its own goroutine while the test reads it.
type loginThatComesBack struct {
	*fake.Adapter
	failures int32
	calls    atomic.Int32
}

func (a *loginThatComesBack) Preflight(ctx context.Context) error {
	if n := a.calls.Add(1); n <= a.failures {
		return fmt.Errorf("%w: grok reports no login, twice: You are not authenticated.", adapter.ErrPreflight)
	}
	return nil
}

func withLogin(t *testing.T, h *harness, failures int32) *loginThatComesBack {
	t.Helper()
	a := &loginThatComesBack{Adapter: h.agent, failures: failures}
	adapter.Unregister(kind)
	if err := adapter.Register(a); err != nil {
		t.Fatal(err)
	}
	return a
}

// beatStates is the preflight_state every heartbeat carried, in order.
func beatStates(t *testing.T, h *harness) []string {
	t.Helper()
	var out []string
	for _, b := range h.elk.CallsTo("heartbeat_executor") {
		host, _ := b.Args["host"].(map[string]any)
		s, _ := host["preflight_state"].(string)
		out = append(out, s)
	}
	return out
}

// A queue whose login check failed looks again on a later heartbeat, and
// reports ready once the login is back — with no run claimed in between
// (ark:rein#94). Until then it stayed "Agent authentication required", and
// Elk kept asking somebody to sign in, until a run was requeued onto it.
func TestAFaultedQueueReportsReadyOnceItsLoginIsBack(t *testing.T) {
	h := newHarness(t)
	a := withLogin(t, h, 1)

	if err := h.runLoop(runner.Options{
		PollInterval: 10 * time.Millisecond, SetupRecheckInterval: 150 * time.Millisecond,
	}, 1500*time.Millisecond); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	states := beatStates(t, h)
	if len(states) < 2 {
		t.Fatalf("heartbeats: %v\n%s", states, h.log)
	}
	if states[0] != "Agent authentication required" {
		t.Errorf("first beat = %q, want the fault from the startup check", states[0])
	}
	if last := states[len(states)-1]; last != "ready" {
		t.Errorf("last beat = %q, want ready: %v\n%s", last, states, h.log)
	}
	if n := a.calls.Load(); n != 2 {
		t.Errorf("Preflight ran %d times, want 2: once at startup, once to heal", n)
	}
	if specs := h.agent.Specs(); len(specs) != 0 {
		t.Errorf("a run was started: %d", len(specs))
	}
}

// A login that really is gone is checked again at most once an interval, not
// on every heartbeat: the heartbeat is the poll, and for Grok each check is
// two calls to xAI.
func TestAFaultedQueueDoesNotRecheckOnEveryBeat(t *testing.T) {
	h := newHarness(t)
	a := withLogin(t, h, 1000)

	if err := h.runLoop(runner.Options{PollInterval: 10 * time.Millisecond}, 500*time.Millisecond); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if beats := len(beatStates(t, h)); beats < 5 {
		t.Fatalf("only %d heartbeats; the test needs several", beats)
	}
	if n := a.calls.Load(); n != 1 {
		t.Errorf("Preflight ran %d times inside one recheck interval, want 1 (startup)", n)
	}
}

// A healthy queue never runs its agent's login check on a heartbeat.
func TestAHealthyQueueNeverRechecksOnABeat(t *testing.T) {
	h := newHarness(t)
	a := withLogin(t, h, 0)

	if err := h.runLoop(runner.Options{
		PollInterval: 10 * time.Millisecond, SetupRecheckInterval: time.Millisecond,
	}, 300*time.Millisecond); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := a.calls.Load(); n != 1 {
		t.Errorf("Preflight ran %d times, want 1 (startup)", n)
	}
	for _, s := range beatStates(t, h) {
		if s != "ready" {
			t.Errorf("a beat said %q", s)
		}
	}
}
