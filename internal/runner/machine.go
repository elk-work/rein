package runner

import (
	"fmt"
	"runtime"
)

// Machine is one instant's description of the box Rein is running on: what it
// is, and how it is doing.
//
// It is the input to the `host` half of a fleet reading, and it is measured by
// the same per-platform files the concurrency gate's [Headroom] comes from —
// headroom_darwin.go, headroom_linux.go, headroom_windows.go — because on
// every platform the free number and the total number come out of the same
// source. Splitting them across two files would parse /proc/meminfo twice and
// let the two halves drift.
//
// Every measured field carries its own "known" flag, for the reason the whole
// feature exists: **a platform that cannot measure something degrades rather
// than lying.** Windows has no load average at all, and reporting 0.0 there
// would be a Windows box permanently claiming to be the idlest machine in the
// fleet. Three fields have no flag because they cannot fail — GOOS, GOARCH and
// the CPU count are answered by the runtime.
type Machine struct {
	// OSKind and Arch are Go's own GOOS and GOARCH, compiled in.
	OSKind string
	Arch   string

	// CPUCount is [runtime.NumCPU]: the logical CPUs this PROCESS may use.
	//
	// Not the machine's total, and the difference is deliberate. On Linux it
	// reflects the CPU affinity mask, so a runner in a container pinned to two
	// cores reports two — which is the number that bears on how much work this
	// runner can take, and therefore the number a fleet view wants. A machine
	// with cores the runner is forbidden to touch does not have them.
	CPUCount int

	OS      string
	OSKnown bool

	CPUModel      string
	CPUModelKnown bool

	Load1      float64
	Load1Known bool

	TotalRAM      uint64
	TotalRAMKnown bool

	// TotalDisk describes the filesystem holding the work directory, matching
	// [Headroom.FreeDisk]. Worktrees land there, not on "/".
	TotalDisk      uint64
	TotalDiskKnown bool

	// Headroom is the free half — the same measurement the concurrency gate
	// takes, carried here so one sample answers both questions.
	Headroom Headroom
}

// MachineFunc measures the machine. dir is the work directory, whose
// filesystem is the one the disk halves describe.
type MachineFunc func(dir string) Machine

// DetectMachine takes one full sample. It is the [MachineFunc] a real runner
// uses; tests inject their own, because a test that asserted against whatever
// this machine happens to be doing would assert nothing.
//
// It is deliberately a snapshot rather than a set of accessors: the reading
// that goes to Elk should describe one instant, not five of them a few
// milliseconds apart.
func DetectMachine(dir string) Machine {
	m := Machine{
		OSKind:   runtime.GOOS,
		Arch:     runtime.GOARCH,
		CPUCount: runtime.NumCPU(),
		Headroom: DetectHeadroom(dir),
	}
	if v, ok := osVersion(); ok {
		m.OS, m.OSKnown = v, true
	}
	if v, ok := cpuModel(); ok {
		m.CPUModel, m.CPUModelKnown = v, true
	}
	if v, ok := loadAvg1(); ok {
		m.Load1, m.Load1Known = v, true
	}
	if v, ok := totalRAM(); ok {
		m.TotalRAM, m.TotalRAMKnown = v, true
	}
	if dir != "" {
		if v, ok := totalDisk(dir); ok {
			m.TotalDisk, m.TotalDiskKnown = v, true
		}
	}
	return m
}

// windowsOSName renders a Windows version the way a person names it.
//
// It lives here rather than in headroom_windows.go so that it compiles and is
// tested on every platform: it is pure arithmetic over four numbers, and the
// one thing in the Windows path with a judgement call in it.
//
// The judgement is the 22000 boundary. Windows 11 reports itself as major 10,
// minor 0 — Microsoft never bumped the major — and the only thing separating
// it from Windows 10 is the build number, which crossed 22000 at 11's release.
// Every tool that names Windows correctly does this same comparison. The build
// number is kept in the string either way, so a reader can check the guess.
//
// Server editions are named as such rather than guessed at a year, because the
// mapping from build number to "Server 2019" / "Server 2022" is a table that
// goes stale, and being vaguely right beats being confidently wrong in a fleet
// listing.
func windowsOSName(major, minor, build uint32, workstation bool) string {
	switch {
	case !workstation:
		return fmt.Sprintf("Windows Server (build %d)", build)
	case major == 10 && build >= 22000:
		return fmt.Sprintf("Windows 11 (build %d)", build)
	case major == 10:
		return fmt.Sprintf("Windows 10 (build %d)", build)
	default:
		return fmt.Sprintf("Windows %d.%d (build %d)", major, minor, build)
	}
}
