package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/worktree"
)

// Handing an interrupted run back to Elk.
//
// A run Rein stops driving without telling Elk stays `running` under this
// machine's claim until its lease lapses, fifteen minutes after the last
// report. On 2026-11-20 a `rein service stop` and `start` on fij-claude did
// exactly that to arun-0940104a: Rein stopped the agent, Elk went on showing
// the run in progress, and the Agents page had a fourth run beside the three
// Rein was really driving until the lease ran out.
//
// Elk's connector has no call that puts a `running` run back to `queued`
// (read in elk-work/scout on 2026-11-20: supabase/functions/elk-mcp/queue.ts
// offers claim_run, report_progress, ask_elk and submit_deliverable to a
// runner, and only the claim's own unanswered hold is ever released). What it
// has for a run whose runner stopped driving it is the stop code
// `claim_lapsed` on a `stuck` submit: migration 0782's recovery restarts such
// a stop on a ready queue, as a new run that carries this one as its parent,
// without asking a person. It is the code Elk itself gives a claim it finds
// lapsed. So the hand-back is two calls: a report saying the run was
// interrupted by a restart, then that submit. The run is never called done or
// failed, because it was neither.

// handBackStopCode is Elk's stop code for a run whose runner stopped driving
// it. Elk restarts a stop carrying it; see above.
const handBackStopCode = "claim_lapsed"

// handBackTimeout bounds the two calls. A service manager gives a stopping
// process a few seconds before it kills it, and the worktree is still to reap.
const handBackTimeout = 15 * time.Second

// handBackWindow is how recently a dead runner must have been writing a run's
// log for a new Rein to hand the run back at startup. Elk's lease is fifteen
// minutes from the last report and Rein reports at least every ten, so inside
// five minutes of the last sign of life the claim is certainly still this
// machine's. Past that the lease may have lapsed and the run been claimed
// again, by this queue on another machine as easily as not, and a submit then
// would end a run somebody is working. Elk's own lapse handling has it by then.
const handBackWindow = 5 * time.Minute

// handBackInterrupted hands the current run back when Rein is stopping and the
// run has no outcome: not submitted, not cancelled, not kept for a review that
// resumes on the next start. It is safe to call on every way out of a drive;
// it acts once, and only on a stop.
func (qr *queueRunner) handBackInterrupted(ctx context.Context, wo *elk.WorkOrder, wt *worktree.Worktree, pending *elk.Usage) {
	if ctx.Err() == nil || qr.handedBack || qr.logStatus != runlog.StatusInterrupted {
		return
	}
	if qr.r.opts.Hosted || qr.r.opts.DryRun {
		// A hosted container is one attempt at one run, and Elk starts
		// another; a dry run claimed nothing.
		return
	}
	qr.handedBack = true
	if !qr.started.Load() {
		// Elk's claim only holds a run until the runner first speaks on it
		// (scout 0790). This one was never started, so it is still queued:
		// saying anything now would start it only to stop it.
		qr.logf("run %s: Rein is stopping before the run started; Elk still has it queued", wo.RunID)
		return
	}
	branch := ""
	if wt != nil {
		branch = wt.Branch
	}
	err := qr.handBack(ctx, wo.RunID, branch,
		fmt.Sprintf("%s was stopped or restarted while this run was in progress.", qr.reinHere()), pending)
	switch {
	case err == nil:
		qr.logf("run %s: handed back to Elk as interrupted by a restart; Elk starts it again", wo.RunID)
		qr.log.Runner(runlog.KindSubmit, "handed back to Elk: interrupted by a Rein restart")
	case errors.Is(err, elk.ErrCancelled):
		qr.logf("run %s: nothing to hand back, Elk says the run is no longer running", wo.RunID)
	default:
		qr.logf("run %s: could not hand the run back to Elk (%s); its claim lapses in up to 15 minutes and Elk offers it again",
			wo.RunID, handBackFailure(err))
		qr.log.Runner(runlog.KindSubmit, "could not hand the run back to Elk; the claim will lapse")
	}
}

// handBackDead hands back the runs an earlier Rein on this machine was driving
// on this queue when it died without unwinding (a kill, a crash, power loss).
// A graceful stop has handed its own back already. It runs before the queue's
// first claim, so nothing here can be a run this process is driving.
func (qr *queueRunner) handBackDead(ctx context.Context) {
	if qr.r.opts.Hosted {
		return
	}
	label := qr.r.opts.Config.QueueLabel(qr.q)
	for _, d := range qr.r.deadRuns {
		if d.Queue != label || d.Workspace != qr.workspace || ctx.Err() != nil {
			continue
		}
		switch {
		case qr.alreadyReviewing(d.RunID):
			// Its review was saved and has just been resumed (review_state.go).
			continue
		case !d.Acknowledged && d.Worktree == "" && d.SessionID == "":
			// It never reached its first report, so Elk never started it.
			qr.logf("run %s: the Rein that claimed it stopped before starting it; Elk still has it queued", d.RunID)
			continue
		case time.Since(d.LastActivity) > handBackWindow:
			qr.logf("run %s: the Rein driving it stopped %s ago, so its claim may have lapsed already; left to Elk",
				d.RunID, span(time.Since(d.LastActivity)))
			continue
		}
		err := qr.handBack(ctx, d.RunID, d.Branch,
			fmt.Sprintf("%s started and found this run, which an earlier Rein on this machine was driving when it stopped.", qr.reinHere()), nil)
		switch {
		case err == nil:
			qr.logf("run %s: no longer driven on this machine; handed back to Elk as interrupted by a restart", d.RunID)
		case errors.Is(err, elk.ErrCancelled):
			qr.logf("run %s: no longer driven on this machine, and Elk says it is no longer running; nothing to hand back", d.RunID)
		default:
			qr.logf("run %s: could not hand the run back to Elk (%s); its claim lapses and Elk offers it again",
				d.RunID, handBackFailure(err))
		}
	}
}

// handBack tells Elk a run was interrupted by a restart and gives up the
// claim. what is the sentence saying how Rein came to stop driving it. It
// returns [elk.ErrCancelled] when the run was not running any more.
func (qr *queueRunner) handBack(ctx context.Context, runID, branch, what string, pending *elk.Usage) error {
	return qr.handBackAs(ctx, runID, branch, "Interrupted by a Rein restart", what+" The run was interrupted by the restart.", false, pending)
}

// handBackAs is [queueRunner.handBack] under a title of the caller's, for a
// stop that was not a restart (yield.go). pushed says the branch is on
// origin.
//
// ctx may already be cancelled: the calls are made on one that is not, bounded
// by [handBackTimeout].
func (qr *queueRunner) handBackAs(ctx context.Context, runID, branch, title, what string, pushed bool, pending *elk.Usage) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handBackTimeout)
	defer cancel()

	var usage *elk.Usage
	if pending.Valid() && !pending.Empty() {
		u := *pending
		usage = &u
	}
	// The line on the run's timeline. If Elk says the run is not running,
	// there is no claim left to give up.
	_, err := qr.elk.ReportProgress(ctx, elk.Progress{
		RunID: runID,
		Kind:  elk.ProgressStep,
		Body: qr.redactString(what + " It did not finish and it did not fail. " +
			"Rein is handing it back to Elk to be started again."),
		Usage: usage,
	})
	if errors.Is(err, elk.ErrCancelled) {
		return err
	}
	if err == nil && usage != nil {
		*pending = elk.Usage{}
	}

	body := "## " + title + "\n\n" + what + " It did not finish and it did not fail. " +
		"Rein handed it back so Elk can start it again on a ready agent, and nobody needs to do anything.\n\n"
	switch {
	case branch != "" && pushed:
		body += "Anything the agent had committed is pushed to branch `" + branch + "`. Changes it had not committed are gone with the run's folder.\n"
	case branch != "":
		body += "Anything the agent had committed is on branch `" + branch + "`. Changes it had not committed are gone with the run's folder.\n"
	default:
		body += "The run had no branch yet, so nothing is left of it on this machine.\n"
	}
	_, err = qr.elk.SubmitDeliverable(ctx, elk.Submission{
		RunID:       runID,
		Deliverable: qr.redactString(body),
		Status:      elk.StatusStuck,
		StopCode:    handBackStopCode,
		Title:       title,
	})
	return err
}

// reinHere names this Rein for a person reading the run in Elk.
func (qr *queueRunner) reinHere() string {
	if host := qr.r.opts.Config.HostID; host != "" {
		return "Rein on " + host
	}
	return "Rein"
}

// handBackFailure is why a hand-back failed, in words safe for the service
// log's neighbours: a transport error carries the connector URL, and the
// connector URL carries the queue's token.
func handBackFailure(err error) string {
	var tool *elk.ToolError
	var hErr *elk.HTTPError
	switch {
	case errors.As(err, &tool):
		return firstLine(tool.Text)
	case errors.As(err, &hErr):
		return "Elk answered " + hErr.Status
	case errors.Is(err, context.DeadlineExceeded):
		return "Elk did not answer in time"
	}
	return "Elk could not be reached"
}
