package runner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

const gib = 1 << 30

func TestSlotsTakesTheSmallestAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    Headroom
		hard int
		want int
	}{
		{
			name: "plenty of everything is the cap",
			h:    Headroom{FreeRAM: 32 * gib, RAMKnown: true, FreeDisk: 400 * gib, DiskKnown: true},
			hard: 4, want: 4,
		},
		{
			// The ruling: reduced when free RAM is under 4 GB.
			name: "under one run's memory budget is the floor",
			h:    Headroom{FreeRAM: 3 * gib, RAMKnown: true, FreeDisk: 400 * gib, DiskKnown: true},
			hard: 4, want: 1,
		},
		{
			// And under 10 GB of disk under the work directory.
			name: "under one run's disk budget is the floor",
			h:    Headroom{FreeRAM: 32 * gib, RAMKnown: true, FreeDisk: 9 * gib, DiskKnown: true},
			hard: 4, want: 1,
		},
		{
			name: "memory in between is graduated, not a cliff",
			h:    Headroom{FreeRAM: 9 * gib, RAMKnown: true, FreeDisk: 400 * gib, DiskKnown: true},
			hard: 4, want: 2,
		},
		{
			name: "disk holds it down too",
			h:    Headroom{FreeRAM: 64 * gib, RAMKnown: true, FreeDisk: 25 * gib, DiskKnown: true},
			hard: 4, want: 2,
		},
		{
			// An unmeasurable machine falls back to the cap rather than
			// refusing work: not knowing is not the same as knowing there is
			// nothing spare.
			name: "unknown headroom does not constrain",
			h:    Headroom{},
			hard: 4, want: 4,
		},
		{
			name: "one known half still constrains",
			h:    Headroom{FreeDisk: 12 * gib, DiskKnown: true},
			hard: 4, want: 1,
		},
		{
			// --max-concurrent is an override, and it overrides downwards as
			// well as up.
			name: "the explicit cap wins when it is lower",
			h:    Headroom{FreeRAM: 64 * gib, RAMKnown: true, FreeDisk: 999 * gib, DiskKnown: true},
			hard: 1, want: 1,
		},
		{
			name: "a nonsense cap is one",
			h:    Headroom{FreeRAM: 64 * gib, RAMKnown: true},
			hard: 0, want: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := Slots(tc.h, tc.hard)
			if got != tc.want {
				t.Fatalf("Slots = %d, want %d (%s)", got, tc.want, why)
			}
			if why == "" {
				t.Error("no explanation — an operator cannot tell a throttled runner from an idle one")
			}
		})
	}
}

func TestSlotsExplanationNamesTheLimit(t *testing.T) {
	_, why := Slots(Headroom{FreeRAM: 5 * gib, RAMKnown: true, FreeDisk: 400 * gib, DiskKnown: true}, 4)
	if !strings.Contains(why, "RAM") {
		t.Errorf("the explanation does not name memory as the limit: %q", why)
	}
	_, why = Slots(Headroom{FreeRAM: 64 * gib, RAMKnown: true, FreeDisk: 15 * gib, DiskKnown: true}, 4)
	if !strings.Contains(why, "disk") {
		t.Errorf("the explanation does not name disk as the limit: %q", why)
	}
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
		if !g.tryEnter() {
			t.Fatalf("entry %d refused with 32 GiB free", i)
		}
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
	if g.tryEnter() {
		t.Error("a fourth run was admitted with 2 GiB free")
	}
	mu.Lock()
	n = len(logs)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("logged %d times, want 2 — the change must be visible", n)
	}
}

func TestSlotGateWaitsForRoomAndTakesItWhenFreed(t *testing.T) {
	g := &slotGate{
		hard:  1,
		probe: func(string) Headroom { return Headroom{} },
		logf:  func(string, ...any) {},
		retry: time.Millisecond,
	}
	if !g.tryEnter() {
		t.Fatal("the first run was refused")
	}
	if g.inFlight() != 1 {
		t.Fatalf("in flight = %d, want 1", g.inFlight())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered := make(chan bool, 1)
	go func() { entered <- g.enter(ctx) }()

	select {
	case <-entered:
		t.Fatal("a second run entered a gate of one")
	case <-time.After(20 * time.Millisecond):
	}

	g.leave()
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
	g := &slotGate{
		hard:  1,
		probe: func(string) Headroom { return Headroom{} },
		logf:  func(string, ...any) {},
		retry: time.Millisecond,
	}
	if !g.tryEnter() {
		t.Fatal("the first run was refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if g.enter(ctx) {
		t.Fatal("enter reported a slot after the context ended")
	}
}

func TestDetectHeadroomMeasuresSomethingOnThisMachine(t *testing.T) {
	// Deliberately not asserting a value: this is the one place the real
	// platform code runs, and all it has to prove is that it answers.
	h := DetectHeadroom(t.TempDir())
	if !h.DiskKnown {
		t.Error("free disk was not measurable under a temp directory")
	}
	if h.DiskKnown && h.FreeDisk == 0 {
		t.Error("free disk measured as exactly zero, which is almost certainly a parse failure")
	}
	if !h.RAMKnown {
		t.Logf("free RAM is not measurable on %s — the cap will not be reduced for memory", "this platform")
	}
}
