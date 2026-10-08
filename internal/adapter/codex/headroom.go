package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/elk-work/rein/internal/secretenv"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// Subscription headroom: how much of the ChatGPT plan behind this login is
// left, read without spending a turn (ark:rein#40).
//
// Codex is the one adapter that can do this. `account/rateLimits/read` over
// `codex app-server` answers from the backend's usage endpoint for the price
// of a process start — 0.4 s after initialize on 2026-09-29, codex-cli
// 0.159.0 — and the same numbers arrive during a run as
// `account/rateLimits/updated`. Elk's router reads the result to decide
// whether this person's Codex has room before it sends work here, and the run
// loop reads [adapter.RateLimit.Exhausted] to stop claiming while it has none.

// defaultProbeTimeout bounds one headroom read: spawn, initialize, one call.
// It is far longer than the read takes, because a Codex that is slow to start
// is a Codex whose reading can wait for the next idle poll.
const defaultProbeTimeout = 30 * time.Second

// Window names. Elk renders `five_hour` and `seven_day` today, and names what
// a window IS rather than which slot it arrived in — so a Codex window whose
// stated length is one of those two is reported under that name, and Elk shows
// it without a server change. A length that matches neither keeps the vendor's
// slot name, `primary` or `secondary`.
const (
	fiveHourMinutes = 5 * 60
	sevenDayMinutes = 7 * 24 * 60
)

func windowName(slot string, w *rateLimitWindow) string {
	if w != nil && w.WindowDurationMins != nil {
		switch *w.WindowDurationMins {
		case fiveHourMinutes:
			return "five_hour"
		case sevenDayMinutes:
			return "seven_day"
		}
	}
	return slot
}

// toRateLimit is the adapter's reading of one snapshot. allowed is
// `ordinaryUsageAllowed` from a read, and nil from a notification, which does
// not carry it.
//
// Exhausted is set on the backend's word only, never on a percentage this
// adapter judged close enough:
//
//   - `ordinaryUsageAllowed: false` — the backend's own no;
//   - any `rateLimitReachedType` — a limit, or a workspace's credits, ran out;
//   - a window at 100% with no credits behind it. Credits are what carry a
//     ChatGPT account past a full window, the way extra usage does for Claude,
//     so a full window on an account with credits is not a stopped one.
//
// ExhaustedUntil is the latest reset among the full windows — the soonest the
// account can be served again — and zero when none is full (a stop for a
// reason no window explains). The run loop re-reads a Codex queue before it
// claims again after that time, because the schema says outright that a client
// must not infer recovery from a reset time.
func toRateLimit(snap rateLimitSnapshot, allowed *bool, now time.Time) *adapter.RateLimit {
	rl := &adapter.RateLimit{
		Status:    snap.RateLimitReachedType,
		Source:    adapter.SourceCodexRateLimits,
		SampledAt: now.UTC(),
	}
	credits := snap.Credits != nil && (snap.Credits.HasCredits || snap.Credits.Unlimited)

	slots := []struct {
		slot string
		w    *rateLimitWindow
	}{{"primary", snap.Primary}, {"secondary", snap.Secondary}}

	var full []adapter.RateLimitWindow
	var fullNames []string
	for _, s := range slots {
		if s.w == nil {
			continue
		}
		name := windowName(s.slot, s.w)
		if _, taken := rl.Windows[name]; taken {
			// Two windows of the same stated length. Unlikely, and not worth
			// losing one over: fall back to the slot names for both.
			name = s.slot
		}
		w := adapter.RateLimitWindow{Utilization: float64(s.w.UsedPercent) / 100}
		if s.w.ResetsAt != nil {
			w.ResetsAt = epochSeconds(*s.w.ResetsAt)
		}
		if s.w.WindowDurationMins != nil {
			w.Minutes = *s.w.WindowDurationMins
		}
		if rl.Windows == nil {
			rl.Windows = map[string]adapter.RateLimitWindow{}
		}
		rl.Windows[name] = w
		if s.w.UsedPercent >= 100 {
			full = append(full, w)
			fullNames = append(fullNames, name)
		}
	}

	switch {
	case allowed != nil && !*allowed:
		rl.Exhausted = true
	case snap.RateLimitReachedType != "":
		rl.Exhausted = true
	case len(full) > 0 && !credits:
		rl.Exhausted = true
	}
	if rl.Exhausted {
		for i, w := range full {
			if w.ResetsAt.After(rl.ExhaustedUntil) {
				rl.ExhaustedUntil = w.ResetsAt
				rl.Type = fullNames[i]
			}
		}
		rl.ResetsAt = rl.ExhaustedUntil
	}
	return rl
}

func epochSeconds(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// ReadHeadroom implements [adapter.HeadroomReader]: start `codex app-server`,
// initialize, ask `account/rateLimits/read`, and stop. No thread, no turn, no
// model call.
//
// It runs under a private CODEX_HOME holding only the login link — not even
// Rein's own history, because Codex indexes whatever history it is given
// before it answers initialize (home.go, ark:rein#38), and a headroom read
// needs none of it.
func (a *Adapter) ReadHeadroom(ctx context.Context) (*adapter.RateLimit, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultProbeTimeout)
		defer cancel()
	}
	proc, err := a.spawnProbe(ctx)
	if err != nil {
		return nil, fmt.Errorf("codex: starting `%s app-server` for a headroom read: %w", a.binary, err)
	}
	defer func() {
		// Closing stdin is how the app server is told the client is done; the
		// kill is the backstop for one that does not take the hint.
		_ = proc.stdin.Close()
		done := make(chan struct{})
		go func() { _ = proc.wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(a.probeGrace()):
			proc.kill()
			<-done
		}
		proc.home.remove()
	}()

	h := &probeHandler{}
	c := newConn(proc.stdout, proc.stdin)
	go func() { _ = c.serve(h) }()

	var initRes initializeResponse
	if err := c.call(ctx, methodInitialize, initializeParams{
		ClientInfo: clientInfo{Name: "rein", Version: "0", Title: "Rein runner (headroom)"},
	}, &initRes); err != nil {
		return nil, fmt.Errorf("codex: headroom read: initialize: %w%s", err, stderrNote(proc))
	}
	if err := c.notify(methodInitialized, struct{}{}); err != nil {
		return nil, fmt.Errorf("codex: headroom read: initialized: %w", err)
	}
	var res rateLimitsReadResponse
	if err := c.call(ctx, methodRateLimitsRead, rateLimitsReadParams{ExcludeResetCreditDetails: true}, &res); err != nil {
		return nil, fmt.Errorf("codex: headroom read: %s: %w%s", methodRateLimitsRead, err, stderrNote(proc))
	}
	return toRateLimit(res.RateLimits, res.OrdinaryUsageAllowed, a.now()), nil
}

// probeGrace is how long a finished headroom read waits for the app server to
// exit on its own before killing it.
func (a *Adapter) probeGrace() time.Duration {
	if a.probeExitGrace > 0 {
		return a.probeExitGrace
	}
	return 5 * time.Second
}

func stderrNote(p *process) string {
	if t := p.stderrTail.String(); t != "" {
		return "\napp-server stderr:\n" + t
	}
	return ""
}

// probeHandler ignores everything the app server volunteers during a headroom
// read. It sends no server request before a thread exists — approvals and
// questions belong to a turn — so there is nothing to answer.
type probeHandler struct{}

func (h *probeHandler) onNotification(string, json.RawMessage) {}

func (h *probeHandler) onRequest(json.RawMessage, string, json.RawMessage) {}

// spawnProbeProcess is the real probe: `codex app-server` under a home that
// holds the login and nothing else, run from inside that home so it reads no
// project configuration either.
func (a *Adapter) spawnProbeProcess(ctx context.Context) (*process, error) {
	home, err := newHome("headroom", false)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(a.binary, "app-server")
	cmd.Dir = home.dir
	cmd.Env = mergeEnv(secretenv.Filter(os.Environ()), home.env())

	stdin, stdout, stderr, err := pipes(cmd)
	if err != nil {
		home.remove()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		home.remove()
		return nil, err
	}
	t := newTail(20)
	go t.drain(stderr)

	var once sync.Once
	var waitErr error
	done := make(chan struct{})
	return &process{
		stdin:      stdin,
		stdout:     stdout,
		home:       home,
		stderrTail: t,
		wait: func() error {
			once.Do(func() { waitErr = cmd.Wait(); close(done) })
			<-done
			return waitErr
		},
		kill: func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		},
		exitCode: func() int {
			if cmd.ProcessState == nil {
				return -1
			}
			return cmd.ProcessState.ExitCode()
		},
	}, nil
}

// telemetry is what a Codex session can say about itself: the model, and the
// subscription window as the last `account/rateLimits/updated` described it.
// Context fill and MCP inventory are left out — Codex does not say, and an
// adapter that guessed would render every Codex run at 0% context.
func (s *session) Telemetry() adapter.SessionTelemetry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return adapter.SessionTelemetry{Model: s.model, RateLimit: s.rateLimit}
}

// onRateLimits records a mid-run window update. Replaced wholesale, never
// mutated: [session.Telemetry] hands the pointer to another goroutine.
func (s *session) onRateLimits(n rateLimitsUpdatedNotification) {
	rl := toRateLimit(n.RateLimits, nil, s.a.now())
	s.mu.Lock()
	s.rateLimit = rl
	s.mu.Unlock()
}
