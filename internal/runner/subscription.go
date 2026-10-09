package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
)

// Subscription headroom, and backing off when there is none (ark:rein#40).
//
// # The failure this exists to stop
//
// Every queue runs on a person's own subscription — their Claude plan, their
// ChatGPT plan, their SuperGrok. Until this file, when that subscription's
// window ran out Rein did not notice: it kept claiming runs, each one started
// an agent that failed on its first request, each was submitted `stuck`, and
// the queue went on doing that until the window reset. Five hours of a
// person's work order backlog could be converted into five hours of failed
// runs, and Elk's router had no way to know beforehand that it was sending
// work to a subscription that had none left.
//
// # What each agent can say
//
//   - **Claude** reports its windows in `rate_limit_event` — but only on the
//     stream of a request it is already making. There is no turn-free read:
//     `/usage` is a separate endpoint that is itself rate-limited, and a
//     "cheap" idle call would still be a real turn billed to the same window.
//     So Claude's reading is CARRIED FORWARD from the last run, with
//     `sampled_at` saying how old it is, and never refreshed while idle.
//   - **Codex** answers `account/rateLimits/read` over its app server with no
//     turn at all ([adapter.HeadroomReader]). An idle Codex queue re-reads it
//     every [HeadroomRefreshAge], and before it claims again after a hold.
//   - **Grok** exposes no window. A turn that stops on a rate limit is the only
//     signal, and it comes after the failure.
//
// Each adapter turns its vendor's words into [adapter.RateLimit.Exhausted] and
// [adapter.RateLimit.ExhaustedUntil]; this file never learns which vendor said
// what. It keeps the latest reading per queue, decides how long to hold when
// the vendor gave no time, gates the claim, reports the state on every beat,
// and remembers it across a restart.

const (
	// HeadroomRefreshAge is how old an idle queue's reading may get before an
	// adapter that can read it without a turn is asked again.
	HeadroomRefreshAge = 30 * time.Minute

	// ExhaustedFallback is how long a queue holds when the vendor said it is
	// out and did not say until when, and the person stated no weekly reset.
	ExhaustedFallback = 24 * time.Hour

	// staleResetHold is how long to hold when the vendor said it is out and
	// named a reset that has already passed — clock skew, or a reading that
	// was a moment late. Short, because the vendor has said the time is up.
	staleResetHold = 5 * time.Minute

	// headroomReadTimeout bounds one idle read.
	headroomReadTimeout = 45 * time.Second
)

// subscription is one queue's headroom state. Guarded by its own lock: the
// serve loop writes it, the telemetry tick reads it, and a live session's
// reading is folded in from whichever of the two asks first.
type subscription struct {
	mu sync.Mutex

	// last is the most recent reading, from any source, carried forward.
	last *adapter.RateLimit

	// until is exhausted_until: the queue claims nothing before it. Zero is
	// "not held".
	until time.Time

	// reason is one line — why, and on what basis `until` was chosen.
	reason string

	// lastRead is when an idle read was last attempted, and readErr what the
	// last failed one said, so a failing read is logged once rather than
	// every half hour.
	lastRead time.Time
	readErr  string
}

// persistedSubscription is what survives a restart. A queue that was held
// must still be held after the service restarts — otherwise the first thing a
// restarted runner does is claim a run on a subscription it already knows is
// out, which is the failure this file exists to stop.
type persistedSubscription struct {
	Until  time.Time          `json:"exhausted_until,omitempty"`
	Reason string             `json:"exhausted_reason,omitempty"`
	Last   *adapter.RateLimit `json:"last,omitempty"`
}

// subscriptionFile is where one queue's state lives under a state directory.
func subscriptionFile(stateDir, queue string) string {
	return filepath.Join(stateDir, "headroom", url.PathEscape(queue)+".json")
}

// subscriptionPath is where this queue's state lives, or "" when the runner
// has no state directory (most tests).
func (qr *queueRunner) subscriptionPath() string {
	dir := qr.r.opts.StateDir
	if dir == "" {
		return ""
	}
	return subscriptionFile(dir, qr.r.opts.Config.QueueLabel(qr.q))
}

func readPersisted(path string) (persistedSubscription, bool) {
	var p persistedSubscription
	b, err := os.ReadFile(path)
	if err != nil {
		return p, false
	}
	if json.Unmarshal(b, &p) != nil {
		return p, false
	}
	return p, true
}

// SubscriptionState is a queue's remembered headroom, as `rein status` reads
// it from the state directory a running `rein run` writes.
type SubscriptionState struct {
	// Until is exhausted_until; zero when the queue is not held.
	Until  time.Time
	Reason string
	// Last is the newest reading, or nil when none has been taken.
	Last *adapter.RateLimit
}

// ReadSubscriptionState reads one queue's state. It reports false when
// there is none — the queue has never run under this Rein home, or never
// reported a window.
func ReadSubscriptionState(stateDir, queue string) (SubscriptionState, bool) {
	p, ok := readPersisted(subscriptionFile(stateDir, queue))
	if !ok {
		return SubscriptionState{}, false
	}
	return SubscriptionState{Until: p.Until, Reason: p.Reason, Last: p.Last}, true
}

// Summary is the state in one short line: held and until when, or the
// windows as last read and how long ago.
func (s SubscriptionState) Summary(now time.Time) string {
	if !s.Until.IsZero() && s.Until.After(now) {
		return "exhausted until " + s.Until.UTC().Format(time.RFC3339)
	}
	if s.Last == nil {
		return "—"
	}
	var parts []string
	for _, name := range []string{"five_hour", "seven_day"} {
		if w, ok := s.Last.Windows[name]; ok && (w.ResetsAt.IsZero() || w.ResetsAt.After(now)) {
			parts = append(parts, fmt.Sprintf("%s %.0f%%", name, w.Utilization*100))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "room")
	}
	if !s.Last.SampledAt.IsZero() {
		parts = append(parts, "read "+ago(now.Sub(s.Last.SampledAt)))
	}
	return strings.Join(parts, ", ")
}

// ago renders an age the way a person reads one: "just now", "12m ago",
// "3h ago", "2d ago" — not "1h0m0s".
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}

// loadSubscription restores a queue's state at startup. A hold that has
// already lapsed is dropped; the reading is kept, because it is still the
// newest thing known about the account.
func (qr *queueRunner) loadSubscription() {
	path := qr.subscriptionPath()
	if path == "" {
		return
	}
	p, ok := readPersisted(path)
	if !ok {
		return
	}
	qr.sub.mu.Lock()
	defer qr.sub.mu.Unlock()
	qr.sub.last = p.Last
	if p.Until.After(time.Now()) {
		qr.sub.until, qr.sub.reason = p.Until, p.Reason
	}
}

// saveSubscription writes the state. Best effort: a runner that cannot write
// its state directory still holds in memory, which covers everything but a
// restart.
func (qr *queueRunner) saveSubscription(p persistedSubscription) {
	path := qr.subscriptionPath()
	if path == "" {
		return
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// absorbRateLimit folds one reading into the queue's state and reports
// whether the hold changed.
//
// A reading older than the one already held is ignored: the telemetry tick
// and the end of a run can both hand in the same session's reading, and a
// headroom read can race a run's own events.
func (qr *queueRunner) absorbRateLimit(rl *adapter.RateLimit) {
	if rl == nil {
		return
	}
	now := time.Now()

	qr.sub.mu.Lock()
	if prev := qr.sub.last; prev != nil && !rl.SampledAt.IsZero() && rl.SampledAt.Before(prev.SampledAt) {
		qr.sub.mu.Unlock()
		return
	}
	if prev := qr.sub.last; prev != nil && rl.SampledAt.Equal(prev.SampledAt) && rl.Exhausted == prev.Exhausted &&
		rl.ExhaustedUntil.Equal(prev.ExhaustedUntil) {
		// The same reading handed in twice.
		qr.sub.last = rl
		qr.sub.mu.Unlock()
		return
	}
	qr.sub.last = rl
	wasUntil := qr.sub.until
	if rl.Exhausted {
		qr.sub.until, qr.sub.reason = qr.holdFor(rl, now)
	} else {
		// The vendor says there is room. That outranks any hold, including a
		// fallback this runner chose for itself.
		qr.sub.until, qr.sub.reason = time.Time{}, ""
	}
	until, reason := qr.sub.until, qr.sub.reason
	p := persistedSubscription{Until: until, Reason: reason, Last: rl}
	qr.sub.mu.Unlock()

	qr.saveSubscription(p)
	switch {
	case !until.IsZero() && !until.Equal(wasUntil):
		qr.logf("subscription exhausted — not claiming until %s (%s)", until.UTC().Format(time.RFC3339), reason)
	case until.IsZero() && !wasUntil.IsZero():
		qr.logf("subscription has room again (%s) — claiming", describeSource(rl))
	}
}

// holdFor decides exhausted_until for an exhausted reading, and says why.
//
// The vendor's own reset time is used whenever it gave one. Without one:
//
//   - an adapter that can re-read without a turn holds only until the next
//     read, which is what settles it;
//   - otherwise the person's stated weekly reset, from config.toml;
//   - otherwise [ExhaustedFallback].
func (qr *queueRunner) holdFor(rl *adapter.RateLimit, now time.Time) (time.Time, string) {
	what := describeExhaustion(rl)
	if u := rl.ExhaustedUntil; !u.IsZero() {
		if u.After(now) {
			return u, what + "; resets at the time the vendor gave"
		}
		return now.Add(staleResetHold), what + "; the reset it named has passed, holding briefly"
	}
	if qr.headroomReader() != nil {
		return now.Add(HeadroomRefreshAge), what + "; no reset given, holding until the next read"
	}
	if wr, ok, _ := config.ParseWeeklyReset(qr.q.WeeklyReset); ok {
		return wr.Next(now), what + "; no reset given, holding until the stated weekly reset (" + qr.q.WeeklyReset + ")"
	}
	return now.Add(ExhaustedFallback), what + "; no reset given and no weekly_reset stated, holding 24 hours"
}

// describeExhaustion says what the vendor said, in a sentence a person can
// read in a log line or a stuck deliverable.
func describeExhaustion(rl *adapter.RateLimit) string {
	window := ""
	if rl.Type != "" {
		window = " (" + rl.Type + " window)"
	}
	switch rl.Source {
	case adapter.SourceClaudeEvent:
		return "Claude rejected the request" + window
	case adapter.SourceCodexRateLimits:
		if rl.Status != "" {
			return "Codex reports " + rl.Status + window
		}
		if rl.Type != "" {
			return "Codex reports the " + rl.Type + " window full"
		}
		return "Codex reports ordinary usage is not allowed"
	case adapter.SourceGrokStopFailure:
		return "a Grok turn stopped on a rate limit"
	}
	return "the agent reported its subscription exhausted" + window
}

func describeSource(rl *adapter.RateLimit) string {
	switch rl.Source {
	case adapter.SourceClaudeEvent:
		return "Claude's rate_limit_event"
	case adapter.SourceCodexRateLimits:
		return "Codex account/rateLimits"
	case adapter.SourceGrokStopFailure:
		return "Grok"
	}
	return "the agent"
}

// holding reports whether the queue must not claim now, and until when. A
// hold that has lapsed is cleared here, and said so, so the log shows the
// queue coming back.
func (qr *queueRunner) holding() (time.Time, string, bool) {
	now := time.Now()
	qr.sub.mu.Lock()
	until, reason := qr.sub.until, qr.sub.reason
	lapsed := !until.IsZero() && !now.Before(until)
	if lapsed {
		qr.sub.until, qr.sub.reason = time.Time{}, ""
	}
	last := qr.sub.last
	qr.sub.mu.Unlock()

	if lapsed {
		qr.saveSubscription(persistedSubscription{Last: last})
		qr.logf("subscription hold ended at %s — claiming again", until.UTC().Format(time.RFC3339))
		return time.Time{}, "", false
	}
	return until, reason, !until.IsZero()
}

// headroomReader is this queue's adapter as a [adapter.HeadroomReader], or
// nil when it cannot read its window without a turn.
func (qr *queueRunner) headroomReader() adapter.HeadroomReader {
	a, ok := adapter.Lookup(qr.q.AgentKind)
	if !ok {
		return nil
	}
	hr, _ := a.(adapter.HeadroomReader)
	return hr
}

// refreshHeadroom reads the window while the queue is idle, when the adapter
// can do that without a turn and the reading is due: never read, older than
// [HeadroomRefreshAge], or a hold has just ended — Codex's schema is explicit
// that a client must not infer recovery from a reset time, so it is asked
// before the queue claims again.
//
// Called only from the serve loop, between runs, so it never races the
// queue's own session.
func (qr *queueRunner) refreshHeadroom(ctx context.Context) {
	hr := qr.headroomReader()
	if hr == nil || qr.r.opts.DryRun {
		return
	}
	now := time.Now()
	qr.sub.mu.Lock()
	sampled := time.Time{}
	if qr.sub.last != nil {
		sampled = qr.sub.last.SampledAt
	}
	holdEnded := !qr.sub.until.IsZero() && !now.Before(qr.sub.until) && qr.sub.lastRead.Before(qr.sub.until)
	due := holdEnded ||
		(now.Sub(sampled) >= qr.refreshAge() && now.Sub(qr.sub.lastRead) >= qr.refreshAge())
	if due {
		qr.sub.lastRead = now
	}
	qr.sub.mu.Unlock()
	if !due {
		return
	}

	rctx, cancel := context.WithTimeout(ctx, headroomReadTimeout)
	defer cancel()
	rl, err := hr.ReadHeadroom(rctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		msg := firstLine(err.Error())
		qr.sub.mu.Lock()
		changed := msg != qr.sub.readErr
		qr.sub.readErr = msg
		qr.sub.mu.Unlock()
		if changed {
			qr.logf("headroom read failed (carrying the last reading forward): %s", msg)
		}
		return
	}
	qr.sub.mu.Lock()
	hadErr := qr.sub.readErr != ""
	qr.sub.readErr = ""
	qr.sub.mu.Unlock()
	if hadErr {
		qr.logf("headroom read recovered")
	}
	qr.absorbRateLimit(rl)
}

func (qr *queueRunner) refreshAge() time.Duration {
	if d := qr.r.opts.HeadroomRefresh; d > 0 {
		return d
	}
	return HeadroomRefreshAge
}

// absorbSession folds a session's own reading, if it has one, into the
// queue's state.
func (qr *queueRunner) absorbSession(sess adapter.Session) {
	if t, ok := sess.(adapter.Telemeter); ok {
		qr.absorbRateLimit(t.Telemetry().RateLimit)
	}
}

// carriedReading is the queue's last reading as the fleet reading reports it
// between runs: a copy, with every window whose own reset has passed left
// out. A fill measured before a reset says nothing about the window after it,
// and reporting it would show an account as full that has been empty for
// hours.
func (qr *queueRunner) carriedReading(now time.Time) *adapter.RateLimit {
	qr.sub.mu.Lock()
	last := qr.sub.last
	qr.sub.mu.Unlock()
	if last == nil {
		return nil
	}
	cp := *last
	cp.Windows = nil
	for name, w := range last.Windows {
		if !w.ResetsAt.IsZero() && !w.ResetsAt.After(now) {
			continue
		}
		if cp.Windows == nil {
			cp.Windows = map[string]adapter.RateLimitWindow{}
		}
		cp.Windows[name] = w
	}
	return &cp
}

// holdSentence is what a stuck deliverable says about a hold, or "" when the
// queue is not held.
func (qr *queueRunner) holdSentence() string {
	until, reason, held := qr.holding()
	if !held {
		return ""
	}
	return fmt.Sprintf("The subscription behind queue %s is exhausted: %s. Rein will not claim another run on "+
		"this queue until %s, and reports that time to Elk as `exhausted_until` on every heartbeat.",
		qr.q.Name, strings.TrimSuffix(reason, "."), until.UTC().Format(time.RFC3339))
}
