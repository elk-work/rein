// Package power is what Rein knows about the machine going to sleep: a clock
// that stops while it sleeps, a watch that notices when it has slept, and, on
// a Mac, the assertion that keeps it from idling to sleep while a run is in
// flight (ark:rein#90).
//
// The finding behind it: a MacBook asleep on battery with its lid closed wakes
// for about 45 seconds every 4 to 18 minutes (a maintenance wake, or
// DarkWake). Rein claimed runs in those wakes, the worktree step took a
// quarter of an hour of wall time with under two minutes of it awake, and the
// claude adapter's idle guard, which measured wall time, counted the sleep as
// the agent's silence and killed it. No assertion keeps a closed-lid Mac on
// battery awake (IOPMLib.h: PreventUserIdleSystemSleep "has no effect if the
// system is in Dark Wake", and the system "may still sleep for lid close"), so
// the fix has two halves and needs both: hold the assertion, which keeps an
// open or docked Mac awake for the run, and measure every run clock in awake
// time, so that a sleep the assertion cannot stop does not count against the
// run.
package power

import "time"

// Clock reads the two clocks a run is measured on.
type Clock interface {
	// Now is wall time. It keeps counting while the machine sleeps.
	Now() time.Time

	// Awake is how long the machine has been awake since a fixed point in
	// this process's life. It stops while the machine sleeps, so the
	// difference of two readings is time the machine actually spent running.
	Awake() time.Duration
}

// System is the machine's own clocks.
//
// Awake is Go's monotonic clock: mach_absolute_time on macOS and
// CLOCK_MONOTONIC on Linux, both of which stop while the machine sleeps.
// That is measured, not assumed: on 2026-10-24 a run claimed at 05:20:40 had,
// by Rein's slot line at 05:43:56, "held its slot for 50s" (ark:rein#90). On
// Windows Go's monotonic clock counts sleep, so there a sleep still reads as
// time passing; the Windows lab machines do not sleep.
var System Clock = systemClock{}

// epoch is the fixed point System.Awake measures from. It carries a monotonic
// reading, so time.Since(epoch) is monotonic time.
var epoch = time.Now()

type systemClock struct{}

func (systemClock) Now() time.Time       { return time.Now() }
func (systemClock) Awake() time.Duration { return time.Since(epoch) }
