package runner

import (
	"context"
	"sort"
	"strings"

	"github.com/elk-work/rein/internal/control"
)

// What the running service is doing right now, for `rein status`.
//
// Elk's reading is one state a queue, and "running" there covers three things
// a person debugging a quiet machine has to tell apart: an agent process at
// work, a run parked with Elk's review that holds no slot and costs nothing,
// and a run or a claim standing in line for a slot. This says which, run by
// run, and beside it the two numbers that decide whether the next run starts:
// the slots the machine can afford, and how many runs the queue's own
// `parallel` setting lets it drive.

// The states a run can be in on this machine. They are sentences a person
// reads in `rein status`, not a vocabulary anything parses.
const (
	// ActivitySession is an agent process at work on the run.
	ActivitySession = "agent session"
	// ActivityBetween is a run that holds a slot with no agent process: just
	// claimed and being set up, or between a session and its submit.
	ActivityBetween = "holding a slot, no agent process"
	// ActivityReview is a run submitted and waiting on Elk's review. It
	// holds no slot.
	ActivityReview = "with Elk's review, no slot held"
	// ActivityRevisionWait is a run Elk's review sent back, in line for a
	// slot to work the revisions.
	ActivityRevisionWait = "waiting for a slot to work Elk's revisions"
)

// controlHandler is the runner's side of the control plane: the session
// registry, which is everything `rein attach` needs, plus the status report.
type controlHandler struct {
	*sessionRegistry
	r *Runner
}

// Status implements [control.Reporter].
func (h controlHandler) Status(context.Context) (control.Status, error) {
	return h.r.status(), nil
}

// status is the whole machine's activity, queue by queue.
func (r *Runner) status() control.Status {
	holds, waiting, total := r.gate.snapshot()
	st := control.Status{
		Version:      r.opts.Version,
		SlotsCeiling: r.opts.MaxConcurrent,
		SlotsNow:     total,
		SlotsUsed:    len(holds),
		Draining:     r.isDraining(),
	}
	live := r.sessions.all()

	r.qrsMu.Lock()
	qrs := append([]*queueRunner(nil), r.qrs...)
	r.qrsMu.Unlock()
	for _, qr := range qrs {
		label := r.opts.Config.QueueLabel(qr.q)
		q := control.QueueStatus{Queue: label, Parallel: qr.parallel, Driving: qr.drivingCount()}
		seen := map[string]bool{}

		for _, l := range live {
			if l.queue != label {
				continue
			}
			seen[l.runID] = true
			q.Runs = append(q.Runs, control.RunActivity{RunID: l.runID, State: ActivitySession, Since: l.started})
		}
		for _, h := range holds {
			if h.queue != label || h.runID == "" || seen[h.runID] {
				continue
			}
			seen[h.runID] = true
			q.Runs = append(q.Runs, control.RunActivity{RunID: h.runID, State: ActivityBetween, Since: h.since})
		}
		for _, w := range waiting {
			if w.queue != label {
				continue
			}
			reason := strings.TrimPrefix(w.reason, slotWaitPrefix)
			if w.runID == "" {
				// A claim: the run is not known until it has a slot.
				q.ClaimWaiting, q.ClaimWaitingSince = orWords(reason, "in line for a slot"), w.since
				continue
			}
			if seen[w.runID] {
				continue
			}
			seen[w.runID] = true
			q.Runs = append(q.Runs, control.RunActivity{RunID: w.runID, State: ActivityRevisionWait, Since: w.since, Detail: reason})
		}
		qr.reviewMu.Lock()
		for id, since := range qr.reviewing {
			if !seen[id] {
				q.Runs = append(q.Runs, control.RunActivity{RunID: id, State: ActivityReview, Since: since})
			}
		}
		qr.reviewMu.Unlock()
		sort.SliceStable(q.Runs, func(i, j int) bool { return q.Runs[i].Since.Before(q.Runs[j].Since) })

		// Why the queue is not starting work Elk counts on it, when that is
		// not a claim in line for a slot, which is said above.
		if why := qr.waiting(); why != "" && !strings.HasPrefix(why, slotWaitPrefix) {
			q.NotClaiming = why
		}
		st.Queues = append(st.Queues, q)
	}
	return st
}

func orWords(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
