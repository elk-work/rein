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
		fmt.Fprintf(&why, " — held down by %s (cap %d)", strings.Join(limits, " and "), hard)
	}
	return n, why.String()
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
			limits = append(limits, fmt.Sprintf("%s RAM free", humanBytes(h.FreeRAM)))
			kinds = append(kinds, "memory")
		}
	}
	if h.DiskKnown {
		if byDisk := int(h.FreeDisk / DiskPerRun); byDisk < n {
			n = byDisk
			limits = append(limits, fmt.Sprintf("%s disk free under the work directory", humanBytes(h.FreeDisk)))
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
