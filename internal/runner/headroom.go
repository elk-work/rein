package runner

import (
	"fmt"
	"strings"
)

// How many runs may be in flight at once, and on what evidence.
//
// The cap used to be one number picked in advance. That is wrong in both
// directions on the same machine: four agents on a 64 GB desktop is timid,
// and four on a laptop with two browsers and a simulator open is the
// 2026-08-20 incident, where four concurrent agents took the machine's memory
// out and froze it — the reason elk's CLAUDE.md says "cap concurrency by
// machine memory — four on a Mac".
//
// So four is the ceiling, not the number: every tick the loop asks what the
// machine has spare right now and takes the smaller of the two answers. A run
// is budgeted [RAMPerRun] of memory and [DiskPerRun] of space under the work
// directory, and gets a slot only if the machine can currently show that much.
const (
	// DefaultMaxConcurrent is the ceiling when --max-concurrent says nothing.
	DefaultMaxConcurrent = 4

	// RAMPerRun is what one run is assumed to need. A coding-agent CLI is a
	// Node process plus whatever it builds and tests, and the builds are the
	// expensive half.
	RAMPerRun = 4 << 30 // 4 GiB

	// DiskPerRun is what one run is assumed to need under the work directory:
	// a git worktree of a real repository, its build output, and room for the
	// dependency tree a test run downloads.
	DiskPerRun = 10 << 30 // 10 GiB

	// MinSlots is the floor. A machine below the budget for even one run still
	// gets one, because a runner that claims nothing is a queue that silently
	// stops draining — a worse failure than a run that is short of memory and
	// says so. The log line says which one is happening.
	MinSlots = 1
)

// Headroom is what the machine has spare, at one instant.
//
// Each half carries its own "known" flag rather than using zero as a sentinel,
// because zero free bytes is a real and important measurement and "I could not
// measure" is not the same claim. An unknown half does not constrain the slot
// count: a runner that cannot read /proc must not therefore refuse work.
type Headroom struct {
	FreeRAM   uint64
	RAMKnown  bool
	FreeDisk  uint64
	DiskKnown bool
}

// HeadroomFunc measures the machine. dir is the work directory, whose
// filesystem is the one that matters — worktrees land there, not on /.
type HeadroomFunc func(dir string) Headroom

// Slots is how many runs may be in flight, given the headroom and a hard cap,
// with a sentence saying why. The sentence is the point: a runner that quietly
// drops to one slot looks exactly like a runner with nothing to do.
func Slots(h Headroom, hard int) (int, string) {
	n, limits, _, floored := slots(h, hard)
	if hard < MinSlots {
		hard = MinSlots
	}

	var why strings.Builder
	fmt.Fprintf(&why, "%d slot", n)
	if n != 1 {
		why.WriteString("s")
	}
	switch {
	case len(limits) == 0:
		fmt.Fprintf(&why, " (the cap; %s)", describe(h))
	case floored:
		fmt.Fprintf(&why, " — the floor: %s, short of the %s of RAM and %s of disk one run is budgeted",
			strings.Join(limits, " and "), humanBytes(RAMPerRun), humanBytes(DiskPerRun))
	default:
		fmt.Fprintf(&why, " — held down by %s, budgeting %s (cap %d)", strings.Join(limits, " and "), budget(kindsOf(h, hard)), hard)
	}
	return n, why.String()
}

// The hysteresis margins: how far past the line for a slot count a reading
// has to move before the count follows it (ark:rein#33). A quarter of one
// run's budget each — enough to swallow the few hundred megabytes a busy Mac's
// free memory wanders between two readings five seconds apart, small enough
// that a real change is still followed within a tick.
const (
	RAMHysteresis  = RAMPerRun / 4
	DiskHysteresis = DiskPerRun / 4
)

// StableSlots is [Slots] for a gate whose count was prev at its last reading,
// with hysteresis, so that one steady machine gives one steady answer.
//
// Without it the count flapped (ark:rein#33): a Mac reading just either side
// of 8 GiB free — 7.98 one tick, 8.02 the next — is one slot, then two, then
// one, five seconds apart, each logged, and each logged as "8.0 GiB" because
// that is what both readings round to. Here the count rises to k only when the
// reading clears k runs' budget by a margin, and falls below k only when it is
// short of it by a margin; a reading inside the band keeps the last count.
// prev <= 0 means nothing has been measured, and the reading is taken as it
// stands.
func StableSlots(h Headroom, hard, prev int) (int, string) {
	raw, why := Slots(h, hard)
	if prev <= 0 || raw == prev {
		return raw, why
	}
	if hard < MinSlots {
		hard = MinSlots
	}
	low, _, _, _ := slots(shift(h, false), hard)
	high, _, _, _ := slots(shift(h, true), hard)
	n := prev
	switch {
	case low > prev:
		n = low
	case high < prev:
		n = high
	}
	if n > hard {
		n = hard
	}
	if n == raw {
		return raw, why
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d slot", n)
	if n != 1 {
		b.WriteString("s")
	}
	fmt.Fprintf(&b, " — kept from the last reading, within %s of RAM or %s of disk of the line for %d (%s; budgeting %s, cap %d)",
		humanBytes(RAMHysteresis), humanBytes(DiskHysteresis), raw, describe(h), budget(nil), hard)
	return n, b.String()
}

// shift moves each known half of a reading by its hysteresis margin, up or
// down, without wrapping below zero.
func shift(h Headroom, up bool) Headroom {
	move := func(v, by uint64) uint64 {
		if up {
			return v + by
		}
		if v < by {
			return 0
		}
		return v - by
	}
	if h.RAMKnown {
		h.FreeRAM = move(h.FreeRAM, RAMHysteresis)
	}
	if h.DiskKnown {
		h.FreeDisk = move(h.FreeDisk, DiskHysteresis)
	}
	return h
}

// kindsOf is the binding limits of a reading as bare nouns.
func kindsOf(h Headroom, hard int) []string {
	_, _, kinds, _ := slots(h, hard)
	return kinds
}

// budget names what one run is budgeted, for the limits that bind — so a
// sentence saying a reading holds the count down also says what it is
// measured against. nil names both.
func budget(kinds []string) string {
	var parts []string
	for _, k := range kinds {
		switch k {
		case "memory":
			parts = append(parts, humanBytes(RAMPerRun)+" of RAM")
		case "disk":
			parts = append(parts, humanBytes(DiskPerRun)+" of disk")
		}
	}
	if len(parts) == 0 {
		parts = []string{humanBytes(RAMPerRun) + " of RAM", humanBytes(DiskPerRun) + " of disk"}
	}
	return strings.Join(parts, " and ") + " a run"
}

// slots is the arithmetic behind [Slots], separated from the prose so the
// fleet reading can name the limit without parsing a sentence.
//
// It returns the count, the prose fragments naming each binding limit, the
// same limits as bare nouns — "memory", "disk" — and whether the floor was
// applied.
func slots(h Headroom, hard int) (n int, limits, kinds []string, floored bool) {
	if hard < MinSlots {
		hard = MinSlots
	}
	n = hard

	if h.RAMKnown {
		if byRAM := int(h.FreeRAM / RAMPerRun); byRAM < n {
			n = byRAM
			limits = append(limits, fmt.Sprintf("%s RAM free", humanBytesDown(h.FreeRAM)))
			kinds = append(kinds, "memory")
		}
	}
	if h.DiskKnown {
		if byDisk := int(h.FreeDisk / DiskPerRun); byDisk < n {
			n = byDisk
			limits = append(limits, fmt.Sprintf("%s disk free under the work directory", humanBytesDown(h.FreeDisk)))
			kinds = append(kinds, "disk")
		}
	}
	if n < MinSlots {
		n, floored = MinSlots, true
	}
	return n, limits, kinds, floored
}

// SlotsLimitedBy names what is holding the slot count below the configured
// ceiling: "memory", "disk", "memory and disk", or "" when nothing is.
//
// The empty answer means the runner is at its cap, which is a real state and
// not a missing measurement — the fleet reading omits the key for it, and that
// is the one place in the payload where an absent field means "no" rather than
// "unknown".
func SlotsLimitedBy(h Headroom, hard int) string {
	_, _, kinds, _ := slots(h, hard)
	return strings.Join(kinds, " and ")
}

// describe renders the measurement itself, for the unconstrained case where
// naming the limit would be misleading.
func describe(h Headroom) string {
	parts := make([]string, 0, 2)
	if h.RAMKnown {
		parts = append(parts, humanBytes(h.FreeRAM)+" RAM")
	}
	if h.DiskKnown {
		parts = append(parts, humanBytes(h.FreeDisk)+" disk")
	}
	if len(parts) == 0 {
		return "headroom unmeasurable on this platform"
	}
	return strings.Join(parts, ", ") + " free"
}

// HumanBytes renders a byte count the way a person reads one. Exported so
// `rein status` and the run loop's log agree about what 15246002176 is.
func HumanBytes(n uint64) string { return humanBytes(n) }

// bytes renders a byte count the way a person reads one.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// humanBytesDown is [humanBytes] rounded down rather than to nearest, for a
// reading that is said to hold a count down. Rounding to nearest is what made
// "1 slot — held down by 8.0 GiB RAM free" possible at a 4 GiB budget: the
// reading was 7.98 GiB, which floors to one run and rounds to 8.0
// (ark:rein#33). Rounded down, the figure and the count agree.
func humanBytesDown(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	// n*10 cannot overflow for any reading under an exbibyte.
	tenths := n * 10 / div
	return fmt.Sprintf("%d.%d %ciB", tenths/10, tenths%10, "KMGTP"[exp])
}

// DetectHeadroom measures free memory and the free space under dir. It is the
// [HeadroomFunc] a real runner uses; tests inject their own.
//
// Both halves are best-effort by design. Every platform measures "free memory"
// differently enough that a wrong answer is likelier than a missing one, so
// each implementation returns a not-known result rather than a guess when it
// cannot read what it needs.
func DetectHeadroom(dir string) Headroom {
	var h Headroom
	if free, ok := freeRAM(); ok {
		h.FreeRAM, h.RAMKnown = free, true
	}
	if dir != "" {
		if free, ok := freeDisk(dir); ok {
			h.FreeDisk, h.DiskKnown = free, true
		}
	}
	return h
}
