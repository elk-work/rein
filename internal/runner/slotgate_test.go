package runner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// oneSlot is a gate the machine allows exactly one run through: the shape of
// Issac's Mac on 2026-10-06, held to one slot by memory.
func oneSlot() *slotGate {
	return &slotGate{
		hard:  1,
		probe: func(string) Headroom { return Headroom{} },
		logf:  func(string, ...any) {},
		retry: time.Millisecond,
	}
}

// take grants a slot to a request that is alone in line, or fails the test.
func take(t *testing.T, g *slotGate, req slotRequest) *slotHold {
	t.Helper()
	w := g.join(req)
	defer g.quit(w)
	h, why := g.tryEnter(w)
	if h == nil {
		t.Fatalf("%s was refused a slot: %s", req.queue, why)
	}
	return h
}

func TestSlotGateLogsOnlyWhenTheCountChanges(t *testing.T) {
	var (
		mu   sync.Mutex
		logs []string
		free = uint64(32 * gib)
	)
	g := &slotGate{
		hard: 4,
		probe: func(string) Headroom {
			mu.Lock()
			defer mu.Unlock()
			return Headroom{FreeRAM: free, RAMKnown: true}
		},
		logf:  func(format string, args ...any) { mu.Lock(); logs = append(logs, format); mu.Unlock() },
		retry: time.Millisecond,
	}

	for i := 0; i < 3; i++ {
		take(t, g, slotRequest{queue: "mac-codex"})
	}
	mu.Lock()
	n := len(logs)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("logged %d times for an unchanging slot count, want 1", n)
	}

	// Memory disappears; the next measurement drops to the floor and says so.
	mu.Lock()
	free = 2 * gib
	mu.Unlock()
	w := g.join(slotRequest{queue: "mac-codex"})
	if h, _ := g.tryEnter(w); h != nil {
		t.Error("a fourth run was admitted with 2 GiB free")
	}
	g.quit(w)
	mu.Lock()
	n = len(logs)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("logged %d times, want 2 — the change must be visible", n)
	}
}

func TestSlotGateWaitsForRoomAndTakesItWhenFreed(t *testing.T) {
	g := oneSlot()
	first := take(t, g, slotRequest{queue: "mac-codex"})
	if g.inFlight() != 1 {
		t.Fatalf("in flight = %d, want 1", g.inFlight())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered := make(chan bool, 1)
	go func() {
		_, ok := g.enter(ctx, slotRequest{queue: "mac-grok"}, nil)
		entered <- ok
	}()

	select {
	case <-entered:
		t.Fatal("a second run entered a gate of one")
	case <-time.After(20 * time.Millisecond):
	}

	first.release()
	select {
	case ok := <-entered:
		if !ok {
			t.Fatal("the waiting run gave up once a slot was free")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting run never noticed the freed slot")
	}
}

func TestSlotGateGivesUpWhenTheContextEnds(t *testing.T) {
	g := oneSlot()
	take(t, g, slotRequest{queue: "mac-codex"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if _, ok := g.enter(ctx, slotRequest{queue: "mac-grok"}, nil); ok {
		t.Fatal("enter reported a slot after the context ended")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.waiting) != 0 {
		t.Errorf("%d requests still in line after giving up — a ghost ahead of everyone", len(g.waiting))
	}
}

// TestAWranglerClaimTakesTheNextSlotAheadOfBuildsThatWaitedLonger is
// ark:rein#60 at the gate: on 2026-10-06 the Elk Scout Wrangler cycle waited
// an hour and a half while a one-slot machine handed its slot to seven build
// runs in turn, because whichever queue happened to ask first after a run
// finished took it.
func TestAWranglerClaimTakesTheNextSlotAheadOfBuildsThatWaitedLonger(t *testing.T) {
	g := oneSlot()
	running := take(t, g, slotRequest{queue: "mac-codex"})

	// Two builds were waiting before the Wrangler queue asked.
	grok := g.join(slotRequest{queue: "mac-grok", prio: prioBuild})
	epi := g.join(slotRequest{queue: "epi-codex", prio: prioBuild})
	wrangler := g.join(slotRequest{queue: "mac-claude", prio: prioWrangler})

	running.release()

	// The builds measure first and find a free slot — and are still refused,
	// because the Wrangler is ahead of them in line.
	for _, w := range []*slotWaiter{grok, epi} {
		if h, why := g.tryEnter(w); h != nil {
			t.Fatalf("%s took the slot ahead of the waiting Wrangler", w.queue)
		} else if !strings.Contains(why, "next in line mac-claude (Wrangler)") {
			t.Errorf("%s's reason does not say the Wrangler is next: %q", w.queue, why)
		}
	}
	cycle, _ := g.tryEnter(wrangler)
	if cycle == nil {
		t.Fatal("the Wrangler was refused a free slot")
	}
	g.quit(wrangler)

	// The builds then go in the order they started waiting.
	cycle.release()
	if h, _ := g.tryEnter(epi); h != nil {
		t.Fatal("a later build jumped an earlier one")
	}
	if h, _ := g.tryEnter(grok); h == nil {
		t.Fatal("the build that waited longest was refused the free slot")
	}
}

// A run reopened by Elk's review already holds a lease and a worktree, so it
// goes ahead of claims not yet made — but behind a Wrangler cycle.
func TestARevisionGoesAheadOfBuildsAndBehindTheWrangler(t *testing.T) {
	g := oneSlot()
	running := take(t, g, slotRequest{queue: "mac-grok"})

	build := g.join(slotRequest{queue: "mac-codex", prio: prioBuild})
	revision := g.join(slotRequest{queue: "epi-claude", prio: prioRevision})
	wrangler := g.join(slotRequest{queue: "mac-claude", prio: prioWrangler})
	running.release()

	order := []*slotWaiter{wrangler, revision, build}
	for i, want := range order {
		for _, w := range order[i+1:] {
			if h, _ := g.tryEnter(w); h != nil {
				t.Fatalf("%s (%s) took the slot ahead of %s (%s)", w.queue, w.prio, want.queue, want.prio)
			}
		}
		h, why := g.tryEnter(want)
		if h == nil {
			t.Fatalf("%s (%s) was refused: %s", want.queue, want.prio, why)
		}
		g.quit(want)
		h.release()
	}
}

// With room for more than one run, priority decides who goes first, not who
// goes at all: a Wrangler in line does not keep a build out of a second slot.
func TestPriorityOrdersTheLineButDoesNotIdleFreeSlots(t *testing.T) {
	g := oneSlot()
	g.hard = 2
	wrangler := g.join(slotRequest{queue: "mac-claude", prio: prioWrangler})
	build := g.join(slotRequest{queue: "mac-codex", prio: prioBuild})
	if h, why := g.tryEnter(build); h == nil {
		t.Fatalf("a build was refused the second of two free slots: %s", why)
	}
	if h, why := g.tryEnter(wrangler); h == nil {
		t.Fatalf("the Wrangler was refused the slot kept for it: %s", why)
	}
}

func TestReleasingASlotTwiceGivesBackOne(t *testing.T) {
	g := oneSlot()
	g.hard = 4
	a := take(t, g, slotRequest{queue: "mac-claude"})
	take(t, g, slotRequest{queue: "mac-codex"})
	a.release()
	a.release()
	var none *slotHold
	none.release()
	if n := g.inFlight(); n != 1 {
		t.Fatalf("in flight = %d after releasing one of two slots twice, want 1", n)
	}
}

// The reason is what Elk shows instead of silence (ark:rein#59): how many
// slots, why that many, who holds them and for how long, and who is next.
func TestTheWaitReasonNamesTheLimitTheHolderAndTheLine(t *testing.T) {
	g := oneSlot()
	g.probe = func(string) Headroom { return Headroom{FreeRAM: 5 * gib, RAMKnown: true} }
	g.hard = 4
	h := take(t, g, slotRequest{queue: "mac-codex"})
	h.setRun("arun-48e658fb")

	g.join(slotRequest{queue: "mac-claude", prio: prioWrangler})
	w := g.join(slotRequest{queue: "mac-grok"})
	_, why := g.tryEnter(w)
	for _, want := range []string{
		slotWaitPrefix,
		"1 slot",
		"RAM free",
		"in use by mac-codex run arun-48e658fb for ",
		"next in line mac-claude (Wrangler)",
	} {
		if !strings.Contains(why, want) {
			t.Errorf("reason %q is missing %q", why, want)
		}
	}
}

func TestAGrantRemembersWhatItWaitedFor(t *testing.T) {
	g := oneSlot()
	h := take(t, g, slotRequest{queue: "mac-codex"})
	h.setRun("arun-1")
	w := g.join(slotRequest{queue: "mac-claude"})
	// Started waiting a second ago: Windows' clock is too coarse to see the
	// microseconds this test would otherwise wait.
	w.since = w.since.Add(-time.Second)
	if got, _ := g.tryEnter(w); got != nil {
		t.Fatal("entered a full gate")
	}
	h.release()
	got, _ := g.tryEnter(w)
	if got == nil {
		t.Fatal("refused a free slot")
	}
	if !strings.Contains(got.waitedOn, "mac-codex run arun-1") {
		t.Errorf("the grant forgot what it waited on: %q", got.waitedOn)
	}
	if got.waited < time.Second {
		t.Errorf("the grant recorded waiting %s, want at least the second it stood in line", got.waited)
	}
}

func TestSpanReadsLikeAPersonWouldSayIt(t *testing.T) {
	for d, want := range map[time.Duration]string{
		40 * time.Second:                "40s",
		25 * time.Minute:                "25m",
		time.Hour:                       "1h",
		time.Hour + 12*time.Minute:      "1h12m",
		26*time.Hour + 5*time.Minute:    "26h05m",
		59*time.Minute + 59*time.Second: "59m",
	} {
		if got := span(d); got != want {
			t.Errorf("span(%s) = %q, want %q", d, got, want)
		}
	}
}
