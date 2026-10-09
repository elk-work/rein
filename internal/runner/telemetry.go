package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/runlog"
)

// Fleet telemetry: what this machine looks like, and what the session on each
// queue is doing, shipped to Elk on the beat.
//
// # Where it rides, and why that was the only workable answer
//
// Elk carries the reading on `heartbeat_executor`, as two optional objects
// stored verbatim. So "ride the existing heartbeat" and "make a separate call"
// are the same HTTP request either way; the real question is WHICH CLOCK fires
// it, and the loop as it stood answered that for us:
//
// **A queue's own heartbeat stops for the whole of a run.** [queueRunner.serve]
// is one goroutine per queue that beats, then calls claimAndDrive — which
// blocks until the run is submitted, reviewed and reaped. A thirty-seven-minute
// run is thirty-seven minutes with no beat, and Elk's online window is fifteen.
// So the queue that has the most interesting session reading in the fleet is
// exactly the queue that cannot send one, and — separately, and worse — has
// been showing as offline in `list_executors` the whole time it was working.
//
// Hence both halves of the answer:
//
//   - **Every beat carries the reading**, from whichever clock sent it. It
//     costs nothing: the call is happening anyway and the reply is still the
//     poll loop's queue-depth signal.
//   - **One goroutine, [Runner.telemetryTick], is a second clock that beats a
//     queue that has gone quiet.** It is a backstop, not a duplicate: a queue
//     whose serve loop beat within the interval is left alone, so an idle
//     machine sends exactly the beats it always did, and a busy one gets a beat
//     a minute from the tick instead of nothing for half an hour.
//
// Beating more often would also poll more often, and shorter claim latency is a
// feature — but that is a claiming decision and it belongs to
// [Options.PollInterval]. Wiring it to the telemetry cadence would mean nobody
// could make the fleet view fresher without also making the runner greedier.
// The two clocks stay separate, which is the same reasoning the package doc
// gives for `report_progress` and `heartbeat_executor` being separate already.
//
// # What is measured, and when
//
// The machine is sampled ONCE per interval and cached, not once per beat per
// queue. On macOS the free-memory half is a `/usr/bin/vm_stat` subprocess (see
// headroom_darwin.go for why), and a machine serving three queues at two beats
// a minute each would otherwise fork it a dozen times a minute to answer the
// same question. The sample refreshes on demand when it is older than the
// interval, so the first beat after startup is not a hole.
//
// # The rule the whole thing rests on
//
// **Omit what could not be measured; never send zero for it.** Elk renders
// "could not measure" and "zero" differently and they mean opposite things —
// a Windows box has no load average, while a load average of 0.0 is a machine
// doing nothing at all. Every measurement below therefore travels as a pointer
// or behind a known-flag, and internal/elk/telemetry.go holds the wire types
// that make that survive the encoding.

const (
	// DefaultTelemetryInterval is how often the machine is measured, and how
	// long a queue may go without a beat before the tick sends one for it.
	//
	// Sixty seconds against a fifteen-minute online window is fourteen
	// consecutive failures before Elk calls the queue offline, and a fleet
	// view that is at most a minute stale — which is the granularity at which
	// "is that box wedged" is a question anyone asks.
	DefaultTelemetryInterval = 60 * time.Second

	// MinTelemetryInterval is the floor a config may ask for. A reading is a
	// round trip to Elk per queue; there is no view that gets better below
	// this, and there is a fleet that gets noisier.
	MinTelemetryInterval = 10 * time.Second
)

// SessionState is what a queue is doing, as the fleet reading reports it. The
// vocabulary is Elk's.
type SessionState string

const (
	// StateIdle — no run. The queue is polling and there is nothing to claim.
	StateIdle SessionState = "idle"

	// StateRunning — this queue holds a run and is working it. It covers the
	// gap after a submit while Elk's review pass is thinking, which is a
	// window with no agent process but very much not an idle queue.
	StateRunning SessionState = "running"

	// StateAttached — a person has taken the session over through
	// `rein attach`. The watchdog is held for as long as they are there.
	StateAttached SessionState = "attached"

	// StateAsked — the agent asked a question and has done nothing since.
	StateAsked SessionState = "asked"

	// StateApproval — a permission request is in front of an attached person
	// and has not been answered. It outranks `attached` because it is the more
	// specific and the more actionable of the two.
	StateApproval SessionState = "approval"

	// StateFaulted — no run, and the last one ended `stuck`. An idle queue and
	// an idle-because-its-runs-keep-failing queue look identical otherwise,
	// and they are the two most different things on a fleet page.
	StateFaulted SessionState = "faulted"
)

// RunnerName is what Rein calls itself in a host reading.
const RunnerName = "rein"

// ---------------------------------------------------------------- the machine

// machineCache holds the most recent sample and when it was taken. One sample
// serves every queue's beat until it ages out.
type machineCache struct {
	mu sync.Mutex
	at time.Time
	m  Machine
	ok bool
}

// machine returns a sample no older than maxAge, taking a fresh one if needed.
func (r *Runner) machine(maxAge time.Duration) Machine {
	r.machines.mu.Lock()
	defer r.machines.mu.Unlock()
	if r.machines.ok && time.Since(r.machines.at) < maxAge {
		return r.machines.m
	}
	r.machines.m = r.opts.Machine(r.gate.workDir)
	r.machines.at = time.Now()
	r.machines.ok = true
	return r.machines.m
}

// hostReading is the `host` half: what the machine is and how it is doing,
// plus Rein's own account of how much of it is in use.
//
// Every field that could fail to measure is set only when it did. GOOS, GOARCH
// and the CPU count are always present because the runtime answers them, which
// is what stops a reading from an unsupported platform being an empty object —
// and Elk discards an empty object as no reading at all.
func (r *Runner) hostReading() *elk.HostReading {
	m := r.machine(r.telemetryInterval())

	h := &elk.HostReading{
		OSKind:   m.OSKind,
		Arch:     m.Arch,
		CPUCount: m.CPUCount,
	}
	h.MachineName, _ = os.Hostname()
	if m.OSKnown {
		h.OS = m.OS
	}
	if m.CPUModelKnown {
		h.CPUModel = m.CPUModel
	}
	if m.Load1Known {
		load := m.Load1
		h.Load1 = &load
	}
	if m.TotalRAMKnown {
		total := m.TotalRAM
		h.MemTotalBytes = &total
	}
	if m.Headroom.RAMKnown {
		free := m.Headroom.FreeRAM
		h.MemFreeBytes = &free
	}
	if m.TotalDiskKnown {
		total := m.TotalDisk
		h.DiskTotalBytes = &total
	}
	if m.Headroom.DiskKnown {
		free := m.Headroom.FreeDisk
		h.DiskFreeBytes = &free
	}

	// The runner's own half. slots_total is what the machine can afford right
	// now, not the configured ceiling — the two differ on any laptop with a
	// simulator open, and the difference IS the interesting number.
	total, _ := Slots(m.Headroom, r.opts.MaxConcurrent)
	h.Runner = &elk.RunnerReading{
		Name:           RunnerName,
		Version:        r.opts.Version,
		SlotsTotal:     total,
		SlotsUsed:      r.gate.inFlight(),
		SlotsLimitedBy: SlotsLimitedBy(m.Headroom, r.opts.MaxConcurrent),
	}
	return h
}

// ---------------------------------------------------------------- the session

// queueTelemetry is a queue's own state, as opposed to its session's. It is
// guarded because the telemetry tick reads it from a goroutine that is not the
// one driving the run.
//
// The run id lives here rather than being taken from the session registry
// because the two disagree in a way that matters: between a submit and Elk's
// review verdict there is no agent process, and a queue whose state came from
// the registry would report itself idle while it was in fact holding a run and
// waiting on the server.
type queueTelemetry struct {
	runID   string
	started time.Time

	// tokens is what THIS RUN has spent, across every turn, review round and
	// stall-restart it took.
	tokens tokens

	// asked records that the agent put a question to a person and has done
	// nothing since. Any other event clears it.
	asked bool

	// faulted records that the last run on this queue ended `stuck`. It
	// survives into the idle period after it, which is the only time it is
	// reported.
	faulted bool

	// lastBeat is when this queue last told Elk it was alive, from either
	// clock. It is what makes the tick a backstop rather than a duplicate.
	lastBeat time.Time

	// waitingOn is why work Elk counts on this queue is not being started,
	// and waitingSince when that began — see [elk.SessionReading.WaitingOn].
	// Reported idle or running: a run reopened for revisions can be the one
	// waiting for a slot.
	waitingOn    string
	waitingSince time.Time
}

// tokens is a cumulative spend. Four plain counters: this is a running total
// that is only reported when something has been spent, so a zero here is
// genuinely zero rather than unmeasured.
type tokens struct {
	in, out, cacheRead, cacheWrite int64
}

func (t tokens) empty() bool {
	return t.in == 0 && t.out == 0 && t.cacheRead == 0 && t.cacheWrite == 0
}

func (t *tokens) add(u adapter.Usage) {
	t.in += u.InputTokens
	t.out += u.OutputTokens
	t.cacheRead += u.CacheReadTokens
	t.cacheWrite += u.CacheWriteTokens
}

// startRun marks the beginning of a run: a fresh spend, no question
// outstanding, and no fault carried over from the last one.
func (qr *queueRunner) startRun(runID string) {
	qr.telMu.Lock()
	defer qr.telMu.Unlock()
	qr.tel.runID = runID
	qr.tel.started = time.Now()
	qr.tel.tokens = tokens{}
	qr.tel.asked = false
	qr.tel.faulted = false
}

// endRun marks the end of one, remembering whether it ended badly. Only
// `stuck` counts: a cancellation is a person's decision and a shutdown is not
// the machine's fault, and neither should make a queue look broken.
func (qr *queueRunner) endRun(status runlog.Status) {
	qr.telMu.Lock()
	defer qr.telMu.Unlock()
	qr.tel.runID = ""
	qr.tel.started = time.Time{}
	qr.tel.faulted = status == runlog.StatusStuck
}

// noteUsage folds one usage increment into the run's total.
func (qr *queueRunner) noteUsage(u adapter.Usage) {
	qr.telMu.Lock()
	defer qr.telMu.Unlock()
	qr.tel.tokens.add(u)
}

// noteEvent tracks whether the agent is waiting on an answer: a question sets
// it, and the next thing the agent does clears it.
//
// EventIdle is the exception, because idleness after a question is the agent
// STILL waiting — the very state this is trying to report — and treating it as
// activity would clear the flag at exactly the wrong moment.
func (qr *queueRunner) noteEvent(kind adapter.EventKind) {
	if kind == adapter.EventIdle {
		return
	}
	qr.telMu.Lock()
	defer qr.telMu.Unlock()
	qr.tel.asked = kind == adapter.EventQuestion
}

// setWaiting records why work Elk counts on this queue is not being started,
// or clears it with "". It reports whether the reason changed in substance —
// compared with its numbers blanked, the way heartbeat replies are, so that a
// wait whose sentence only counts up the minutes is still the same wait.
func (qr *queueRunner) setWaiting(reason string) (changed bool) {
	qr.telMu.Lock()
	defer qr.telMu.Unlock()
	changed = digitsRE.ReplaceAllString(reason, "#") != digitsRE.ReplaceAllString(qr.tel.waitingOn, "#")
	switch {
	case reason == "":
		qr.tel.waitingSince = time.Time{}
	case qr.tel.waitingOn == "":
		qr.tel.waitingSince = time.Now()
	}
	qr.tel.waitingOn = reason
	return changed
}

// waiting is the current reason, if any.
func (qr *queueRunner) waiting() string {
	qr.telMu.Lock()
	defer qr.telMu.Unlock()
	return qr.tel.waitingOn
}

// noteBeat records that this queue has just told Elk it is alive.
func (qr *queueRunner) noteBeat() {
	qr.telMu.Lock()
	defer qr.telMu.Unlock()
	qr.tel.lastBeat = time.Now()
}

// beatDue reports whether this queue has gone longer than d without a beat —
// which, given the serve loop beats every poll, means its serve loop is inside
// a run and cannot.
func (qr *queueRunner) beatDue(d time.Duration) bool {
	qr.telMu.Lock()
	defer qr.telMu.Unlock()
	return time.Since(qr.tel.lastBeat) >= d
}

// sessionReading is the `session` half: what this queue is doing.
//
// It is never nil and always carries a state, because "there is no session" is
// itself the answer a fleet page needs — a queue that reported nothing would be
// indistinguishable from a queue whose runner is not running.
func (qr *queueRunner) sessionReading() *elk.SessionReading {
	qr.telMu.Lock()
	tel := qr.tel
	qr.telMu.Unlock()

	if tel.runID == "" {
		state := StateIdle
		if tel.faulted {
			state = StateFaulted
		}
		s := &elk.SessionReading{State: string(state), Land: qr.q.LandOrDefault()}
		putWaiting(s, tel)
		// No session, but the subscription is still there: the last reading,
		// carried forward, and whether the queue is holding (ark:rein#40).
		qr.fillHeadroom(s, nil)
		return s
	}

	s := &elk.SessionReading{
		State:     string(StateRunning),
		RunID:     tel.runID,
		StartedAt: elk.RFC3339(tel.started),
		Land:      qr.q.LandOrDefault(),
	}
	if !tel.tokens.empty() {
		s.Tokens = &elk.TokenReading{
			In:         tel.tokens.in,
			Out:        tel.tokens.out,
			CacheRead:  tel.tokens.cacheRead,
			CacheWrite: tel.tokens.cacheWrite,
		}
	}
	if tel.asked {
		s.State = string(StateAsked)
	}
	putWaiting(s, tel)

	// The live session, if there is one. There is not, between a submit and
	// Elk's review verdict — the run is still this queue's, so the state above
	// stands, and there is simply nothing more to say about it.
	live := qr.r.sessions.byQueue(qr.r.opts.Config.QueueLabel(qr.q))
	if live == nil {
		qr.fillHeadroom(s, nil)
		return s
	}
	if state, ok := live.state(); ok {
		s.State = string(state)
	}
	fillSessionTelemetry(s, live.sess)
	qr.fillHeadroom(s, live.sess)
	return s
}

// putWaiting adds why the queue's work is held up, when something is.
//
// The state stays what it was — idle, or running for a run waiting to work its
// revisions — rather than becoming a new word: the apps render Elk's state
// vocabulary and nothing else, and a state they do not know reads as "No
// reading", which is the opposite of what this is for.
func putWaiting(s *elk.SessionReading, tel queueTelemetry) {
	if tel.waitingOn == "" {
		return
	}
	s.WaitingOn = tel.waitingOn
	s.WaitingSince = elk.RFC3339(tel.waitingSince)
}

// fillHeadroom adds the subscription half of the reading: the window as the
// newest source knows it, and `exhausted_until` while the queue is holding.
//
// A live session's own reading is folded into the queue's state first, so a
// window that runs out mid-run is on the next beat rather than the first one
// after the run ends. Where the session has said nothing about its window —
// Codex before its first update, Grok always — the carried reading stands in,
// marked with its own `sampled_at`, so a reader can see how old it is.
func (qr *queueRunner) fillHeadroom(s *elk.SessionReading, sess adapter.Session) {
	if sess != nil {
		qr.absorbSession(sess)
	}
	until, reason, held := qr.holding()
	if held {
		s.ExhaustedUntil = elk.RFC3339(until)
		s.ExhaustedReason = reason
	}
	if s.RateLimit == nil && len(s.Limits) == 0 {
		if rl := qr.carriedReading(time.Now()); rl != nil {
			putRateLimit(s, rl)
		}
	}
	if s.RateLimit != nil {
		s.RateLimit.Exhausted = &held
	}
}

// state is what a person has done to this session, if anything. It reports
// false when nobody has, leaving the queue's own state to stand.
func (l *liveSession) state() (SessionState, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.pendingID != "":
		return StateApproval, true
	case l.attached:
		return StateAttached, true
	}
	return "", false
}

// fillSessionTelemetry copies whatever the adapter can say about its own
// session into the reading.
//
// [adapter.Telemeter] is optional, and what each adapter answers differs:
// Claude says nearly everything, Codex its model and subscription window, Grok
// its model and whether the turn ended on a rate limit. A field an adapter
// does not answer is left out, which is honest — the alternative is a fleet
// page showing every Codex run at 0% context.
func fillSessionTelemetry(s *elk.SessionReading, sess adapter.Session) {
	t, ok := sess.(adapter.Telemeter)
	if !ok {
		return
	}
	tel := t.Telemetry()
	s.Model = tel.Model
	s.ContextUsedTokens = tel.ContextUsedTokens
	s.ContextWindowTokens = tel.ContextWindowTokens

	if rl := tel.RateLimit; rl != nil {
		putRateLimit(s, rl)
	}

	// The empty list is a real answer and is reported as one. A Rein-driven
	// Claude run has no MCP servers by construction — every session is started
	// with --strict-mcp-config and an explicit empty set, after ark:rein#21 —
	// so `[]` here is the truth, and an omitted key would wrongly say the
	// question was never put.
	if tel.MCPServers != nil {
		servers := make([]elk.MCPServerReading, 0, len(*tel.MCPServers))
		for _, m := range *tel.MCPServers {
			servers = append(servers, elk.MCPServerReading{Name: m.Name, Status: m.Status})
		}
		s.MCPServers = &servers
	}
}

// putRateLimit writes a subscription reading into the session half: the
// scalars under `rate_limit`, the windows under `limits`.
func putRateLimit(s *elk.SessionReading, rl *adapter.RateLimit) {
	s.RateLimit = &elk.RateLimitState{
		Status:          rl.Status,
		Type:            rl.Type,
		ResetsAt:        elk.RFC3339(rl.ResetsAt),
		UsingOverage:    rl.UsingOverage,
		OverageStatus:   rl.OverageStatus,
		OverageResetsAt: elk.RFC3339(rl.OverageResetsAt),
		Source:          rl.Source,
		SampledAt:       elk.RFC3339(rl.SampledAt),
	}
	if len(rl.Windows) > 0 {
		s.Limits = make(map[string]elk.RateLimitWindow, len(rl.Windows))
		for name, w := range rl.Windows {
			s.Limits[name] = elk.RateLimitWindow{
				Utilization:   w.Utilization,
				ResetsAt:      elk.RFC3339(w.ResetsAt),
				WindowMinutes: w.Minutes,
			}
		}
	}
}

// telemetry is the pair of readings for one queue's beat.
func (qr *queueRunner) telemetry() elk.Telemetry {
	host := qr.r.hostReading()
	qr.telMu.Lock()
	host.PreflightState = qr.preflightState
	qr.telMu.Unlock()
	return elk.Telemetry{Host: host, Session: qr.sessionReading()}
}

// ---------------------------------------------------------------- the tick

func (r *Runner) telemetryInterval() time.Duration {
	d := r.opts.TelemetryInterval
	if d <= 0 {
		d = DefaultTelemetryInterval
	}
	if d < MinTelemetryInterval {
		d = MinTelemetryInterval
	}
	return d
}

// telemetryTick is the daemon's one periodic loop: keep the machine sample
// warm, and beat for any queue whose own loop has gone quiet.
//
// It exits with ctx, which is cancelled when [Runner.Run] returns — so
// `--once` does not leave a goroutine beating for a runner that has finished.
func (r *Runner) telemetryTick(ctx context.Context) {
	d := r.telemetryInterval()
	t := time.NewTicker(d)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r.telemetryPass(ctx, d)
	}
}

// telemetryPass is one turn of the tick: take a fresh sample, then beat for
// every queue that has not beaten inside d.
//
// Separated from the ticker so the policy — which queues get a beat, and that
// the sample is taken exactly once for all of them — is testable without a
// test that has to wait out a real interval.
func (r *Runner) telemetryPass(ctx context.Context, d time.Duration) {
	// Refresh first, so this pass's beats and any beat a serve loop sends
	// before the next pass all describe the same instant.
	r.machine(0)
	for _, qr := range r.qrs {
		if ctx.Err() != nil {
			return
		}
		if !qr.beatDue(d) {
			// Its own loop is beating; a second call would say the same thing
			// twice a minute for nothing.
			continue
		}
		qr.heartbeat(ctx)
	}
}

// Setup state is the last check, not a claim that an unchecked queue is ready.
// Never ship error text: vendor failures may contain credentials or URLs.
func (qr *queueRunner) setPreflightState(state string) {
	qr.telMu.Lock()
	qr.preflightState = state
	qr.telMu.Unlock()
}

func agentSetupFailure(err error) string {
	if errors.Is(err, exec.ErrNotFound) {
		return "Missing agent capability"
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "login") || strings.Contains(text, "auth") || errors.Is(err, adapter.ErrNotPlanAuth) {
		return "Agent authentication required"
	}
	return "The agent is not ready on this machine"
}

func (qr *queueRunner) checkAgentSetup(ctx context.Context) {
	a, ok := adapter.Lookup(qr.q.AgentKind)
	if !ok {
		qr.setPreflightState("No adapter for this queue")
		return
	}
	if !a.Manifest().SupportsHost() {
		qr.setPreflightState("Missing agent capability for this host")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := a.Preflight(ctx); err != nil {
		qr.setPreflightState(agentSetupFailure(err))
		return
	}
	qr.setPreflightState("ready")
}
