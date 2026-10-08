package elk

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

// TestHostReadingOmitsWhatWasNotMeasured is the rule the whole feature rests
// on: Elk renders "could not measure" and "zero" differently, so a key that
// could not be measured must be ABSENT and a key measured as zero must be
// PRESENT. A struct with plain numbers and omitempty gets this exactly
// backwards on the second half.
func TestHostReadingOmitsWhatWasNotMeasured(t *testing.T) {
	// A platform that knows only what the runtime compiled in — the reading a
	// runner on an unsupported OS produces.
	bare := &HostReading{OSKind: "plan9", Arch: "amd64"}
	blob := marshal(t, bare)
	for _, key := range []string{
		"os", "cpu_model", "cpu_count", "load1",
		"mem_total_bytes", "mem_free_bytes", "disk_total_bytes", "disk_free_bytes",
	} {
		if strings.Contains(blob, `"`+key+`"`) {
			t.Errorf("%q is present in a reading that measured nothing: %s", key, blob)
		}
	}
	if !strings.Contains(blob, `"os_kind":"plan9"`) {
		t.Errorf("os_kind was dropped: %s", blob)
	}

	// And the opposite: a machine genuinely at zero load, with genuinely no
	// free disk, must say so rather than look unmeasured.
	measured := &HostReading{
		OSKind: "linux", Arch: "arm64",
		Load1:        ptr(0.0),
		MemFreeBytes: ptr(uint64(0)),
	}
	blob = marshal(t, measured)
	if !strings.Contains(blob, `"load1":0`) {
		t.Errorf("a measured load of zero was dropped — it reads as unmeasured: %s", blob)
	}
	if !strings.Contains(blob, `"mem_free_bytes":0`) {
		t.Errorf("a measured zero bytes free was dropped: %s", blob)
	}
}

// TestRunnerReadingAlwaysReportsSlotsUsed guards the one field where zero is
// not only legitimate but the commonest value there is.
func TestRunnerReadingAlwaysReportsSlotsUsed(t *testing.T) {
	blob := marshal(t, &RunnerReading{Name: "rein", SlotsTotal: 4, SlotsUsed: 0})
	if !strings.Contains(blob, `"slots_used":0`) {
		t.Errorf("an idle runner reported no slots_used at all: %s", blob)
	}
	// Nothing is holding it down, and that is not a missing measurement.
	if strings.Contains(blob, "slots_limited_by") {
		t.Errorf("slots_limited_by is present when nothing limits the count: %s", blob)
	}
}

// TestSessionReadingDistinguishesAnEmptyInventoryFromNoAnswer is why
// MCPServers is a pointer to a slice. A Rein-driven Claude run genuinely has
// no MCP servers — `--strict-mcp-config` with an explicit empty set, after
// ark:rein#21 — and reporting that honestly is different from never having
// asked.
func TestSessionReadingDistinguishesAnEmptyInventoryFromNoAnswer(t *testing.T) {
	asked := &SessionReading{State: "running", MCPServers: &[]MCPServerReading{}}
	if blob := marshal(t, asked); !strings.Contains(blob, `"mcp_servers":[]`) {
		t.Errorf("an empty inventory was dropped instead of reported: %s", blob)
	}
	never := &SessionReading{State: "running"}
	if blob := marshal(t, never); strings.Contains(blob, "mcp_servers") {
		t.Errorf("mcp_servers is present on a session that never reported one: %s", blob)
	}
}

func TestRateLimitWindowKeepsAFreshlyResetWindow(t *testing.T) {
	blob := marshal(t, RateLimitWindow{Utilization: 0})
	if !strings.Contains(blob, `"utilization":0`) {
		t.Errorf("a window that has just reset reads as unreported: %s", blob)
	}
}

func TestRFC3339DropsTheZeroTime(t *testing.T) {
	if got := RFC3339(time.Time{}); got != "" {
		t.Errorf("the zero time rendered as %q; it must be an omitted key, not 1 January year one", got)
	}
	want := "2026-08-30T12:00:00Z"
	if got := RFC3339(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)); got != want {
		t.Errorf("RFC3339 = %q, want %q", got, want)
	}
}

// TestEmptyHalvesAreNotSent covers Elk's third rule: an empty object counts as
// no reading and stores nothing, so sending one is a round trip for nothing.
func TestEmptyHalvesAreNotSent(t *testing.T) {
	args := map[string]any{}
	Telemetry{}.put(args)
	if len(args) != 0 {
		t.Fatalf("an empty telemetry put %v on the call", args)
	}

	args = map[string]any{}
	Telemetry{Host: &HostReading{}, Session: &SessionReading{}}.put(args)
	if len(args) != 0 {
		t.Fatalf("two empty halves were sent as %v", args)
	}

	args = map[string]any{}
	Telemetry{Session: &SessionReading{State: "idle"}}.put(args)
	if _, ok := args["session"]; !ok {
		t.Error("a session reading with a state was not sent")
	}
	if _, ok := args["host"]; ok {
		t.Error("a nil host half was sent anyway")
	}
}

// TestFitTrimsInOrder pins the cap and the order things are given up in: the
// MCP inventory first, then the rest of the session, then the host. A reading
// the server refuses is a hole in the fleet view; a trimmed one is not.
func TestFitTrimsInOrder(t *testing.T) {
	// A full reading is nowhere near the cap and must lose nothing.
	small := Telemetry{
		Host:    &HostReading{OSKind: "darwin", Arch: "arm64", CPUCount: 12},
		Session: &SessionReading{State: "running", MCPServers: &[]MCPServerReading{{Name: "elk"}}},
	}
	dropped, err := small.Fit()
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("an ordinary reading lost %v", dropped)
	}

	// A session that has somehow accumulated a vast MCP inventory loses the
	// inventory and keeps everything else.
	huge := make([]MCPServerReading, 400)
	for i := range huge {
		huge[i] = MCPServerReading{Name: strings.Repeat("server", 8), Status: "connected"}
	}
	big := Telemetry{
		Host:    &HostReading{OSKind: "darwin", Arch: "arm64"},
		Session: &SessionReading{State: "running", RunID: "arun-1", MCPServers: &huge},
	}
	dropped, err = big.Fit()
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "mcp_servers" {
		t.Fatalf("dropped %v, want just the inventory", dropped)
	}
	if big.Session == nil || big.Session.RunID != "arun-1" {
		t.Error("the session half was thrown away when trimming its inventory was enough")
	}
	if n, _ := big.Size(); n > MaxTelemetryBytes {
		t.Errorf("still %d bytes after trimming, over the %d cap", n, MaxTelemetryBytes)
	}

	// A session too big even without an inventory loses the session and keeps
	// the host, which is the half that is always worth having.
	bulk := Telemetry{
		Host:    &HostReading{OSKind: "darwin", Arch: "arm64"},
		Session: &SessionReading{State: "running", RunID: strings.Repeat("x", MaxTelemetryBytes+1)},
	}
	dropped, err = bulk.Fit()
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if len(dropped) != 2 || dropped[0] != "mcp_servers" || dropped[1] != "session" {
		t.Fatalf("dropped %v, want the inventory then the session", dropped)
	}
	if bulk.Host == nil {
		t.Error("the host half went too, and it was well inside the cap")
	}
}

func TestSizeCountsBothHalves(t *testing.T) {
	host := &HostReading{OSKind: "darwin", Arch: "arm64"}
	session := &SessionReading{State: "idle"}

	hostOnly, err := Telemetry{Host: host}.Size()
	if err != nil {
		t.Fatal(err)
	}
	both, err := Telemetry{Host: host, Session: session}.Size()
	if err != nil {
		t.Fatal(err)
	}
	if both <= hostOnly {
		t.Errorf("size with both halves (%d) is not bigger than with one (%d)", both, hostOnly)
	}
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding %T: %v", v, err)
	}
	return string(blob)
}
