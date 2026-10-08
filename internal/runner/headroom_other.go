//go:build !darwin && !linux && !windows

package runner

// freeRAM has no portable answer. On a platform Rein has never been run on,
// saying so is better than a number nobody has checked: an unknown half does
// not constrain the slot count, so the machine falls back to the cap.
func freeRAM() (uint64, bool) { return 0, false }

// The same rule for the fleet reading. Every one of these could be guessed at
// — a BSD has a `hw.physmem` sysctl, most unixes have getloadavg — but a guess
// that nobody has ever run is not a measurement, and the reading omits what it
// does not know rather than sending a number that has never been checked
// against the machine it claims to describe.
//
// GOOS, GOARCH and the CPU count still answer on every platform Go compiles
// for, so a reading from here is not empty: it says what the machine is and
// declines to say how it is doing. Whichever platform this turns out to be,
// filling these in is a small file and a good first contribution.

func totalRAM() (uint64, bool)  { return 0, false }
func loadAvg1() (float64, bool) { return 0, false }
func cpuModel() (string, bool)  { return "", false }
func osVersion() (string, bool) { return "", false }
