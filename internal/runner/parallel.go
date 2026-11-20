package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
)

// A queue that may drive more than one run at once.
//
// Until this, a queue's serve loop claimed a run and drove it on its own
// goroutine, so a machine with free slots and one queue did one thing at a
// time: the second and third slots were only ever for other queues, for a
// Wrangler cycle, or for a run back from Elk's review. `parallel` in a queue's
// config.toml block lets the loop hand each claimed run to a fork and go back
// to claiming, while fewer than that many are being driven.
//
// Two numbers decide whether a run starts, and they are kept apart on purpose:
//
//   - what the MACHINE can afford: the slot gate (slotgate.go), measured
//     against live headroom, one line for every queue, with Wrangler and
//     revision priority. Nothing here changes it. A parallel queue takes a
//     slot for every claim exactly as a serial one does, and joins the back
//     of the line again for its next, so another queue's waiting claim goes
//     ahead of it.
//   - what the QUEUE allows: `parallel`, the bound on runs this queue's loop
//     has being driven. A queue at its bound claims nothing more whatever
//     slots are free.
//
// What the bound counts is a run from its claim until its drive returns: the
// agent finished and Rein submitted, or the run was handed to its wait on
// Elk's review. A run waiting on Elk's review holds no slot and is not
// counted, and neither is a run the review sent back for revisions: that one
// takes a slot at revision priority, ahead of any new claim, as it always has
// (review_detach.go). So `parallel = 1` is exactly the loop as it was.
//
// What it does NOT do is order work. Each run has a worktree of its own, which
// keeps two agents out of each other's files and nothing more: it says nothing
// about which merges first, or whether one task needed the other's result.
// Order is Elk's, at the claim. As read in elk-work/scout on 2026-11-20
// (supabase/functions/elk-mcp/queue.ts): claim_run offers only the first
// unfinished step of a sequential group, never hands out a second run on a
// task another session is working, and counts nothing per queue, so a queue
// may hold several runs. It does not look at links between different tasks.
// Two tasks that merely depend on each other can therefore both be claimable,
// which is why the setting is an opt-in for queues whose work is independent,
// and why the default stays at one.

// parallelFor is how many runs q's loop may have being driven at once.
//
// Never more than the machine's slot ceiling: a bound above it could not be
// reached and would only mislead whoever reads it. And one under --once, which
// claims a single run and waits for it, and in a hosted container, which
// exists for exactly one run.
func (r *Runner) parallelFor(q config.Queue) int {
	if r.opts.Once || r.opts.Hosted || r.opts.DryRun {
		return 1
	}
	n := q.ParallelOrDefault()
	if r.opts.MaxConcurrent > 0 && n > r.opts.MaxConcurrent {
		n = r.opts.MaxConcurrent
	}
	return n
}

// parallelLine is the start-up line that says what this queue allows, apart
// from what the machine can afford.
func (qr *queueRunner) parallelLine() string {
	set := qr.q.ParallelOrDefault()
	switch {
	case set > 1 && (qr.r.opts.Once || qr.r.opts.Hosted || qr.r.opts.DryRun):
		return fmt.Sprintf("parallel: one run at a time. config.toml says parallel = %d, which does not apply to "+
			"--once, --dry-run or a hosted run", set)
	case set > qr.parallel:
		return fmt.Sprintf("parallel: up to %s at once on this queue. config.toml says parallel = %d, held to "+
			"this machine's ceiling of %d (--max-concurrent). Each run still starts only when the machine has a free slot",
			runs(qr.parallel), set, qr.parallel)
	case qr.parallel <= 1:
		return "parallel: one run at a time on this queue (config.toml parallel is 1 or not set)"
	}
	return fmt.Sprintf("parallel: up to %d runs at once on this queue (config.toml parallel = %d). "+
		"Each run still starts only when the machine has a free slot; the ceiling is %d",
		qr.parallel, set, qr.r.opts.MaxConcurrent)
}

// root is the queue's own runner: this one, or the one this fork drives a run
// for. What belongs to the queue rather than to a run is read and written
// there, because the serve loop and the heartbeat look nowhere else.
func (qr *queueRunner) root() *queueRunner {
	if qr.owner != nil {
		return qr.owner
	}
	return qr
}

// drivenRun is one run a queue handed to a fork.
type drivenRun struct {
	runID string
	since time.Time
	fork  *queueRunner
}

// drivingCount is how many runs this queue is driving beside its loop.
func (qr *queueRunner) drivingCount() int {
	qr.drivingMu.Lock()
	defer qr.drivingMu.Unlock()
	return len(qr.driving)
}

// alreadyDriving reports whether a fork of this queue is driving runID now.
// A claim that hands it over again (a take-back of a run Elk lists as held,
// a test double that repeats itself) must not start a second agent on it.
func (qr *queueRunner) alreadyDriving(runID string) bool {
	qr.drivingMu.Lock()
	defer qr.drivingMu.Unlock()
	for _, d := range qr.driving {
		if d.runID == runID {
			return true
		}
	}
	return false
}

// oldestDriving is the fork driving the run this queue has had longest, or
// nil. It is the run the queue's one fleet reading describes (telemetry.go).
func (qr *queueRunner) oldestDriving() *queueRunner {
	qr.drivingMu.Lock()
	defer qr.drivingMu.Unlock()
	if len(qr.driving) == 0 {
		return nil
	}
	return qr.driving[0].fork
}

// drivingRuns is a copy of what this queue is driving, oldest first.
func (qr *queueRunner) drivingRuns() []drivenRun {
	qr.drivingMu.Lock()
	defer qr.drivingMu.Unlock()
	return append([]drivenRun(nil), qr.driving...)
}

// parallelHold is why a queue already driving busy runs claims nothing more
// right now, or "" when it may claim.
//
// It is the queue's own limit and says so: the sentence a person otherwise
// reads is the gate's, about slots, and on a machine with slots free that
// would send them looking at memory for something config.toml decided.
func (qr *queueRunner) parallelHold(busy int) string {
	if busy >= qr.parallel {
		return fmt.Sprintf("not claiming: this queue is driving %d runs, as many as its parallel setting in "+
			"config.toml allows (%d). That is the queue's own limit, not the machine's: free slots do not change it",
			busy, qr.parallel)
	}
	if qr.reconnect.wanted.Load() && !time.Now().Before(qr.reconnect.nextTry) &&
		!qr.r.opts.Hosted && qr.r.opts.Store != nil {
		// A new credential ends the old one at its first use, and the runs
		// being driven hold the old one (reconnect.go). A serial queue is
		// between runs at every poll; this one has to make the gap.
		return fmt.Sprintf("not claiming: Elk asked this queue for a new connection, and it takes it once the %s "+
			"it is driving finished", map[bool]string{true: "1 run has", false: runs(busy) + " have"}[busy == 1])
	}
	return ""
}

// stoppedClaiming reports whether the queue must not claim right now for a
// reason a run can discover mid-flight: the agent needs signing in, or the
// subscription has no headroom.
func (qr *queueRunner) stoppedClaiming() bool {
	qr.telMu.Lock()
	login := qr.loginBlocked
	qr.telMu.Unlock()
	if login {
		return true
	}
	_, _, held := qr.holding()
	return held
}

// forkRun is a queueRunner that drives one run for this queue: the queue's
// configuration and shared clients, and per-run fields of its own. It must not
// be a copy of qr, which would copy locks that are in use.
func (qr *queueRunner) forkRun() *queueRunner {
	root := qr.root()
	return &queueRunner{
		r:         qr.r,
		q:         qr.q,
		workspace: qr.workspace,
		elk:       qr.elk,
		elkToken:  qr.elkToken,
		mode:      qr.mode,
		host:      qr.host,
		mcp:       qr.mcp,
		owner:     root,
		subOwner:  root,
		parallel:  1,
	}
}

// driveBeside hands a claimed run to a fork and returns, so the serve loop can
// claim the queue's next. The fork takes the run's slot, its place in the
// trace and its place in the drain count with it, and gives each back when the
// drive returns.
//
// It is called on the serve goroutine, which is what makes the registration
// safe to rely on: by the time the loop claims again, the run is listed.
func (qr *queueRunner) driveBeside(ctx context.Context, wo *elk.WorkOrder, hold *slotHold) {
	w := qr.forkRun()
	w.slot, w.trace = hold, qr.trace
	qr.slot, qr.trace = nil, nil

	qr.drivingMu.Lock()
	qr.driving = append(qr.driving, drivenRun{runID: wo.RunID, since: time.Now(), fork: w})
	n := len(qr.driving)
	qr.drivingMu.Unlock()
	qr.logf("run %s: driving it beside the queue's loop (%d of %d this queue may drive at once)", wo.RunID, n, qr.parallel)

	qr.drives.Add(1)
	go func() {
		defer qr.drives.Done()
		// In the order a serial drive gives things back, and the slot first:
		// the loop this wakes must find it free.
		defer qr.r.endClaim()
		defer qr.driveOver(wo.RunID)
		defer hold.release()
		if err := w.drive(ctx, wo); err != nil {
			w.logf("run %s: %v", wo.RunID, err)
		}
	}()
}

// driveOver takes a run off the list of what this queue is driving and wakes
// the loop, which may now have room to claim.
func (qr *queueRunner) driveOver(runID string) {
	qr.drivingMu.Lock()
	for i, d := range qr.driving {
		if d.runID == runID {
			qr.driving = append(qr.driving[:i], qr.driving[i+1:]...)
			break
		}
	}
	qr.drivingMu.Unlock()
	select {
	case qr.driveEnded <- struct{}{}:
	default:
	}
}

// runs is "1 run" or "3 runs".
func runs(n int) string {
	if n == 1 {
		return "1 run"
	}
	return fmt.Sprintf("%d runs", n)
}
