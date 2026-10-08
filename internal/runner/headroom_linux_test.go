package runner

import "testing"

const memInfoSample = `MemTotal:       16336268 kB
MemFree:          210268 kB
MemAvailable:    9218844 kB
Buffers:          104432 kB
Cached:          8100224 kB
SwapTotal:       2097148 kB
HugePages_Total:       0
`

func TestParseMemInfoPrefersMemAvailable(t *testing.T) {
	got, ok := parseMemInfo(memInfoSample)
	if !ok {
		t.Fatal("/proc/meminfo was not parsed")
	}
	if want := uint64(9218844) * 1024; got != want {
		t.Fatalf("free RAM = %d, want MemAvailable in bytes (%d)", got, want)
	}
}

func TestParseMemInfoFallsBackToFreePlusCaches(t *testing.T) {
	const old = `MemTotal:       16336268 kB
MemFree:          210268 kB
Buffers:          104432 kB
Cached:          8100224 kB
`
	got, ok := parseMemInfo(old)
	if !ok {
		t.Fatal("a pre-3.14 /proc/meminfo was not parsed")
	}
	if want := uint64(210268+104432+8100224) * 1024; got != want {
		t.Fatalf("free RAM = %d, want %d", got, want)
	}
}

func TestParseMemInfoRefusesNonsense(t *testing.T) {
	if _, ok := parseMemInfo("this is not /proc/meminfo\n"); ok {
		t.Error("parsed nonsense as a measurement")
	}
}

func TestParseMemTotal(t *testing.T) {
	got, ok := parseMemTotal(memInfoSample)
	if !ok {
		t.Fatal("MemTotal was not parsed")
	}
	if want := uint64(16336268) * 1024; got != want {
		t.Fatalf("total RAM = %d, want %d", got, want)
	}
	// A file with no MemTotal is not a /proc/meminfo, and reporting zero for
	// it would render as a machine with no memory rather than as one that
	// could not be measured.
	if _, ok := parseMemTotal("MemFree: 100 kB\n"); ok {
		t.Error("a meminfo with no MemTotal was accepted")
	}
}

func TestParseLoadAvg(t *testing.T) {
	got, ok := parseLoadAvg("0.52 0.58 0.59 2/1234 56789\n")
	if !ok {
		t.Fatal("/proc/loadavg was not parsed")
	}
	if got != 0.52 {
		t.Fatalf("load1 = %v, want the FIRST field, 0.52", got)
	}
	// A completely idle box is a real reading and must not read as a failure.
	if v, ok := parseLoadAvg("0.00 0.01 0.05 1/200 3\n"); !ok || v != 0 {
		t.Errorf("an idle box parsed as %v, %v; zero load is a measurement", v, ok)
	}
	if _, ok := parseLoadAvg("not a load average\n"); ok {
		t.Error("nonsense parsed as a load average")
	}
}

// TestParseCPUInfoX86 is an ordinary hosted-runner /proc/cpuinfo, trimmed to
// one core's block.
func TestParseCPUInfoX86(t *testing.T) {
	const sample = `processor	: 0
vendor_id	: AuthenticAMD
cpu family	: 25
model		: 1
model name	: AMD EPYC 7763 64-Core Processor
stepping	: 1
cpu MHz		: 2445.406

processor	: 1
model name	: AMD EPYC 7763 64-Core Processor
`
	got, ok := parseCPUInfo(sample)
	if !ok {
		t.Fatal("/proc/cpuinfo was not parsed")
	}
	if got != "AMD EPYC 7763 64-Core Processor" {
		t.Fatalf("cpu model = %q", got)
	}
}

// TestParseCPUInfoARM is the case a naive `model name` grep gets wrong: arm64
// has no such key at all, and what a person recognises comes from the board's
// firmware under `Model` or `Hardware`.
func TestParseCPUInfoARM(t *testing.T) {
	const sample = `processor	: 0
BogoMIPS	: 50.00
Features	: fp asimd evtstrm aes
CPU implementer	: 0x41
CPU architecture: 8
CPU variant	: 0x1
CPU part	: 0xd0c
CPU revision	: 1

Hardware	: BCM2835
Model		: Raspberry Pi 5 Model B Rev 1.0
`
	got, ok := parseCPUInfo(sample)
	if !ok {
		t.Fatal("an arm64 /proc/cpuinfo was not parsed")
	}
	if got != "Raspberry Pi 5 Model B Rev 1.0" {
		t.Fatalf("cpu model = %q, want the readable Model line", got)
	}

	// And an arm64 kernel offering neither says nothing, rather than
	// "0x41 0xd0c", which is worse than nothing in a fleet listing.
	const bare = `processor	: 0
CPU implementer	: 0x41
CPU part	: 0xd0c
`
	if v, ok := parseCPUInfo(bare); ok {
		t.Errorf("parsed %q out of hex identifiers", v)
	}
}

func TestParseOSRelease(t *testing.T) {
	const ubuntu = `PRETTY_NAME="Ubuntu 24.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
ID=ubuntu
`
	got, ok := parseOSRelease(ubuntu)
	if !ok || got != "Ubuntu 24.04.1 LTS" {
		t.Fatalf("os-release = %q, %v", got, ok)
	}

	// PRETTY_NAME is optional in the spec even though almost nothing omits it.
	const bare = `NAME="Alpine Linux"
VERSION_ID=3.20.3
ID=alpine
`
	got, ok = parseOSRelease(bare)
	if !ok || got != "Alpine Linux 3.20.3" {
		t.Fatalf("os-release without PRETTY_NAME = %q, %v", got, ok)
	}

	if _, ok := parseOSRelease("# just a comment\n"); ok {
		t.Error("a file naming no distribution was accepted")
	}
}

// TestThisLinuxBoxAnswersEveryReading is the one place the real files are
// read. It asserts that they answer, not what they answer.
func TestThisLinuxBoxAnswersEveryReading(t *testing.T) {
	if v, ok := totalRAM(); !ok || v == 0 {
		t.Errorf("MemTotal did not answer on Linux (%d, %v)", v, ok)
	} else {
		t.Logf("total RAM: %s", humanBytes(v))
	}
	if v, ok := loadAvg1(); !ok {
		t.Error("/proc/loadavg did not answer on Linux")
	} else {
		t.Logf("load1: %.2f", v)
	}
	// The last two are best-effort even here: a container filesystem may hide
	// /proc/cpuinfo or ship no /etc/os-release, and degrading is the point.
	if v, ok := cpuModel(); ok {
		t.Logf("CPU: %s", v)
	} else {
		t.Log("no CPU model on this box — the key is omitted, not zeroed")
	}
	if v, ok := osVersion(); ok {
		t.Logf("OS: %s", v)
	} else {
		t.Log("no OS name on this box — the key is omitted")
	}
}

func TestFreeRAMOnThisLinuxBox(t *testing.T) {
	got, ok := freeRAM()
	if !ok {
		t.Fatal("/proc/meminfo did not answer on Linux")
	}
	if got == 0 {
		t.Fatal("free RAM measured as exactly zero")
	}
	t.Logf("free RAM: %s", humanBytes(got))
}
