package runner

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// ark:rein#33, as observed on 2026-08-30: within five seconds "1 slot — held
// down by 8.0 GiB RAM free (cap 4)", then 2 slots, then 1, all at "8.0 GiB".
// The reading was straddling the two-run line at a 4 GiB budget.
func TestSlotGateDoesNotFlapOnAReadingStraddlingASlotLine(t *testing.T) {
	const (
		under = 8*gib - 20<<20 // 7.98 GiB
		over  = 8*gib + 20<<20 // 8.02 GiB
	)
	var (
		mu    sync.Mutex
		logs  []string
		flips int
	)
	g := &slotGate{
		hard: 4,
		probe: func(string) Headroom {
			mu.Lock()
			defer mu.Unlock()
			flips++
			if flips%2 == 0 {
				return Headroom{FreeRAM: over, RAMKnown: true}
			}
			return Headroom{FreeRAM: under, RAMKnown: true}
		},
		logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, format)
		},
		retry: time.Millisecond,
	}
	counts := map[int]bool{}
	for i := 0; i < 20; i++ {
		w := g.join(slotRequest{queue: "mac-claude"})
		h, _ := g.tryEnter(w)
		g.quit(w)
		h.release()
		counts[g.lastCount()] = true
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 1 || len(counts) != 1 {
		t.Fatalf("logged %d times over %d slot counts for one steady machine, want 1 and 1", len(logs), len(counts))
	}
}

func TestStableSlotsFollowsARealChangeButNotNoise(t *testing.T) {
	ram := func(b uint64) Headroom { return Headroom{FreeRAM: b, RAMKnown: true} }
	cases := []struct {
		name string
		free uint64
		prev int
		want int
	}{
		{"first reading is taken as it stands", 8*gib - 20<<20, 0, 1},
		{"just under the line keeps two", 8*gib - 20<<20, 2, 2},
		{"just over the line keeps one", 8*gib + 20<<20, 1, 1},
		{"a gibibyte short drops to one", 7*gib - 1, 2, 1},
		{"a gibibyte over rises to two", 9 * gib, 1, 2},
		{"a big drop goes straight to the floor", 2 * gib, 4, 1},
		{"a big rise goes straight to the cap", 64 * gib, 1, 4},
		{"at the cap, a shrinking reading inside the band holds", 16*gib - 512<<20, 4, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := StableSlots(ram(c.free), 4, c.prev)
			if got != c.want {
				t.Fatalf("StableSlots(%s free, cap 4, last %d) = %d (%s), want %d",
					humanBytes(c.free), c.prev, got, why, c.want)
			}
			if why == "" {
				t.Fatalf("no explanation: %q", why)
			}
		})
	}
}

// The sentence must not contradict its own count: 7.98 GiB at 4 GiB a run is
// one slot, and saying "held down by 8.0 GiB" made it look like a bug.
func TestSlotsSentenceAgreesWithItsCount(t *testing.T) {
	n, why := Slots(Headroom{FreeRAM: 8*gib - 20<<20, RAMKnown: true}, 4)
	if n != 1 {
		t.Fatalf("slots = %d, want 1", n)
	}
	if strings.Contains(why, "8.0 GiB") || !strings.Contains(why, "7.9 GiB RAM free") {
		t.Errorf("the sentence rounds the reading up past its own count: %q", why)
	}
	if !strings.Contains(why, "4.0 GiB of RAM a run") {
		t.Errorf("the sentence does not say what the reading is measured against: %q", why)
	}

	// A count held by hysteresis says so, rather than repeating a limit the
	// reading alone would not produce.
	n, why = StableSlots(Headroom{FreeRAM: 8*gib - 20<<20, RAMKnown: true}, 4, 2)
	if n != 2 || !strings.Contains(why, "kept from the last reading") {
		t.Errorf("StableSlots = %d, %q; want 2, kept from the last reading", n, why)
	}
}
