package runner

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX structure. x/sys/windows
// wraps GetDiskFreeSpaceEx but not GlobalMemoryStatusEx, so the struct and the
// call are declared here rather than pulling in a dependency for one function.
//
// dwLength must be set to the struct's size before the call; the API uses it
// to version the structure and fails with ERROR_INVALID_PARAMETER otherwise.
type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

var (
	modkernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatus makes the one call both memory readings come out of, so a
// machine is not asked twice for two halves of the same answer.
func memoryStatus() (memoryStatusEx, bool) {
	st := memoryStatusEx{}
	st.dwLength = uint32(unsafe.Sizeof(st))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st)))
	if r == 0 {
		return memoryStatusEx{}, false
	}
	return st, true
}

// freeRAM on Windows is ullAvailPhys: physical memory available to processes,
// which already accounts for the standby list the same way MemAvailable does
// on Linux and the inactive pages do on macOS.
func freeRAM() (uint64, bool) {
	st, ok := memoryStatus()
	if !ok {
		return 0, false
	}
	return st.ullAvailPhys, true
}

// totalRAM is ullTotalPhys: the physical memory installed.
func totalRAM() (uint64, bool) {
	st, ok := memoryStatus()
	if !ok || st.ullTotalPhys == 0 {
		return 0, false
	}
	return st.ullTotalPhys, true
}

// freeDisk is the space available to this user on the volume holding dir.
//
// freeBytesAvailableToCaller, not totalNumberOfFreeBytes: on a volume with
// quotas the two differ, and the first is the one a run can actually write
// into.
func freeDisk(dir string) (uint64, bool) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, false
	}
	return avail, true
}

// totalDisk is the size of the volume holding dir — the denominator the free
// half above is read against.
//
// totalNumberOfBytes, which is the second out-parameter and is quota-adjusted
// for the caller the same way the first one is. On a volume with no quotas the
// two agree with the volume's real size; on one with quotas they describe what
// this user has, which is the only figure that means anything to a runner.
func totalDisk(dir string) (uint64, bool) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, false
	}
	if total == 0 {
		return 0, false
	}
	return total, true
}

// loadAvg1 has no answer on Windows, and that is the whole point of saying so.
//
// Windows publishes no load average: there is a "% Processor Time" performance
// counter, which is an instantaneous utilisation and not the run-queue average
// the other two platforms report. Returning that would put a number in the
// same field meaning something else, which is exactly the failure the
// omit-rather-than-zero rule exists to prevent. So a Windows runner reports no
// load, and Elk renders it as "could not measure" — which is true.
func loadAvg1() (float64, bool) { return 0, false }

// cpuModel comes out of the registry key the CPU driver populates at boot:
//
//	HKLM\HARDWARE\DESCRIPTION\System\CentralProcessor\0 → ProcessorNameString
//
// That is where Task Manager and `wmic cpu get name` both read it from, and it
// is one registry read rather than a WMI query, which on a cold machine can
// take seconds and needs a service that is sometimes switched off.
func cpuModel() (string, bool) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer func() { _ = k.Close() }()
	s, _, err := k.GetStringValue("ProcessorNameString")
	if err != nil {
		return "", false
	}
	if s = strings.TrimSpace(s); s == "" {
		return "", false
	}
	return s, true
}

// osVersion asks the kernel rather than the Win32 shim.
//
// RtlGetVersion reports the real version. GetVersionEx lies to a process
// without the right entry in its application manifest — it has capped its
// answer at 6.2 since Windows 8.1 — and a Go binary has no such manifest, so a
// runner using it would report every Windows 11 machine in the fleet as
// Windows 8.
func osVersion() (string, bool) {
	v := windows.RtlGetVersion()
	if v == nil || v.MajorVersion == 0 {
		return "", false
	}
	const verNTWorkstation = 1
	return windowsOSName(v.MajorVersion, v.MinorVersion, v.BuildNumber,
		v.ProductType == verNTWorkstation), true
}
