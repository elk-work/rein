package runner

import (
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/worktree"
)

// The run loop's own half of the event log: opening it, recording the
// lifecycle around the agent's events, and closing it with an outcome.
//
// Every call here is unconditional. [runlog.Writer] is a working no-op on a
// nil receiver and never returns an error, so a runner with logging disabled,
// and a runner whose disk is full, take exactly the same path as one whose log
// is being watched. That is the point: a log must not be able to fail a run,
// and the only reliable way to guarantee that is to give the call sites no
// error to get wrong.

// openLog starts the current run's event log.
func (qr *queueRunner) openLog(wo *elk.WorkOrder) {
	qr.logStatus = runlog.StatusInterrupted
	qr.humanMu.Lock()
	qr.human = human{}
	qr.humanMu.Unlock()
	// The fleet reading's run starts here, not at the first session: a claimed
	// run with no agent process yet is this queue's, and reporting it idle
	// would be wrong for the whole of the preflight.
	qr.startRun(wo.RunID)
	qr.log = qr.r.opts.Logs.Create(runlog.Meta{
		RunID:          wo.RunID,
		Queue:          qr.r.opts.Config.QueueLabel(qr.q),
		Workspace:      qr.workspace,
		AgentKind:      qr.q.AgentKind,
		Direction:      firstLine(wo.Direction),
		PermissionMode: string(qr.mode),
		Version:        qr.r.opts.Version,
	})
	qr.log.Runner(runlog.KindClaimed, "claimed on queue %s — %s", qr.q.Name, firstLine(wo.Direction))
}

// closeLog settles the log and prunes the oldest.
//
// The fallback status is `interrupted` rather than anything more optimistic:
// a drive that returns without having recorded an outcome is one that was cut
// short, and the log should say so rather than leaving a run that looks like
// it is still going forever.
func (qr *queueRunner) closeLog() {
	// And it ends here, wherever inside drive the run got to. `stuck` is the
	// only ending that leaves the queue reporting `faulted` afterwards; see
	// [queueRunner.endRun].
	qr.endRun(qr.logStatus)
	qr.log.Close(qr.logStatus)
	// The run is over and its values with it; the next run arms its own.
	defer qr.redact.Store(nil)
	if err := qr.log.Err(); err != nil {
		// Once, at the end, rather than per record: a log that cannot be
		// written fails on every line, and a runner that said so every time
		// would drown its own log in the complaint.
		qr.logf("run log: %v", err)
	}
	if keep := qr.r.opts.LogKeepRuns; keep > 0 {
		if n, err := qr.r.opts.Logs.Prune(keep); err != nil {
			qr.logf("pruning run logs: %v", err)
		} else if n > 0 {
			qr.logf("pruned %d old run log(s), keeping %d", n, keep)
		}
	}
	qr.log = nil
}

// recordWorktree fills in the facts a live view needs and the log's reader
// will still want when the directory is gone.
func (qr *queueRunner) recordWorktree(wt *worktree.Worktree, repo RepoResolution) {
	qr.log.Update(func(m *runlog.Meta) {
		m.Repo = repo.Path
		m.Worktree = wt.Dir
		m.Branch = wt.Branch
	})
	qr.log.Runner(runlog.KindWorktree, "worktree %s on branch %s from %s (%s)",
		wt.Dir, wt.Branch, wt.BaseRef, shortSHA(wt.BaseSHA))
}
