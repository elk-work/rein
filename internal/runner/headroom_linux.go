package runner

import (
	"os"
	"strconv"
	"strings"
)

// freeRAM on Linux is `MemAvailable` from /proc/meminfo — the kernel's own
// estimate of what a new workload could get without swapping, which is exactly
// the question being asked here and is more honest than MemFree for the same
// reason it is on macOS: MemFree excludes the reclaimable page cache.
//
// MemAvailable has been in /proc/meminfo since Linux 3.14. The fallback is for
// a kernel or a container filesystem that does not present it.
func freeRAM() (uint64, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	return parseMemInfo(string(data))
}

// meminfoFields reads /proc/meminfo into a map, in bytes:
//
//	MemTotal:       16336268 kB
//	MemFree:          210268 kB
//	MemAvailable:    9218844 kB
//
// Values are in kB, which is the one thing about this file that never changes.
// Split out from [parseMemInfo] so that the free half and the total half come
// from one parse of one read rather than two of each.
func meminfoFields(data string) map[string]uint64 {
	fields := map[string]uint64{}
	for _, line := range strings.Split(data, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		parts := strings.Fields(rest)
		if len(parts) == 0 {
			continue
		}
		v, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			continue
		}
		if len(parts) > 1 && strings.EqualFold(parts[1], "kB") {
			v *= 1024
		}
		fields[key] = v
	}
	return fields
}

// parseMemInfo pulls the availability estimate out of /proc/meminfo.
func parseMemInfo(data string) (uint64, bool) {
	fields := meminfoFields(data)
	if v, ok := fields["MemAvailable"]; ok {
		return v, true
	}
	// Pre-3.14, or a filesystem that hides it: free plus the two caches the
	// kernel would drop first. Coarser, and in the right direction.
	if free, ok := fields["MemFree"]; ok {
		return free + fields["Buffers"] + fields["Cached"], true
	}
	return 0, false
}

// The rest of what a Linux box can say about itself, for the fleet reading.
//
// All of it comes out of /proc and /etc, which is the same standard the free
// half above is held to: files the kernel or the distribution publishes, read
// and parsed here, with a parser split out for every one of them so the tests
// assert against fixed sample text rather than against whatever this machine
// happens to be. A file that is missing or shaped differently returns
// not-known, and the key is omitted from the reading rather than sent as zero.

// totalRAM is `MemTotal` from /proc/meminfo. Unlike the free half there is no
// fallback and none is wanted: a /proc/meminfo without MemTotal is not a
// /proc/meminfo.
func totalRAM() (uint64, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	return parseMemTotal(string(data))
}

func parseMemTotal(data string) (uint64, bool) {
	v, ok := meminfoFields(data)["MemTotal"]
	if !ok || v == 0 {
		return 0, false
	}
	return v, true
}

// loadAvg1 is the first field of /proc/loadavg.
func loadAvg1() (float64, bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	return parseLoadAvg(string(data))
}

// parseLoadAvg reads the one-minute average out of /proc/loadavg:
//
//	0.52 0.58 0.59 2/1234 56789
//
// Already a decimal string, unlike darwin's fixed-point struct — the kernel
// does the division here.
func parseLoadAvg(data string) (float64, bool) {
	parts := strings.Fields(data)
	if len(parts) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(parts[0], 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// cpuModel reads /proc/cpuinfo.
func cpuModel() (string, bool) {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", false
	}
	return parseCPUInfo(string(data))
}

// parseCPUInfo finds the most human-readable CPU name /proc/cpuinfo offers,
// which is a different key depending on the architecture.
//
// On x86 it is `model name`:
//
//	model name	: AMD EPYC 7763 64-Core Processor
//
// On arm64 there is no such key at all — the kernel publishes `CPU implementer`
// and `CPU part` as hex numbers, and it is the board's firmware that supplies
// anything readable, under `Hardware` on older kernels or `Model` on a
// device-tree machine. So the keys are tried in order of how much they mean to
// a person, and an arm64 box that offers none of them reports no CPU model
// rather than "0x41 0xd0c", which would be worse than nothing in a fleet
// listing.
func parseCPUInfo(data string) (string, bool) {
	want := []string{"model name", "Model", "Hardware", "cpu model"}
	found := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		// First occurrence wins: every core repeats the same block.
		if _, seen := found[key]; !seen {
			found[key] = value
		}
	}
	for _, k := range want {
		if v, ok := found[k]; ok {
			return v, true
		}
	}
	return "", false
}

// osVersion is the distribution's own name for itself.
func osVersion() (string, bool) {
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		if v, ok := parseOSRelease(string(data)); ok {
			return v, true
		}
	}
	// No /etc/os-release: a container built from scratch, or something very
	// old. The kernel release is a poor substitute for a distribution name and
	// a much better answer than nothing.
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			return "Linux " + v, true
		}
	}
	return "", false
}

// parseOSRelease pulls PRETTY_NAME out of /etc/os-release:
//
//	PRETTY_NAME="Ubuntu 24.04.1 LTS"
//	NAME="Ubuntu"
//	VERSION_ID="24.04"
//
// The file is shell-quotable, so values may or may not be quoted; NAME plus
// VERSION_ID is the fallback for a file that omits PRETTY_NAME, which is
// allowed by the spec even though almost nothing does it.
func parseOSRelease(data string) (string, bool) {
	fields := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if value != "" {
			fields[strings.TrimSpace(key)] = value
		}
	}
	if v, ok := fields["PRETTY_NAME"]; ok {
		return v, true
	}
	if name, ok := fields["NAME"]; ok {
		if ver, ok := fields["VERSION_ID"]; ok {
			return name + " " + ver, true
		}
		return name, true
	}
	return "", false
}
