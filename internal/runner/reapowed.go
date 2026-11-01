package runner

import (
	"context"
	"errors"
	"os"
	"time"

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
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := r.opts.Worktrees.Reap(rctx, &worktree.Worktree{
			Dir: m.Worktree, Repo: m.Repo, Branch: m.Branch, RunID: m.RunID,
		})
		cancel()
		switch {
		case err == nil:
			if cerr := r.opts.Logs.ClearReapPending(m.RunID); cerr != nil && !errors.Is(cerr, os.ErrNotExist) {
				r.logf("run %s: reaped %s but could not clear the mark: %v", m.RunID, m.Worktree, cerr)
			}
			r.logf("run %s: reaped the worktree left in place, %s", m.RunID, m.Worktree)
			n++
		case errors.Is(err, worktree.ErrInUse):
			// Still busy; next time.
		default:
			r.logf("run %s: could not reap %s: %v", m.RunID, m.Worktree, err)
		}
	}
	return n
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
