package runner

import (
	"strings"
	"testing"
)

// Real output from `vm_stat` on macOS 15, trimmed to the lines that matter and
// the ones that must be ignored.
const vmStatSample = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                4438.
Pages active:                            315956.
Pages inactive:                          294061.
Pages speculative:                         2656.
Pages throttled:                              0.
Pages wired down:                        254756.
Pages purgeable:                             34.
"Translation faults":                3489472106.
Pages copy-on-write:                   89340512.
File-backed pages:                       220990.
Anonymous pages:                         391683.
`

func TestParseVMStat(t *testing.T) {
	got, ok := parseVMStat(vmStatSample)
	if !ok {
		t.Fatal("vm_stat output was not parsed")
	}
	// free + inactive + speculative + purgeable, times the page size.
	const wantPages = 4438 + 294061 + 2656 + 34
	want := uint64(wantPages) * 16384
	if got != want {
		t.Fatalf("free RAM = %d, want %d", got, want)
	}
	// The point of using the reclaimable set rather than "Pages free": free
	// alone is 69 MiB on a Mac with nearly 5 GiB spare, which would clamp
	// every Mac to one slot forever.
	if got < 4<<30 {
		t.Errorf("free RAM = %s, and this sample has ~4.6 GiB reclaimable", humanBytes(got))
	}
}

func TestParseVMStatRefusesNonsense(t *testing.T) {
	for _, in := range []string{
		"",
		"not vm_stat output at all\n",
		// A header with no page size is unusable: the counts are in pages.
		"Mach Virtual Memory Statistics:\nPages free: 100.\n",
	} {
		if _, ok := parseVMStat(in); ok {
			t.Errorf("parsed %q as a measurement", in)
		}
	}
}

// TestParseLoadAvg works on the real bytes `sysctl vm.loadavg` returned on a
// Mac at load 6.15 — three fixed-point uint32 averages, four bytes of
// padding, and a 64-bit scale of 2048.
func TestParseLoadAvg(t *testing.T) {
	raw := []byte{
		0x34, 0x31, 0x00, 0x00, // ldavg[0] = 12596
		0xd7, 0x46, 0x00, 0x00, // ldavg[1] = 18135
		0x20, 0x32, 0x00, 0x00, // ldavg[2] = 12832
		0x00, 0x00, 0x00, 0x00, // padding, to align the long
		0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // fscale = 2048
	}
	got, ok := parseLoadAvg(raw)
	if !ok {
		t.Fatal("a real vm.loadavg was not parsed")
	}
	if want := 12596.0 / 2048.0; got != want {
		t.Fatalf("load1 = %v, want %v", got, want)
	}
	// The one-minute average is the FIRST of the three. Reading the wrong slot
	// would still produce a plausible number, which is why this is asserted
	// rather than left to the arithmetic above.
	if got > 6.2 || got < 6.1 {
		t.Errorf("load1 = %v; the sample was taken at 6.15", got)
	}
}

func TestParseLoadAvgRefusesNonsense(t *testing.T) {
	for name, raw := range map[string][]byte{
		"empty":         {},
		"truncated":     {0x34, 0x31, 0x00, 0x00},
		"zero fscale":   make([]byte, 24),
		"absurd result": append(append([]byte{0xff, 0xff, 0xff, 0xff}, make([]byte, 12)...), 1, 0, 0, 0, 0, 0, 0, 0),
	} {
		if v, ok := parseLoadAvg(raw); ok {
			t.Errorf("%s parsed as a load average of %v", name, v)
		}
	}
}

// TestThisMacAnswersEveryReading is the one place the real sysctls run. It
// asserts that they answer, never what they answer: a test that expected
// "Apple M4 Pro" would fail on every other Mac in the fleet.
func TestThisMacAnswersEveryReading(t *testing.T) {
	if v, ok := totalRAM(); !ok || v == 0 {
		t.Errorf("hw.memsize did not answer on a Mac (%d, %v)", v, ok)
	} else {
		t.Logf("total RAM: %s", humanBytes(v))
	}
	if v, ok := cpuModel(); !ok || v == "" {
		t.Error("machdep.cpu.brand_string did not answer on a Mac")
	} else {
		t.Logf("CPU: %s", v)
	}
	if v, ok := osVersion(); !ok || !strings.HasPrefix(v, "macOS ") {
		t.Errorf("osVersion = %q, %v; want a macOS version", v, ok)
	} else {
		t.Logf("OS: %s", v)
	}
	if v, ok := loadAvg1(); !ok {
		t.Error("vm.loadavg did not answer on a Mac")
	} else {
		t.Logf("load1: %.2f", v)
	}
}

func TestFreeRAMOnThisMac(t *testing.T) {
	got, ok := freeRAM()
	if !ok {
		t.Fatal("vm_stat did not answer on a Mac")
	}
	if got == 0 {
		t.Fatal("free RAM measured as exactly zero")
	}
	t.Logf("free RAM: %s", humanBytes(got))
}
