package runner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/control"
	"github.com/elk-work/rein/internal/runlog"
)

// AttachApprovalGrace is how long a permission request waits for the attached
// person before Rein answers it the way it always does, which is no.
//
// It is a grace period rather than an open wait because the alternative is a
// run that hangs forever if somebody attaches and then walks away, and because
// the fallback is the safe answer: the run continues, denied, with the agent
// told why. Two minutes is long enough to read what is being asked for and
// short enough that an abandoned terminal costs one gate rather than a lease.
const AttachApprovalGrace = 2 * time.Minute

// liveSession is one session this runner is currently driving, as the control
// plane sees it. It is what makes `rein attach` possible: the run loop already
// holds the process, so a person taking over is this object changing state,
// not a second process being started.
type liveSession struct {
	runID     string
	queue     string
	agentKind string
	direction string
	started   time.Time
	sess      adapter.Session
	manifest  adapter.Manifest
	log       *runlog.Writer
	logf      func(string, ...any)

	// onRelease folds one attachment's totals into the run's, so the
	// deliverable can say a person took part without the run loop having to
	// watch the socket.
	onRelease func(sends int, held time.Duration)

	mu         sync.Mutex
	attached   bool
	attachedAt time.Time
	sends      int
	pendingID  string
	answer     chan adapter.PermissionResponse
}

// info is what the control plane reports about this session.
func (l *liveSession) info() control.RunInfo {
	l.mu.Lock()
	attached := l.attached
	l.mu.Unlock()
	return control.RunInfo{
		RunID:       l.runID,
		Queue:       l.queue,
		AgentKind:   l.agentKind,
		Direction:   l.direction,
		SessionID:   l.sess.ID(),
		StartedAt:   l.started,
		Interactive: l.manifest.Has(adapter.CapSteer),
		Approvals:   l.manifest.Has(adapter.CapApprovals),
		Attached:    attached,
	}
}

// paused reports whether the run loop should hold its watchdog. A person at
// the keyboard is the one case where silence from the agent is not evidence
// that anything is wrong.
func (l *liveSession) paused() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.attached
}

// attach claims the session for a person, and hands back the release.
func (l *liveSession) attach() (control.Attached, func(), error) {
	l.mu.Lock()
	if l.attached {
		l.mu.Unlock()
		return control.Attached{}, nil, fmt.Errorf(
			"somebody is already attached to run %s", runlog.Short(l.runID))
	}
	l.attached, l.attachedAt, l.sends = true, time.Now(), 0
	l.mu.Unlock()

	info := l.info()
	note := "The daemon has stepped back: its watchdog is paused for as long as you are here, " +
		"and it is still reporting to Elk so the run keeps its lease."
	if !info.Interactive {
		note = "Watch only: the " + l.agentKind + " adapter declares no `steer` capability, so this " +
			"session takes no input after it starts. Nothing you type will reach it."
	}
	l.log.Runner(runlog.KindAttach, "a person attached; the daemon has stepped back")
	l.log.Update(func(m *runlog.Meta) { m.Attached = true })
	l.logf("run %s: a person attached — pausing the watchdog, still reporting to Elk", l.runID)

	var once sync.Once
	release := func() { once.Do(l.release) }
	return control.Attached{Run: info, Note: note}, release, nil
}

// release ends an attachment, however it ended — a clean detach, a Ctrl-C, a
// dropped ssh session. There is no state to reconcile beyond this, which is
// why the attachment's lifetime is the connection's.
func (l *liveSession) release() {
	l.mu.Lock()
	if !l.attached {
		l.mu.Unlock()
		return
	}
	held := time.Since(l.attachedAt)
	sends := l.sends
	l.attached, l.sends = false, 0
	// Anything still waiting for a human answer is not going to get one.
	l.pendingID, l.answer = "", nil
	l.mu.Unlock()

	l.log.Runner(runlog.KindAttach, "the person detached after %s, having sent %d message(s); the daemon is driving again",
		held.Round(time.Second), sends)
	l.log.Update(func(m *runlog.Meta) { m.Attached = false })
	l.logf("run %s: detached after %s (%d message(s) sent) — the watchdog is running again",
		l.runID, held.Round(time.Second), sends)
	if l.onRelease != nil {
		l.onRelease(sends, held)
	}
}

// input delivers a person's line to the agent.
func (l *liveSession) input(ctx context.Context, text string) error {
	if !l.manifest.Has(adapter.CapSteer) {
		return fmt.Errorf("the %s adapter declares no `steer` capability: this session takes no input after it starts",
			l.agentKind)
	}
	if err := l.sess.Send(ctx, text); err != nil {
		if errors.Is(err, adapter.ErrNotSupported) {
			return fmt.Errorf("the %s adapter cannot take input mid-run: %w", l.agentKind, err)
		}
		return err
	}
	l.mu.Lock()
	l.sends++
	l.mu.Unlock()
	// The text goes in the LOCAL log, where it is part of the account of the
	// run. It never reaches the deliverable — see [queueRunner.humanSummary],
	// which puts one line there and no transcript.
	l.log.Runner(runlog.KindAttach, "a person sent: %s", text)
	return nil
}

// respond hands a person's verdict to whatever is waiting for one.
//
// It goes through the run loop rather than straight to the session, because
// the loop is what turns a permission request into an Elk `gate` event, and a
// decision made outside it would be a decision Elk never hears about.
func (l *liveSession) respond(id string, allow bool, reason string) error {
	l.mu.Lock()
	pending, ch := l.pendingID, l.answer
	l.mu.Unlock()
	if ch == nil {
		return errors.New("nothing is waiting for an answer on this run")
	}
	if id != "" && id != pending {
		return fmt.Errorf("the request waiting for an answer is %q, not %q", pending, id)
	}
	select {
	case ch <- adapter.PermissionResponse{ID: pending, Allow: allow, Reason: reason}:
		return nil
	default:
		return errors.New("that request has already been answered")
	}
}

// awaitDecision offers a permission request to the attached person and waits,
// briefly, for an answer.
//
// It returns ok=false when nobody is attached, when the grace period lapses,
// or when the run is shutting down — and the caller then does what Rein has
// always done, which is deny it with a reason the agent can read.
func (l *liveSession) awaitDecision(ctx context.Context, id string, grace time.Duration) (adapter.PermissionResponse, bool) {
	if l == nil {
		return adapter.PermissionResponse{}, false
	}
	ch := make(chan adapter.PermissionResponse, 1)
	l.mu.Lock()
	if !l.attached {
		l.mu.Unlock()
		return adapter.PermissionResponse{}, false
	}
	l.pendingID, l.answer = id, ch
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.pendingID, l.answer = "", nil
		l.mu.Unlock()
	}()

	select {
	case resp := <-ch:
		return resp, true
	case <-time.After(grace):
		return adapter.PermissionResponse{}, false
	case <-ctx.Done():
		return adapter.PermissionResponse{}, false
	}
}

// sessionRegistry is every session this runner is driving, by run id. It is
// the whole of the runner's side of the control plane.
type sessionRegistry struct {
	mu    sync.Mutex
	byRun map[string]*liveSession
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{byRun: map[string]*liveSession{}}
}

func (r *sessionRegistry) add(l *liveSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byRun[l.runID] = l
}

// remove drops a session and releases any attachment it still had: a session
// that has ended cannot be driven by anybody, and a client still holding the
// socket should learn that from its own view of the run rather than from
// sending into a closed process.
func (r *sessionRegistry) remove(runID string) {
	r.mu.Lock()
	l := r.byRun[runID]
	delete(r.byRun, runID)
	r.mu.Unlock()
	if l != nil {
		l.release()
	}
}

// byQueue is the session one queue is driving right now, or nil.
//
// One at a time by construction: [queueRunner.serve] calls claimAndDrive
// serially, so a queue never has two. The concurrency in the loop is between
// queues, never within one.
func (r *sessionRegistry) byQueue(queue string) *liveSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.byRun {
		if l.queue == queue {
			return l
		}
	}
	return nil
}

func (r *sessionRegistry) all() []*liveSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*liveSession, 0, len(r.byRun))
	for _, l := range r.byRun {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].started.Before(out[j].started) })
	return out
}

// lookup resolves what a person typed. In order: nothing at all when exactly
// one run is live, an exact run id, a unique run-id prefix, then a queue with
// exactly one live run.
//
// Ambiguity is an error rather than a guess, and the error names the
// candidates: taking over the wrong agent mid-run is a great deal worse than
// being asked to type four more characters.
func (r *sessionRegistry) lookup(ref string) (*liveSession, error) {
	live := r.all()
	if len(live) == 0 {
		return nil, errors.New("no run is being driven on this machine right now. " +
			"`rein tail` shows what has run; `rein tail <run>` replays one")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		if len(live) == 1 {
			return live[0], nil
		}
		return nil, fmt.Errorf("%d runs are live — say which: %s", len(live), describeLive(live))
	}
	var prefix, queue []*liveSession
	for _, l := range live {
		switch {
		case l.runID == ref:
			return l, nil
		case strings.HasPrefix(l.runID, ref):
			prefix = append(prefix, l)
		}
		if l.queue == ref {
			queue = append(queue, l)
		}
	}
	switch {
	case len(prefix) == 1:
		return prefix[0], nil
	case len(prefix) > 1:
		return nil, fmt.Errorf("%q matches %d live runs: %s", ref, len(prefix), describeLive(prefix))
	case len(queue) == 1:
		return queue[0], nil
	case len(queue) > 1:
		return nil, fmt.Errorf("queue %q is driving %d runs: %s", ref, len(queue), describeLive(queue))
	}
	return nil, fmt.Errorf("no live run matches %q. Live now: %s", ref, describeLive(live))
}

func describeLive(live []*liveSession) string {
	names := make([]string, len(live))
	for i, l := range live {
		names[i] = runlog.Short(l.runID) + " (" + l.queue + ")"
	}
	return strings.Join(names, ", ")
}

// The control.Handler implementation. Every method is safe to call from the
// control server's own goroutine while the run loop is driving.

// List implements [control.Handler].
func (r *sessionRegistry) List(context.Context) ([]control.RunInfo, error) {
	live := r.all()
	out := make([]control.RunInfo, 0, len(live))
	for _, l := range live {
		out = append(out, l.info())
	}
	return out, nil
}

// Attach implements [control.Handler].
func (r *sessionRegistry) Attach(_ context.Context, ref string) (control.Attached, func(), error) {
	l, err := r.lookup(ref)
	if err != nil {
		return control.Attached{}, nil, err
	}
	return l.attach()
}

// Input implements [control.Handler].
func (r *sessionRegistry) Input(ctx context.Context, runID, text string) error {
	l, err := r.byID(runID)
	if err != nil {
		return err
	}
	return l.input(ctx, text)
}

// Respond implements [control.Handler].
func (r *sessionRegistry) Respond(_ context.Context, runID, id string, allow bool, reason string) error {
	l, err := r.byID(runID)
	if err != nil {
		return err
	}
	return l.respond(id, allow, reason)
}

// Detach implements [control.Handler]. The server calls the release it was
// given as well, so this only has to be idempotent.
func (r *sessionRegistry) Detach(_ context.Context, runID string) error {
	l, err := r.byID(runID)
	if err != nil {
		return nil
	}
	l.release()
	return nil
}

func (r *sessionRegistry) byID(runID string) (*liveSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.byRun[runID]
	if !ok {
		return nil, fmt.Errorf("run %s is no longer being driven — it finished, or the runner stopped",
			runlog.Short(runID))
	}
	return l, nil
}

// Control returns this runner's control-plane handler, for
// [control.Serve].
func (r *Runner) Control() control.Handler { return r.sessions }

// human records what a person did to one run, across however many attachments
// and sessions it took. It is what puts one line in the deliverable — and one
// line is all that goes there: what a person typed is in the local log and
// nowhere else, because a deliverable is read by whoever asked for the work
// and a takeover transcript is not theirs.
type human struct {
	attachments int
	sends       int
	held        time.Duration
}

func (qr *queueRunner) noteHuman(sends int, held time.Duration) {
	qr.humanMu.Lock()
	defer qr.humanMu.Unlock()
	qr.human.attachments++
	qr.human.sends += sends
	qr.human.held += held
}

// humanSummary is the deliverable's line, or "" when nobody intervened.
func (qr *queueRunner) humanSummary() string {
	qr.humanMu.Lock()
	h := qr.human
	qr.humanMu.Unlock()
	if h.attachments == 0 {
		return ""
	}
	msg := fmt.Sprintf("A person attached to this session for %s", h.held.Round(time.Second))
	if h.attachments > 1 {
		msg = fmt.Sprintf("A person attached to this session %d times, for %s in total",
			h.attachments, h.held.Round(time.Second))
	}
	switch h.sends {
	case 0:
		return msg + " and watched without sending anything."
	case 1:
		return msg + " and sent it one message."
	default:
		return msg + fmt.Sprintf(" and sent it %d messages.", h.sends)
	}
}
