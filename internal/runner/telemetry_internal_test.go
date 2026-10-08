package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/runlog"
)

// testRunner is the smallest Runner that can build a reading: a machine
// measurement it was given, a slot gate, and nothing else.
func testRunner(m Machine) *Runner {
	r := &Runner{
		opts: Options{
			MaxConcurrent: 4,
			Version:       "v0.1.1",
			Machine:       func(string) Machine { return m },
			Headroom:      func(string) Headroom { return m.Headroom },
		},
		sessions: newSessionRegistry(),
	}
	r.gate = &slotGate{hard: 4, probe: r.opts.Headroom, logf: func(string, ...any) {}, retry: time.Second}
	return r
}

// fullMachine is what a Mac reports: everything measurable, measured.
func fullMachine() Machine {
	return Machine{
		OSKind: "darwin", Arch: "arm64", CPUCount: 12,
		OS: "macOS 26.1", OSKnown: true,
		CPUModel: "Apple M2 Max", CPUModelKnown: true,
		Load1: 4.1, Load1Known: true,
		TotalRAM: 34359738368, TotalRAMKnown: true,
		TotalDisk: 1067009867776, TotalDiskKnown: true,
		Headroom: Headroom{
			FreeRAM: 15246002176, RAMKnown: true,
			FreeDisk: 309237645312, DiskKnown: true,
		},
	}
}

func TestHostReadingFromAFullMeasurement(t *testing.T) {
	r := testRunner(fullMachine())
	h := r.hostReading()

	if h.OS != "macOS 26.1" || h.OSKind != "darwin" || h.Arch != "arm64" {
		t.Errorf("os fields = %+v", h)
	}
	if h.CPUModel != "Apple M2 Max" || h.CPUCount != 12 {
		t.Errorf("cpu fields = %+v", h)
	}
	if h.Load1 == nil || *h.Load1 != 4.1 {
		t.Errorf("load1 = %v", h.Load1)
	}
	if h.MemTotalBytes == nil || *h.MemTotalBytes != 34359738368 {
		t.Errorf("mem_total = %v", h.MemTotalBytes)
	}
	if h.DiskFreeBytes == nil || *h.DiskFreeBytes != 309237645312 {
		t.Errorf("disk_free = %v", h.DiskFreeBytes)
	}
	if h.Runner == nil {
		t.Fatal("no runner half at all")
	}
	if h.Runner.Name != RunnerName || h.Runner.Version != "v0.1.1" {
		t.Errorf("runner = %+v", h.Runner)
	}
	// 15 GiB free is three runs' worth of the 4 GiB budget, so the cap of four
	// is held down to three by memory — and the reading says which.
	if h.Runner.SlotsTotal != 3 {
		t.Errorf("slots_total = %d, want 3 on 14.2 GiB free", h.Runner.SlotsTotal)
	}
	if h.Runner.SlotsUsed != 0 {
		t.Errorf("slots_used = %d, want 0", h.Runner.SlotsUsed)
	}
	if h.Runner.SlotsLimitedBy != "memory" {
		t.Errorf("slots_limited_by = %q, want memory", h.Runner.SlotsLimitedBy)
	}
}

// TestHostReadingOmitsWhatThePlatformCannotMeasure is the Windows case, and
// the rule the whole feature turns on: no load average must mean NO KEY, not a
// load average of zero.
func TestHostReadingOmitsWhatThePlatformCannotMeasure(t *testing.T) {
	m := Machine{
		OSKind: "windows", Arch: "amd64", CPUCount: 16,
		OS: "Windows 11 (build 22631)", OSKnown: true,
		TotalRAM: 68719476736, TotalRAMKnown: true,
		Headroom: Headroom{FreeRAM: 40000000000, RAMKnown: true},
		// No load average, and no disk measurement at all.
	}
	h := testRunner(m).hostReading()
	if h.Load1 != nil {
		t.Errorf("load1 = %v on a platform with no load average", *h.Load1)
	}
	if h.DiskTotalBytes != nil || h.DiskFreeBytes != nil {
		t.Errorf("disk was reported without being measured: %v %v", h.DiskTotalBytes, h.DiskFreeBytes)
	}

	blob, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"load1", "disk_total_bytes", "disk_free_bytes"} {
		if strings.Contains(string(blob), key) {
			t.Errorf("%q survived into the payload: %s", key, blob)
		}
	}
}

// TestMachineIsSampledOnce: one sample serves every queue's beat until it ages
// out. On macOS the free half is a subprocess, and a machine with three queues
// beating twice a minute would otherwise fork it a dozen times a minute for
// the same answer.
func TestMachineIsSampledOnce(t *testing.T) {
	var samples int
	r := testRunner(fullMachine())
	r.opts.Machine = func(string) Machine {
		samples++
		return fullMachine()
	}

	for i := 0; i < 5; i++ {
		r.hostReading()
	}
	if samples != 1 {
		t.Fatalf("measured %d times for five readings inside one interval", samples)
	}
	// A zero max-age is how the tick forces a fresh one.
	r.machine(0)
	if samples != 2 {
		t.Fatalf("the tick's forced refresh did not measure (%d)", samples)
	}
}

// ---------------------------------------------------------- session readings

func testQueueRunner(r *Runner) *queueRunner {
	qr := &queueRunner{r: r, q: config.Queue{Name: "mac-claude", AgentKind: "claude"}, workspace: "Elk Scout"}
	r.qrs = append(r.qrs, qr)
	return qr
}

func TestSessionReadingIdleAndFaulted(t *testing.T) {
	qr := testQueueRunner(testRunner(fullMachine()))

	s := qr.sessionReading()
	if s.State != string(StateIdle) {
		t.Errorf("state = %q, want idle", s.State)
	}
	if s.RunID != "" || s.Tokens != nil {
		t.Errorf("an idle queue reported a run: %+v", s)
	}

	// A run that ends `stuck` leaves the queue faulted, because an idle queue
	// and an idle-because-its-runs-keep-failing queue are the two most
	// different things on a fleet page and look identical otherwise.
	qr.startRun("arun-1")
	qr.endRun(runlog.StatusStuck)
	if got := qr.sessionReading().State; got != string(StateFaulted) {
		t.Errorf("state after a stuck run = %q, want faulted", got)
	}

	// A cancellation is a person's decision, not a broken machine.
	qr.startRun("arun-2")
	qr.endRun(runlog.StatusCancelled)
	if got := qr.sessionReading().State; got != string(StateIdle) {
		t.Errorf("state after a cancellation = %q, want idle", got)
	}

	// And a new run clears the fault it inherited.
	qr.startRun("arun-3")
	qr.endRun(runlog.StatusStuck)
	qr.startRun("arun-4")
	if got := qr.sessionReading().State; got != string(StateRunning) {
		t.Errorf("state = %q, want running", got)
	}
}

// TestSessionReadingRunningWithNoLiveSession is the window between a submit
// and Elk's review verdict: no agent process, but very much not an idle queue.
func TestSessionReadingRunningWithNoLiveSession(t *testing.T) {
	qr := testQueueRunner(testRunner(fullMachine()))
	qr.startRun("arun-7")

	s := qr.sessionReading()
	if s.State != string(StateRunning) {
		t.Errorf("state = %q, want running", s.State)
	}
	if s.RunID != "arun-7" {
		t.Errorf("run_id = %q", s.RunID)
	}
	if s.StartedAt == "" {
		t.Error("no started_at, so nobody can tell how long it has been like this")
	}
}

func TestSessionReadingAccumulatesTheRunsSpend(t *testing.T) {
	qr := testQueueRunner(testRunner(fullMachine()))
	qr.startRun("arun-8")

	if qr.sessionReading().Tokens != nil {
		t.Error("tokens reported before anything was spent")
	}
	qr.noteUsage(adapter.Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 100})
	qr.noteUsage(adapter.Usage{InputTokens: 5, OutputTokens: 3, CacheWriteTokens: 7})

	tok := qr.sessionReading().Tokens
	if tok == nil {
		t.Fatal("no tokens after two usage events")
	}
	if tok.In != 15 || tok.Out != 5 || tok.CacheRead != 100 || tok.CacheWrite != 7 {
		t.Errorf("tokens = %+v", tok)
	}

	// The total is per RUN and spans review rounds and stall-restarts, so a
	// new run starts from nothing.
	qr.endRun(runlog.StatusSubmitted)
	qr.startRun("arun-9")
	if qr.sessionReading().Tokens != nil {
		t.Error("the previous run's spend carried into the next one")
	}
}

func TestSessionReadingAsked(t *testing.T) {
	qr := testQueueRunner(testRunner(fullMachine()))
	qr.startRun("arun-10")

	qr.noteEvent(adapter.EventQuestion)
	if got := qr.sessionReading().State; got != string(StateAsked) {
		t.Errorf("state = %q, want asked", got)
	}
	// Idleness after a question is the agent STILL waiting — the very state
	// this reports — so it must not clear the flag.
	qr.noteEvent(adapter.EventIdle)
	if got := qr.sessionReading().State; got != string(StateAsked) {
		t.Errorf("an idle event cleared the question: state = %q", got)
	}
	// Anything the agent actually does clears it.
	qr.noteEvent(adapter.EventToolUse)
	if got := qr.sessionReading().State; got != string(StateRunning) {
		t.Errorf("state = %q, want running once the agent moved on", got)
	}
}

// TestSessionReadingAttachedAndApproval: a person at the keyboard outranks
// `running`, and a permission request in front of them outranks the person.
func TestSessionReadingAttachedAndApproval(t *testing.T) {
	r := testRunner(fullMachine())
	qr := testQueueRunner(r)
	qr.startRun("arun-11")

	live := &liveSession{
		runID: "arun-11", queue: qr.q.Name, agentKind: "claude",
		started: time.Now(), sess: &telemeterSession{},
		manifest: adapter.Manifest{}, logf: func(string, ...any) {},
	}
	r.sessions.add(live)

	if got := qr.sessionReading().State; got != string(StateRunning) {
		t.Errorf("state = %q, want running before anyone attaches", got)
	}

	live.mu.Lock()
	live.attached = true
	live.mu.Unlock()
	if got := qr.sessionReading().State; got != string(StateAttached) {
		t.Errorf("state = %q, want attached", got)
	}

	live.mu.Lock()
	live.pendingID = "perm-1"
	live.mu.Unlock()
	if got := qr.sessionReading().State; got != string(StateApproval) {
		t.Errorf("state = %q, want approval — it outranks attached", got)
	}
}

// TestSessionReadingTakesTheAdaptersTelemetry, including the distinction the
// whole MCP field exists for.
func TestSessionReadingTakesTheAdaptersTelemetry(t *testing.T) {
	r := testRunner(fullMachine())
	qr := testQueueRunner(r)
	qr.startRun("arun-12")

	used, window := int64(124000), int64(200000)
	sess := &telemeterSession{tel: adapter.SessionTelemetry{
		Model:               "claude-opus-5",
		ContextUsedTokens:   &used,
		ContextWindowTokens: &window,
		MCPServers:          &[]adapter.MCPServer{},
		RateLimit: &adapter.RateLimit{
			Status:       "allowed",
			Type:         "five_hour",
			ResetsAt:     time.Unix(1788057600, 0).UTC(),
			UsingOverage: new(bool),
			Windows: map[string]adapter.RateLimitWindow{
				"five_hour": {Utilization: 0.41, ResetsAt: time.Unix(1788057600, 0).UTC()},
				"seven_day": {Utilization: 0.72, ResetsAt: time.Unix(1788087600, 0).UTC()},
			},
		},
	}}
	r.sessions.add(&liveSession{
		runID: "arun-12", queue: qr.q.Name, started: time.Now(),
		sess: sess, logf: func(string, ...any) {},
	})

	s := qr.sessionReading()
	if s.Model != "claude-opus-5" {
		t.Errorf("model = %q", s.Model)
	}
	if s.ContextUsedTokens == nil || *s.ContextUsedTokens != 124000 {
		t.Errorf("context_used = %v", s.ContextUsedTokens)
	}
	if s.ContextWindowTokens == nil || *s.ContextWindowTokens != 200000 {
		t.Errorf("context_window = %v", s.ContextWindowTokens)
	}
	if len(s.Limits) != 2 || s.Limits["five_hour"].Utilization != 0.41 {
		t.Errorf("limits = %+v", s.Limits)
	}
	if s.Limits["seven_day"].ResetsAt == "" {
		t.Error("a window's reset time was dropped — a percentage cannot say when it stops mattering")
	}
	if s.RateLimit == nil || s.RateLimit.Status != "allowed" || s.RateLimit.Type != "five_hour" {
		t.Errorf("rate_limit = %+v", s.RateLimit)
	}
	if s.RateLimit.UsingOverage == nil || *s.RateLimit.UsingOverage {
		t.Errorf("using_overage = %v; an explicit false is a different claim from silence",
			s.RateLimit.UsingOverage)
	}

	// The empty inventory is the TRUE answer for a Rein-driven Claude run, and
	// it is reported as an empty list rather than as an absent key.
	if s.MCPServers == nil {
		t.Fatal("the session's empty MCP inventory was dropped")
	}
	if len(*s.MCPServers) != 0 {
		t.Errorf("mcp_servers = %v; Rein starts every run with an explicit empty set", *s.MCPServers)
	}
}

// TestSessionReadingWithoutATelemeter: codex and grok publish none of this,
// and an adapter that cannot answer must leave the fields out rather than
// render every run at 0% context.
func TestSessionReadingWithoutATelemeter(t *testing.T) {
	r := testRunner(fullMachine())
	qr := testQueueRunner(r)
	qr.startRun("arun-13")
	r.sessions.add(&liveSession{
		runID: "arun-13", queue: qr.q.Name, started: time.Now(),
		sess: &plainSession{}, logf: func(string, ...any) {},
	})

	s := qr.sessionReading()
	if s.State != string(StateRunning) || s.RunID != "arun-13" {
		t.Errorf("reading = %+v", s)
	}
	if s.Model != "" || s.ContextUsedTokens != nil || s.MCPServers != nil || s.RateLimit != nil {
		t.Errorf("an adapter that says nothing produced %+v", s)
	}
}

// ------------------------------------------------------------------ the tick

func TestBeatDueOnlyWhenAQueueHasGoneQuiet(t *testing.T) {
	qr := testQueueRunner(testRunner(fullMachine()))
	// Nothing has beaten yet, so the first pass is due — a runner starting up
	// should say it is there.
	if !qr.beatDue(time.Minute) {
		t.Error("a queue that has never beaten is not due")
	}
	qr.noteBeat()
	if qr.beatDue(time.Minute) {
		t.Error("a queue that beat a moment ago is due again")
	}
	if !qr.beatDue(0) {
		t.Error("a zero interval should make every queue due")
	}
}

// TestTelemetryPassSkipsAQueueThatIsBeatingForItself. The tick is a backstop,
// not a duplicate: an idle machine must send exactly the beats it always did.
func TestTelemetryPassSkipsAQueueThatIsBeatingForItself(t *testing.T) {
	var samples int
	r := testRunner(fullMachine())
	r.opts.Machine = func(string) Machine {
		samples++
		return fullMachine()
	}
	qr := testQueueRunner(r)
	qr.noteBeat()

	// A nil elk client would panic if the pass tried to beat, which is the
	// assertion: it must not.
	r.telemetryPass(context.Background(), time.Minute)
	if samples != 1 {
		t.Errorf("the machine was sampled %d times in one pass", samples)
	}
}

func TestTelemetryTickStopsWithTheContext(t *testing.T) {
	r := testRunner(fullMachine())
	r.opts.TelemetryInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.telemetryTick(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the telemetry tick outlived its context — a daemon that will not shut down")
	}
}

func TestTelemetryIntervalHasAFloor(t *testing.T) {
	r := testRunner(fullMachine())
	if got := r.telemetryInterval(); got != DefaultTelemetryInterval {
		t.Errorf("unset interval = %s, want the default", got)
	}
	r.opts.TelemetryInterval = time.Millisecond
	if got := r.telemetryInterval(); got != MinTelemetryInterval {
		t.Errorf("a one-millisecond interval = %s, want the floor %s", got, MinTelemetryInterval)
	}
	r.opts.TelemetryInterval = 5 * time.Minute
	if got := r.telemetryInterval(); got != 5*time.Minute {
		t.Errorf("a longer interval was not honoured: %s", got)
	}
}

// TestTheWholeReadingFitsWellInsideTheCap. The cap is 16 KiB; a real reading
// should be nowhere near it, and if it ever is, the trimming in
// internal/elk/telemetry.go is doing work it was never meant to do.
func TestTheWholeReadingFitsWellInsideTheCap(t *testing.T) {
	r := testRunner(fullMachine())
	qr := testQueueRunner(r)
	qr.startRun("arun-14")
	qr.noteUsage(adapter.Usage{InputTokens: 1200, OutputTokens: 800, CacheReadTokens: 240000})
	r.sessions.add(&liveSession{
		runID: "arun-14", queue: qr.q.Name, started: time.Now(),
		sess: &telemeterSession{tel: adapter.SessionTelemetry{
			Model:      "claude-opus-5",
			MCPServers: &[]adapter.MCPServer{},
			RateLimit: &adapter.RateLimit{Status: "allowed", Type: "five_hour", Windows: map[string]adapter.RateLimitWindow{
				"five_hour": {Utilization: 0.41},
				"seven_day": {Utilization: 0.72},
			}},
		}},
		logf: func(string, ...any) {},
	})

	tel := qr.telemetry()
	n, err := tel.Size()
	if err != nil {
		t.Fatal(err)
	}
	if n > elk.MaxTelemetryBytes/8 {
		t.Errorf("a real reading is %d bytes, over an eighth of the %d-byte cap — "+
			"something has grown that should not have", n, elk.MaxTelemetryBytes)
	}
	dropped, err := tel.Fit()
	if err != nil || len(dropped) != 0 {
		t.Errorf("an ordinary reading was trimmed: %v, %v", dropped, err)
	}
	t.Logf("a full reading is %d bytes", n)
}

// ------------------------------------------------------------------ stubs

// telemeterSession is an [adapter.Session] that answers [adapter.Telemeter].
type telemeterSession struct{ tel adapter.SessionTelemetry }

func (s *telemeterSession) Telemetry() adapter.SessionTelemetry { return s.tel }
func (s *telemeterSession) ID() string                          { return "sess-1" }
func (s *telemeterSession) Events() <-chan adapter.Event        { return nil }
func (s *telemeterSession) Send(context.Context, string) error  { return nil }
func (s *telemeterSession) Respond(context.Context, adapter.PermissionResponse) error {
	return nil
}
func (s *telemeterSession) Interrupt(context.Context) error { return nil }
func (s *telemeterSession) Wait() (adapter.Result, error)   { return adapter.Result{}, nil }

// plainSession is one that does NOT implement [adapter.Telemeter], which is
// every adapter but claude today. It is written out rather than embedding the
// one above, because embedding would inherit Telemetry and quietly make this
// the same test twice.
type plainSession struct{}

func (s *plainSession) ID() string                                                { return "sess-2" }
func (s *plainSession) Events() <-chan adapter.Event                              { return nil }
func (s *plainSession) Send(context.Context, string) error                        { return nil }
func (s *plainSession) Respond(context.Context, adapter.PermissionResponse) error { return nil }
func (s *plainSession) Interrupt(context.Context) error                           { return nil }
func (s *plainSession) Wait() (adapter.Result, error)                             { return adapter.Result{}, nil }
