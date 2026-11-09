package runner

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/worktree"
)

// DefaultReapRetryInterval is how often a worktree left in place is tried
// again. A cancelled run's stray process usually ends within minutes.
const DefaultReapRetryInterval = 5 * time.Minute

// reapOwed retries the reap of every finished run whose worktree was left in
// place — because a process was still working in it, or because the runner
// died before it got that far (ark:rein#99). It reports how many it removed.
//
// The runs to try come from the run logs, which already record the worktree
// and which runs are owed one, so nothing new has to survive a restart. A run
// still being driven is never in the list, so this cannot race a run's own
// reap; a worktree kept on purpose (--keep-worktrees) never acquires the mark.
func (r *Runner) reapOwed(ctx context.Context) int {
	if r.opts.Logs == nil || r.opts.Worktrees == nil || r.opts.Hosted || r.opts.DryRun || r.opts.KeepWorktrees {
		return 0
	}
	owed, err := r.opts.Logs.ReapOwed()
	if err != nil {
		r.logf("run logs: could not list worktrees owed a reap: %v", err)
		return 0
	}
	n := 0
	for _, m := range owed {
		if ctx.Err() != nil {
			return n
		}
		if path := reviewFile(r.opts.StateDir, m.RunID); path != "" {
			if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
				continue
			}
		}
		if r.reapedUnmarked[m.RunID] {
			// Reaped already; only the mark is left (below). Reaping again
			// would be a second reap of a worktree that is gone.
			r.clearReapMark(m)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := r.opts.Worktrees.Reap(rctx, &worktree.Worktree{
			Dir: m.Worktree, Repo: m.Repo, Branch: m.Branch, RunID: m.RunID,
		})
		cancel()
		switch {
		case err == nil:
			r.logf("run %s: reaped the worktree left in place, %s", m.RunID, m.Worktree)
			n++
			r.clearReapMark(m)
		case errors.Is(err, worktree.ErrInUse):
			// Still busy; next time.
		default:
			r.logf("run %s: could not reap %s: %v", m.RunID, m.Worktree, err)
		}
	}
	return n
}

// clearReapMark records that m's worktree is gone. Replacing the run's meta
// can fail for a moment (on Windows, while a reader has it open); the run is
// then remembered as reaped so the next pass only retries the mark and never
// reaps the same worktree twice. Only reapOwed's goroutine calls it.
func (r *Runner) clearReapMark(m runlog.Meta) {
	err := r.opts.Logs.ClearReapPending(m.RunID)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		delete(r.reapedUnmarked, m.RunID)
		return
	}
	if r.reapedUnmarked == nil {
		r.reapedUnmarked = map[string]bool{}
	}
	if !r.reapedUnmarked[m.RunID] {
		r.logf("run %s: reaped %s but could not clear the mark yet: %v", m.RunID, m.Worktree, err)
	}
	r.reapedUnmarked[m.RunID] = true
}

// retryReaps runs reapOwed once now and then on an interval until ctx ends.
func (r *Runner) retryReaps(ctx context.Context) {
	r.reapOwed(ctx)
	every := r.opts.ReapRetryInterval
	if every <= 0 {
		every = DefaultReapRetryInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reapOwed(ctx)
		}
	}
}
