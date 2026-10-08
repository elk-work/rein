package runner

import (
	"context"
	"encoding/binary"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// freeRAM on macOS, and the reason it is not one number read from a sysctl.
//
// **macOS keeps free memory near zero on purpose.** `vm.page_free_count` on a
// 24 GB Mac with 5 GB genuinely spare reads a few thousand pages — under
// 100 MB — because anything not in use is holding a file cache. A runner that
// took that as its measurement would clamp itself to one slot forever, on
// every Mac, which is the same mistake as a static cap with a worse
// explanation.
//
// What is actually available is the free list plus the pages the kernel will
// hand over without writing anything to disk: inactive, speculative and
// purgeable. That is psutil's definition of macOS "available" and it is close
// to what Activity Monitor shows as not-in-use.
//
// The inactive count is only in the Mach `host_statistics64` call, which needs
// cgo — and cgo is exactly what release.yml turns off so the darwin binaries
// cross-build. `/usr/bin/vm_stat` reports the same counters, is present on
// every macOS, needs no entitlement and no TCC grant (so it works from a
// launchd agent), and costs a few milliseconds once a tick.
func freeRAM() (uint64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output()
	if err != nil {
		return 0, false
	}
	return parseVMStat(string(out))
}

// parseVMStat reads the counters out of vm_stat's output. Split out so the
// arithmetic is testable without depending on what this machine is doing.
//
//	Mach Virtual Memory Statistics: (page size of 16384 bytes)
//	Pages free:                                   4438.
//	Pages inactive:                             294061.
func parseVMStat(out string) (uint64, bool) {
	var pageSize uint64
	counts := map[string]uint64{}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			// "... (page size of 16384 bytes)"
			if _, rest, ok := strings.Cut(line, "page size of "); ok {
				if n, _, ok := strings.Cut(rest, " "); ok {
					if v, err := strconv.ParseUint(n, 10, 64); err == nil {
						pageSize = v
					}
				}
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSuffix(strings.TrimSpace(value), ".")
		v, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			continue
		}
		counts[strings.TrimSpace(key)] = v
	}

	free, ok := counts["Pages free"]
	if pageSize == 0 || !ok {
		return 0, false
	}
	// The reclaimable set. Missing keys count as zero: an older or newer
	// vm_stat that drops one of these should make the answer conservative,
	// not make it unavailable.
	pages := free + counts["Pages inactive"] + counts["Pages speculative"] + counts["Pages purgeable"]
	return pages * pageSize, true
}

// The rest of what a Mac can say about itself, for the fleet reading — and why
// none of it is a second `vm_stat`.
//
// The free-memory number above needs a subprocess because the counter it wants
// (inactive pages) lives only in `host_statistics64`, which needs cgo.
// Everything below is a plain sysctl, which golang.org/x/sys/unix reaches
// through the syscall directly: no cgo, no fork, microseconds. So this half is
// measured the cheap way and only the half with no cheap way pays for a
// process.
//
// Nothing here guesses. A sysctl missing on some future macOS returns
// not-known and the key is omitted from the reading rather than sent as zero —
// Elk renders "could not measure" and "zero" differently and the whole design
// depends on telling them apart.

// totalRAM is `hw.memsize`: the physical memory installed, in bytes.
//
// It is constant for the life of the boot and is still read every tick,
// because the read costs nothing and a cache would be state to get wrong.
func totalRAM() (uint64, bool) {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil || v == 0 {
		return 0, false
	}
	return v, true
}

// cpuModel is `machdep.cpu.brand_string` — "Apple M2 Max" on Apple silicon,
// the Intel brand string on an Intel Mac.
//
// Deliberately NOT `hw.model`, which is the board identifier: "Mac14,6". That
// is a fine thing to know and a useless thing to show the person reading a
// fleet listing, which is the only thing this reading is for.
func cpuModel() (string, bool) {
	s, err := unix.Sysctl("machdep.cpu.brand_string")
	if err != nil {
		return "", false
	}
	if s = strings.TrimSpace(s); s == "" {
		return "", false
	}
	return s, true
}

// osVersion is "macOS " plus `kern.osproductversion` — "macOS 26.1".
//
// `kern.osrelease` is the Darwin kernel version (25.5.0), which nobody outside
// this file thinks of as the version of their Mac, and there is no sysctl for
// the product NAME — so the one word is supplied here. `sw_vers` would give
// both, and would be a second subprocess for a string.
func osVersion() (string, bool) {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return "", false
	}
	if v = strings.TrimSpace(v); v == "" {
		return "", false
	}
	return "macOS " + v, true
}

// loadAvg1 is the one-minute load average, out of `vm.loadavg`.
//
// The sysctl answers with the kernel's `struct loadavg` rather than with a
// float: three fixed-point averages and the scale to divide them by. Hence the
// raw read, and the decode below — the only part of this file with any risk in
// it, and so split out and tested against fixed bytes.
func loadAvg1() (float64, bool) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		return 0, false
	}
	return parseLoadAvg(raw)
}

// parseLoadAvg decodes darwin's `struct loadavg`:
//
//	struct loadavg { fixpt_t ldavg[3]; long fscale; };
//
// `fixpt_t` is uint32 and `long` is 64-bit on every darwin target Rein ships
// to, so the struct is 12 bytes of averages, 4 bytes of padding to align the
// long, and 8 bytes of scale — 24 in all. Both darwin architectures are
// little-endian; Go has never supported a big-endian one.
//
// A real reading from a Mac at load 6.15:
//
//	34 31 00 00  d7 46 00 00  20 32 00 00  00 00 00 00  00 08 00 00 00 00 00 00
//	 ldavg[0]      ldavg[1]     ldavg[2]      (padding)        fscale = 2048
//
// The sanity checks are not defensive noise. A struct that changed shape under
// us would otherwise be reported as a plausible-looking load average, and a
// wrong number here is worse than no number: Elk renders an absent one
// honestly, as "could not measure".
func parseLoadAvg(raw []byte) (float64, bool) {
	const size = 24
	if len(raw) < size {
		return 0, false
	}
	scale := binary.LittleEndian.Uint64(raw[16:24])
	if scale == 0 {
		return 0, false
	}
	load := float64(binary.LittleEndian.Uint32(raw[0:4])) / float64(scale)
	// No machine has a one-minute load average in the thousands, and a
	// misread struct produces exactly that.
	if load < 0 || load > 10000 {
		return 0, false
	}
	return load, true
}
