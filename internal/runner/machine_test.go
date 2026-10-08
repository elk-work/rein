package runner

import (
	"runtime"
	"testing"
)

// TestDetectMachineAnswersWhatTheRuntimeKnows. The three fields with no known
// flag must be present on every platform, because they are what stops a
// reading from an unsupported OS being an empty object — and Elk discards an
// empty object as no reading at all.
func TestDetectMachineAnswersWhatTheRuntimeKnows(t *testing.T) {
	m := DetectMachine(t.TempDir())
	if m.OSKind != runtime.GOOS || m.Arch != runtime.GOARCH {
		t.Errorf("os/arch = %q/%q, want %q/%q", m.OSKind, m.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if m.CPUCount < 1 {
		t.Errorf("cpu count = %d; runtime.NumCPU never returns less than one", m.CPUCount)
	}
	// Deliberately not asserting the rest: this is the one place the real
	// platform code runs, and what it can answer is the platform's business.
	// The log line is here so a CI leg that measures nothing says so out loud.
	t.Logf("os=%q known=%v cpu=%q known=%v load1=%v known=%v ram=%d known=%v disk=%d known=%v",
		m.OS, m.OSKnown, m.CPUModel, m.CPUModelKnown, m.Load1, m.Load1Known,
		m.TotalRAM, m.TotalRAMKnown, m.TotalDisk, m.TotalDiskKnown)

	// The disk halves describe the same filesystem, so a total below the free
	// space is a sign the two came from different volumes.
	if m.TotalDiskKnown && m.Headroom.DiskKnown && m.TotalDisk < m.Headroom.FreeDisk {
		t.Errorf("total disk %d is under the free disk %d — they are not the same filesystem",
			m.TotalDisk, m.Headroom.FreeDisk)
	}
	if m.TotalRAMKnown && m.TotalRAM == 0 {
		t.Error("total RAM measured as exactly zero, which is a parse failure and not a machine")
	}
}

func TestWindowsOSName(t *testing.T) {
	for _, tc := range []struct {
		name                string
		major, minor, build uint32
		workstation         bool
		want                string
	}{
		{"windows 11 is major 10 over build 22000", 10, 0, 22631, true, "Windows 11 (build 22631)"},
		{"windows 10 is major 10 under it", 10, 0, 19045, true, "Windows 10 (build 19045)"},
		{"the boundary itself is 11", 10, 0, 22000, true, "Windows 11 (build 22000)"},
		{"server is named as server, not guessed at a year", 10, 0, 20348, false, "Windows Server (build 20348)"},
		{"anything else keeps its numbers", 6, 1, 7601, true, "Windows 6.1 (build 7601)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := windowsOSName(tc.major, tc.minor, tc.build, tc.workstation); got != tc.want {
				t.Errorf("windowsOSName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSlotsLimitedByNamesTheBindingResource(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    Headroom
		want string
	}{
		{
			name: "nothing is holding it down at the cap",
			h:    Headroom{FreeRAM: 64 * gib, RAMKnown: true, FreeDisk: 900 * gib, DiskKnown: true},
			want: "",
		},
		{
			name: "memory alone",
			h:    Headroom{FreeRAM: 5 * gib, RAMKnown: true, FreeDisk: 900 * gib, DiskKnown: true},
			want: "memory",
		},
		{
			name: "disk alone",
			h:    Headroom{FreeRAM: 64 * gib, RAMKnown: true, FreeDisk: 15 * gib, DiskKnown: true},
			want: "disk",
		},
		{
			// Both, because each one binds tighter than the last: memory takes
			// the cap of four down to two, and disk then takes two down to one.
			name: "both, when each binds tighter than the last",
			h:    Headroom{FreeRAM: 9 * gib, RAMKnown: true, FreeDisk: 12 * gib, DiskKnown: true},
			want: "memory and disk",
		},
		{
			// And NOT both when the second is merely also low: disk here
			// allows one run, which is what memory already allowed, so it is
			// not holding anything down and does not claim to be.
			name: "the resource that changes nothing is not named",
			h:    Headroom{FreeRAM: 5 * gib, RAMKnown: true, FreeDisk: 12 * gib, DiskKnown: true},
			want: "memory",
		},
		{
			// An unmeasurable machine is not a limited one: it falls back to
			// the cap, so nothing is naming a limit.
			name: "unmeasured is not limited",
			h:    Headroom{},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SlotsLimitedBy(tc.h, 4); got != tc.want {
				t.Errorf("SlotsLimitedBy = %q, want %q", got, tc.want)
			}
		})
	}
}
