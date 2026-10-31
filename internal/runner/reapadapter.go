package runner

import (
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// reapAdapter lets an adapter tidy up what it keeps outside the worktree —
// the check adapter's records and act's scratch directories — at the moment
// the worktree is reaped, for the reason the reaper runs there at all: the
// cheapest moment to tidy up after a run is the moment one finishes, and
// nothing else is going to remember (ark:rein#91).
//
// It is deferred beside [queueRunner.reap], so it runs before [closeLog]: the
// run that is ending is still `running` in its meta and so is one of the live
// runs, which is what keeps its own record out of reach.
//
// Which runs are live is what the run logs say, exactly as [runlog.Store.Prune]
// reads it. With no log store there is no way to tell, and then nothing is
// removed: an adapter that guessed could delete a directory another queue's
// run is working in.
func (qr *queueRunner) reapAdapter(a adapter.Adapter, runID string, spec *adapter.RunSpec) {
	rr, ok := a.(adapter.RunReaper)
	if !ok || qr.r.opts.Hosted || qr.r.opts.Logs == nil {
		return
	}
	active, err := qr.r.opts.Logs.Active()
	if err != nil {
		qr.logf("%s: not tidying up after run %s, because the live runs could not be read: %v", a.Name(), runID, err)
		return
	}
	live := make(map[string]time.Time, len(active)+1)
	for _, m := range active {
		live[m.RunID] = m.StartedAt
	}
	if _, ok := live[runID]; !ok {
		// Its meta could not be written; it is live all the same.
		qr.telMu.Lock()
		live[runID] = qr.tel.started
		qr.telMu.Unlock()
	}
	lines, err := rr.ReapRun(*spec, adapter.ReapPolicy{
		KeepRuns: qr.r.opts.LogKeepRuns,
		Live:     live,
		Now:      time.Now(),
	})
	for _, l := range lines {
		qr.logf("%s: %s", a.Name(), l)
	}
	if err != nil {
		qr.logf("%s: tidying up after run %s: %v", a.Name(), runID, err)
	}
}
