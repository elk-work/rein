package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/tasktrace"
)

// The machine's side of a task's time, as the run loop reports it to Signal
// (internal/tasktrace has the contract and the reasons).
//
// Four moments of a run are marked, each from where it is known to have
// happened and nowhere else:
//
//   - claim received: claimAndDrive, with the times the Elk client took
//     around the claim_run call itself;
//   - worktree ready: drive, the moment the run's folder exists;
//   - agent started: runSession, when the adapter's Start returns a session;
//   - deliverable sent: submit, when Elk has answered that it took one.
//
// A run that never reaches a moment sends no span for it. A run refused at
// preflight has a claim and a deliverable and nothing between, and that is
// what its trace shows.

// Environment variables that configure the spans, the names every other
// Stride emitter reads. They win over config.toml's [stride] block.
const (
	EnvStrideIngestURL   = "STRIDE_INGEST_URL"
	EnvStrideToken       = "STRIDE_TOKEN"
	EnvStrideEnvironment = "STRIDE_ENVIRONMENT"
)

// closeTraceWithin bounds the last send when the loop ends. A receiver that
// is down must not hold up a restart for longer than this.
const closeTraceWithin = 5 * time.Second

// ResolveTaskTrace works out where task-time spans go from config.toml's
// [stride] block and the environment, and says in one line what it decided.
// The line names no URL and no token.
//
// It never fails. A token that cannot be read is spans switched off and a
// sentence saying why, because a machine that cannot report a task's time is
// still a machine that can do the task.
func ResolveTaskTrace(c config.Stride, items keyring.ItemReader, getenv func(string) string, version string) (tasktrace.Config, string) {
	const off = "task time: not sent to Signal"
	if c.Disabled {
		return tasktrace.Config{}, off + " (stride.disabled)"
	}
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	out := tasktrace.Config{
		Endpoint:    firstSet(getenv(EnvStrideIngestURL), c.IngestURL),
		Token:       strings.TrimSpace(getenv(EnvStrideToken)),
		Environment: firstSet(getenv(EnvStrideEnvironment), c.Environment),
		Version:     version,
	}
	if out.Endpoint == "" {
		return tasktrace.Config{}, off + " (no stride.ingest_url)"
	}
	if out.Token == "" {
		item := strings.TrimSpace(c.Token)
		if item == "" {
			return tasktrace.Config{}, off + " (no stride.token names the keychain item holding the token)"
		}
		token, err := readStrideToken(item, items)
		if err != nil {
			return tasktrace.Config{}, off + " (" + err.Error() + ")"
		}
		out.Token = token
	}
	return out, "task time: each run's claim, worktree, agent start and deliverable are sent to Signal"
}

// readStrideToken reads the keychain item stride.token names. Its errors name
// the item and never a value.
func readStrideToken(item string, items keyring.ItemReader) (string, error) {
	service, account, err := config.ParseSecretItem(item)
	if err != nil {
		return "", fmt.Errorf("stride.token: %v", err)
	}
	if items == nil {
		return "", errors.New("no keychain reader to read stride.token with")
	}
	where := fmt.Sprintf("service %q, account %q", service, account)
	v, err := items.ReadItem(service, account)
	switch {
	case errors.Is(err, keyring.ErrItemNotFound):
		return "", fmt.Errorf("stride.token: no keychain item at %s", where)
	case err != nil:
		return "", fmt.Errorf("stride.token: the keychain item at %s could not be read", where)
	case strings.TrimSpace(v) == "":
		return "", fmt.Errorf("stride.token: the keychain item at %s is empty", where)
	}
	return strings.TrimSpace(v), nil
}

func firstSet(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// closeTaskTrace sends whatever spans are still waiting, as the loop ends.
func (r *Runner) closeTaskTrace() {
	if !r.opts.TaskTrace.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTraceWithin)
	defer cancel()
	r.opts.TaskTrace.Close(ctx)
}

// runTrace is one run's place in its task's trace: who it is, and where the
// last span ended, which is where the next one starts. One goroutine drives a
// run at a time (the serve loop, then the review continuation), so it needs no
// lock. Every method is safe on a nil *runTrace.
type runTrace struct {
	em  *tasktrace.Emitter
	run tasktrace.Run
	// last is the end of the last span sent.
	last time.Time
	// agentOwed is true from the worktree, or a revision round, until the
	// agent next starts. A restart after a stall, or the one more turn asked
	// for a screenshot, is the same agent carrying on and marks nothing.
	agentOwed bool
}

// newRunTrace is the trace state for wo, starting at start. Nil when spans are
// off, so a machine that sends none carries nothing per run either.
func (qr *queueRunner) newRunTrace(wo *elk.WorkOrder, start time.Time) *runTrace {
	em := qr.r.opts.TaskTrace
	if !em.Enabled() || wo == nil {
		return nil
	}
	return &runTrace{
		em: em,
		run: tasktrace.Run{
			TaskID:     wo.ActionID,
			RunID:      wo.RunID,
			Queue:      qr.q.Name,
			AgentKind:  qr.q.AgentKind,
			TaskNumber: wo.TaskNumber,
			Round:      1,
		},
		last: start,
	}
}

// traceClaim starts a claimed run's trace with the claim itself: from the
// moment this machine asked claim_run to the moment its reply arrived.
func (qr *queueRunner) traceClaim(wo *elk.WorkOrder) {
	qr.trace = qr.newRunTrace(wo, wo.Received)
	if qr.trace == nil {
		return
	}
	run := qr.trace.run
	run.Round = 0 // a claim belongs to no review round
	qr.trace.em.Span(tasktrace.OpClaimReceived, run, wo.Asked, wo.Received)
}

// mark sends op as the stretch from the last span's end to now.
func (t *runTrace) mark(op string, withRound bool) {
	if t == nil {
		return
	}
	now := time.Now()
	run := t.run
	if !withRound {
		run.Round = 0
	}
	t.em.Span(op, run, t.last, now)
	t.last = now
}

// worktreeReady marks the run's folder existing.
func (t *runTrace) worktreeReady() {
	if t == nil {
		return
	}
	t.mark(tasktrace.OpWorktreeReady, false)
	t.agentOwed = true
}

// agentStarted marks the agent process starting, once per round.
func (t *runTrace) agentStarted() {
	if t == nil || !t.agentOwed {
		return
	}
	t.agentOwed = false
	t.mark(tasktrace.OpAgentStarted, true)
}

// deliverableSent marks Elk having taken a deliverable.
func (t *runTrace) deliverableSent() {
	t.mark(tasktrace.OpDeliverableSent, true)
}

// revision begins a round Elk's review sent back: the agent starts again, and
// that start and the deliverable after it are measured from here.
func (t *runTrace) revision(round int) {
	if t == nil {
		return
	}
	t.run.Round = round
	t.last = time.Now()
	t.agentOwed = true
}
