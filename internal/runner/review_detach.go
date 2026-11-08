package runner

import (
	"context"
	"fmt"
	"sync"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/worktree"
)

// cleanupStack runs functions in reverse order, the way defers do. A review
// handoff takes the stack with it, so the worktree, the log and the run's
// proxy stay until Elk's review — and any revision — is finished, while the
// serve goroutine goes back to claiming.
type cleanupStack struct {
	mu  sync.Mutex
	fns []func()
}

func (c *cleanupStack) add(fn func()) {
	if fn == nil || c == nil {
		return
	}
	c.mu.Lock()
	c.fns = append(c.fns, fn)
	c.mu.Unlock()
}

// run runs whatever was added, once. A second call is a no-op.
func (c *cleanupStack) run() {
	if c == nil {
		return
	}
	c.mu.Lock()
	fns := c.fns
	c.fns = nil
	c.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

// driveState is whose fields the cleanup closures use. It starts as the
// queue's own runner and becomes the review continuation when the wait
// leaves the serve goroutine, so closeLog and reap hit the run they belong
// to rather than whatever that queue claimed next.
type driveState struct {
	cur      *queueRunner
	detached bool
}

// reviewCont is one run paused at Elk's review, with everything a revision
// round needs. It outlives the serve goroutine's drive call.
type reviewCont struct {
	round     int
	a         adapter.Adapter
	spec      adapter.RunSpec
	wo        *elk.WorkOrder
	wt        *worktree.Worktree
	repo      RepoResolution
	pending   *elk.Usage
	sentBack  string
	rec       runRecord
	sessionID string
	clean     *cleanupStack
}

// detachReview moves the review wait off the serve goroutine (ark:rein#65).
//
// v0.8.3 gave the slot back for the wait, which let another queue's run
// start. The wait itself stayed on this goroutine, and this goroutine is the
// queue's serve loop, so the queue claimed nothing of its own until the
// review answered — a Wrangler cycle behind a build under review, for up to
// the review timeout. The continuation polls, and runs a revision if Elk
// asks for one, on its own goroutine. The loop claims the next run.
func (qr *queueRunner) detachReview(ctx context.Context, c reviewCont, st *driveState) {
	child := qr.forkReview(c.wo.RunID)
	st.cur = child
	st.detached = true
	untrack := qr.trackReview(c.wo.RunID)
	qr.reviews.Add(1)
	go func() {
		defer qr.reviews.Done()
		defer untrack()
		child.continueReview(ctx, c)
	}()
}

// forkReview is a queueRunner that owns one run's mutable fields and shares
// the machine: the gate, the Elk client, the subscription. It must not be a
// copy of qr — that would copy locks that are already in use.
func (qr *queueRunner) forkReview(runID string) *queueRunner {
	child := &queueRunner{
		r:             qr.r,
		q:             qr.q,
		workspace:     qr.workspace,
		elk:           qr.elk,
		elkToken:      qr.elkToken,
		mode:          qr.mode,
		host:          qr.host,
		mcp:           qr.mcp,
		log:           qr.log,
		logStatus:     qr.logStatus,
		resultSummary: qr.resultSummary,
		reviewParent:  qr,
		handedRunID:   runID,
		subOwner:      qr,
	}
	child.redact.Store(qr.redact.Load())
	qr.humanMu.Lock()
	child.human = qr.human
	qr.humanMu.Unlock()
	qr.telMu.Lock()
	child.tel = qr.tel
	qr.telMu.Unlock()
	return child
}

// trackReview records that runID is already being reviewed on this queue, so
// a later claim of the same id — a test double that keeps handing it over, or
// a lease reclaim that races the poll — does not start a second agent on it.
func (qr *queueRunner) trackReview(runID string) func() {
	qr.reviewMu.Lock()
	if qr.reviewing == nil {
		qr.reviewing = map[string]struct{}{}
	}
	qr.reviewing[runID] = struct{}{}
	qr.reviewMu.Unlock()
	return func() {
		qr.reviewMu.Lock()
		delete(qr.reviewing, runID)
		qr.reviewMu.Unlock()
	}
}

// alreadyReviewing reports whether this queue's review continuation still
// holds runID.
func (qr *queueRunner) alreadyReviewing(runID string) bool {
	qr.reviewMu.Lock()
	defer qr.reviewMu.Unlock()
	_, ok := qr.reviewing[runID]
	return ok
}

// continueReview is the rest of driveWithReview, off the serve goroutine:
// poll until Elk's review says something, and run the agent again when it
// asks for revisions. Cleanup — the worktree, the log, the proxy — runs here,
// when the run is actually over.
func (qr *queueRunner) continueReview(ctx context.Context, c reviewCont) {
	defer c.clean.run()
	round := c.round
	spec := c.spec
	verdict := ""
	sentBack := c.sentBack
	rec := c.rec
	sessionID := c.sessionID
	// The serve goroutine already logged this wait and gave the slot back.
	announced := true
	for {
		if !announced {
			qr.logf("run %s: submitted; Elk is reviewing it. Waiting up to %s (round %d), with the slot given back",
				c.wo.RunID, qr.reviewTimeout(), round)
			qr.log.Runner(runlog.KindReview, "submitted; waiting up to %s for Elk's review (round %d), with the slot given back",
				qr.reviewTimeout(), round)
			qr.slot.release()
		}
		announced = false

		var outcome reviewOutcome
		verdict, outcome = qr.awaitReview(ctx, c.wo)
		switch outcome {
		case reviewInterrupted:
			// Rein is stopping while Elk still has the run (ark:rein#81). The
			// run stays `ready` in Elk, which is a legitimate place to leave
			// it; saying the review "asked for nothing" was not true.
			qr.logf("run %s: Rein stopped before Elk's review answered; the run stays ready in Elk, and a "+
				"revision request now finds no agent to work it", c.wo.RunID)
			qr.log.Runner(runlog.KindReview, "Rein stopped before Elk's review answered; the run stays ready")
			return
		case reviewCancelled:
			qr.logf("run %s: cancelled while Elk was reviewing — nothing further submitted", c.wo.RunID)
			qr.logStatus = runlog.StatusCancelled
			qr.log.Runner(runlog.KindReview, "cancelled while Elk was reviewing")
			return
		case reviewSettled:
			qr.logf("run %s: Elk's review asked for nothing; the run is the user's now", c.wo.RunID)
			qr.log.Runner(runlog.KindReview, "Elk's review asked for nothing; the run is the user's now")
			return
		}

		if round >= qr.maxReviewRounds() {
			// This deliverable replaces the agent's last one on the run, so
			// that one travels with it — last, as the agent wrote it. Work
			// that merged before the review gave up is otherwise invisible
			// to the person who now has to decide about it.
			err := qr.submitStuck(ctx, c.wo,
				fmt.Sprintf("Elk's review still wants changes after %d rounds", round),
				stuckBody("Rein stopped rather than loop. The last thing Elk asked for:\n\n"+
					blockquote(verdict)+"\n\n"+
					"A person should look at this: either the request is not something the agent can act on, "+
					"or the work needs a different approach.",
					qr.runDetails(c.wt, c.repo, spec, sessionID),
					"### What the agent submitted last", sentBack), c.pending)
			if err != nil {
				qr.logf("run %s: %v", c.wo.RunID, err)
			}
			return
		}
		if stop, err := qr.retakeSlot(ctx, c.wo, round+1, c.pending); stop {
			if err != nil {
				qr.logf("run %s: %v", c.wo.RunID, err)
			}
			return
		}
		qr.logf("run %s: Elk asked for revisions; running the agent again (round %d)", c.wo.RunID, round+1)
		qr.log.Runner(runlog.KindReview, "Elk asked for revisions; running the agent again (round %d)", round+1)
		_ = qr.report(ctx, c.wo.RunID, elk.ProgressStep,
			fmt.Sprintf("Elk's review asked for revisions (round %d of %d). Working them now.",
				round+1, qr.maxReviewRounds()), c.pending)
		spec = qr.revisionSpec(c.a, spec, c.wo, sessionID, verdict)

		round++
		qr.logStatus = runlog.StatusInterrupted
		res, err := qr.runSession(ctx, c.a, spec, c.wo, c.wt, c.repo, c.pending)
		if err != nil {
			qr.logf("run %s: %v", c.wo.RunID, err)
			return
		}
		if res.done {
			return
		}
		rec.add(res.out)
		res, err = qr.secureDeliverable(ctx, c.a, spec, c.wo, c.wt, c.repo, res, verdict, sentBack, &rec, c.pending)
		if err != nil {
			qr.logf("run %s: %v", c.wo.RunID, err)
			return
		}
		if res.done {
			return
		}
		sub, err := qr.submitReady(ctx, c.wo, res.out, c.wt, c.repo, spec, res.sessionID, c.pending)
		if err != nil {
			qr.logf("run %s: %v", c.wo.RunID, err)
			return
		}
		if sub == nil || !sub.ReviewPending {
			return
		}
		sentBack = agentAccount(res.out)
		sessionID = res.sessionID
	}
}
