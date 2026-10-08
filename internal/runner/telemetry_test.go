package runner_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/runner"
)

// The reading has to actually reach Elk, in the shape Elk stores. These tests
// drive the whole loop against the httptest fake and read what came off the
// wire, which is the only assertion that cannot be satisfied by a payload
// nobody sends.

func TestEveryBeatCarriesTheFleetReading(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{Machine: func(string) runner.Machine {
		return runner.Machine{
			OSKind: "darwin", Arch: "arm64", CPUCount: 12,
			OS: "macOS 26.1", OSKnown: true,
			CPUModel: "Apple M2 Max", CPUModelKnown: true,
			Load1: 4.1, Load1Known: true,
			TotalRAM: 34359738368, TotalRAMKnown: true,
			TotalDisk: 1067009867776, TotalDiskKnown: true,
			Headroom: runner.Headroom{
				FreeRAM: 15246002176, RAMKnown: true,
				FreeDisk: 309237645312, DiskKnown: true,
			},
		}
	}}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	beats := h.elk.CallsTo("heartbeat_executor")
	if len(beats) == 0 {
		t.Fatalf("no heartbeat at all\nlog:\n%s", h.log)
	}
	args := beats[0].Args

	host, ok := args["host"].(map[string]any)
	if !ok {
		t.Fatalf("host arrived as %T, not the object Elk requires: %#v", args["host"], args)
	}
	for key, want := range map[string]any{
		"os": "macOS 26.1", "os_kind": "darwin", "arch": "arm64",
		"cpu_model": "Apple M2 Max", "cpu_count": float64(12), "load1": 4.1,
		"mem_total_bytes": float64(34359738368), "mem_free_bytes": float64(15246002176),
		"disk_total_bytes": float64(1067009867776), "disk_free_bytes": float64(309237645312),
	} {
		if host[key] != want {
			t.Errorf("host[%q] = %v, want %v", key, host[key], want)
		}
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		if host["machine_name"] != name {
			t.Errorf("machine_name = %v, want %q", host["machine_name"], name)
		}
	}
	if host["preflight_state"] != "ready" {
		t.Errorf("preflight_state = %v, want ready", host["preflight_state"])
	}
	rn, ok := host["runner"].(map[string]any)
	if !ok {
		t.Fatalf("host.runner = %T", host["runner"])
	}
	if rn["name"] != "rein" {
		t.Errorf("host.runner.name = %v", rn["name"])
	}
	// 14.2 GiB free is three runs' worth of the 4 GiB budget, so the cap of
	// four is held down to three — and the reading says by what.
	if rn["slots_total"] != float64(3) || rn["slots_limited_by"] != "memory" {
		t.Errorf("host.runner = %v", rn)
	}

	session, ok := args["session"].(map[string]any)
	if !ok {
		t.Fatalf("session arrived as %T, not an object", args["session"])
	}
	// The first beat is before the claim, so this queue is genuinely idle.
	if session["state"] != "idle" {
		t.Errorf("session.state = %v on the beat before the claim", session["state"])
	}
}

// TestTelemetryOmitsWhatTheMachineCouldNotMeasure, end to end. A key that
// could not be measured must be ABSENT on the wire: Elk renders "could not
// measure" and "zero" differently and they mean opposite things.
func TestTelemetryOmitsWhatTheMachineCouldNotMeasure(t *testing.T) {
	h := newHarness(t)
	if err := h.run(runner.Options{Machine: func(string) runner.Machine {
		// A Windows box: no load average anywhere, and a disk this runner
		// could not stat.
		return runner.Machine{
			OSKind: "windows", Arch: "amd64", CPUCount: 16,
			OS: "Windows 11 (build 22631)", OSKnown: true,
			TotalRAM: 68719476736, TotalRAMKnown: true,
			Headroom: runner.Headroom{FreeRAM: 40000000000, RAMKnown: true},
		}
	}}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	host := h.elk.CallsTo("heartbeat_executor")[0].Args["host"].(map[string]any)
	for _, key := range []string{"load1", "disk_total_bytes", "disk_free_bytes", "cpu_model"} {
		if v, ok := host[key]; ok {
			t.Errorf("%q was sent as %v by a machine that could not measure it", key, v)
		}
	}
	// And what WAS measured is there, so this is not a test that passes
	// because nothing was sent.
	if host["os"] != "Windows 11 (build 22631)" || host["cpu_count"] != float64(16) {
		t.Errorf("host = %v", host)
	}
}

func TestTelemetryCanBeSwitchedOff(t *testing.T) {
	h := newHarness(t)
	if err := h.run(runner.Options{TelemetryOff: true}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	beats := h.elk.CallsTo("heartbeat_executor")
	if len(beats) == 0 {
		t.Fatal("the beat itself stopped; switching the reading off must not take the machine offline")
	}
	for _, key := range []string{"host", "session"} {
		if _, ok := beats[0].Args[key]; ok {
			t.Errorf("%q was sent with telemetry switched off: %v", key, beats[0].Args)
		}
	}
}

// TestTheReadingIsWellUnderElksCap, measured on the wire rather than on the
// struct: the cap is on what Elk receives.
func TestTheReadingIsWellUnderElksCap(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	args := h.elk.CallsTo("heartbeat_executor")[0].Args
	n := 0
	for _, key := range []string{"host", "session"} {
		blob, err := json.Marshal(args[key])
		if err != nil {
			t.Fatal(err)
		}
		n += len(blob)
	}
	if n > 2048 {
		t.Errorf("a real reading is %d bytes on the wire; it should be a few hundred", n)
	}
	t.Logf("reading on the wire: %d bytes", n)
}

// TestTheHeartbeatReplyIsSurfaced. A reading Elk could not store comes back as
// a note in the reply rather than as an error, so the only way to learn that
// telemetry is being refused is to read what the server said.
func TestTheHeartbeatReplyIsSurfaced(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("heartbeat_executor", `Heartbeat recorded for queue "`+queue+
		`". 0 runs waiting. Telemetry not stored: host must be a JSON object.`)

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if !strings.Contains(h.log.String(), "Telemetry not stored") {
		t.Errorf("Elk's complaint never reached the log:\n%s", h.log)
	}
}

func TestHeartbeatCarriesSafeSetupFailure(t *testing.T) {
	h := newHarness(t)
	h.agent.PreflightErr = errors.New("auth status failed: sensitive vendor output")
	if err := h.run(runner.Options{}); err != nil {
		t.Fatal(err)
	}
	beats := h.elk.CallsTo("heartbeat_executor")
	if len(beats) == 0 {
		t.Fatal("no heartbeat")
	}
	host := beats[0].Args["host"].(map[string]any)
	if host["preflight_state"] != "Agent authentication required" {
		t.Errorf("setup state = %v", host["preflight_state"])
	}
}
