package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/elk-work/rein/internal/elk"
)

// A claim that times out may still have claimed a run (Elk #1772). Elk stops
// waiting for claim_run after a few seconds and answers "it may still have
// claimed a run for you, call claim_run again", while the claim carries on and
// commits. Rein used to log that sentence and move on; the run then sat
// `running` under this queue with no agent, and the next heartbeat said
// nothing was waiting, because a run on a live lease is not waiting.
//
// So a claim that may have landed is asked again, with the same arguments, a
// bounded number of times. And when Elk answers that the queue is clear it
// also lists the runs this caller already holds on a live lease: on startup,
// and after any claim that may have landed, Rein takes each of those back by
// its run_id (which Elk answers with the run's work order again) and starts
// it, unless it is a run this queue is already working.

const (
	// ClaimAttempts bounds how many times one claim is asked before the loop
	// gives up until its next poll.
	ClaimAttempts = 4

	// DefaultClaimRetryBackoff is the wait before the first retry; each later
	// one waits twice as long as the last (2s, 4s, 8s).
	DefaultClaimRetryBackoff = 2 * time.Second
)

func (qr *queueRunner) claimRetryBackoff() time.Duration {
	if d := qr.r.opts.ClaimRetryBackoff; d > 0 {
		return d
	}
	return DefaultClaimRetryBackoff
}

// claimRun is the claim the serve loop makes: the queue's next run, or a run
// Elk already holds for this queue when one may have been orphaned.
func (qr *queueRunner) claimRun(ctx context.Context) (*elk.WorkOrder, error) {
	wo, err := qr.claimRetrying(ctx, elk.ClaimRequest{Workspace: qr.workspace, Queue: qr.q.Name})
	if elk.ClaimMayHaveLanded(err) {
		qr.lookForHeld = true
		return nil, err
	}
	var clear *elk.NoWorkError
	if !errors.As(err, &clear) || !qr.lookForHeld {
		return wo, err
	}
	for _, id := range clear.Held {
		if qr.alreadyReviewing(id) {
			continue
		}
		qr.logf("Elk holds run %s for this queue and nothing here is working it; asking for its work order again", id)
		held, herr := qr.claimRetrying(ctx, elk.ClaimRequest{Workspace: qr.workspace, RunID: id})
		switch {
		case herr == nil:
			// Stay looking: another held run, if any, is taken on the next
			// claim, and a clear answer with none left turns this off.
			return held, nil
		case elk.ClaimMayHaveLanded(herr):
			return nil, herr
		default:
			qr.logf("run %s could not be taken back: %v", id, herr)
		}
	}
	qr.lookForHeld = false
	return nil, err
}

// claimRetrying asks claim_run with the same arguments until it answers, up to
// [ClaimAttempts] times with a doubling wait, and says why it stopped.
func (qr *queueRunner) claimRetrying(ctx context.Context, req elk.ClaimRequest) (*elk.WorkOrder, error) {
	wait := qr.claimRetryBackoff()
	for attempt := 1; ; attempt++ {
		wo, err := qr.elk.ClaimRun(ctx, req)
		if !elk.ClaimMayHaveLanded(err) {
			if attempt > 1 {
				qr.logf("claim_run answered on attempt %d of %d", attempt, ClaimAttempts)
			}
			return wo, err
		}
		if attempt >= ClaimAttempts {
			qr.logf("stopped retrying claim_run after %d attempts: %s — the next poll asks again, and takes back any run Elk already holds for this queue",
				attempt, claimTimeoutReason(err))
			return nil, err
		}
		qr.logf("claim_run may have claimed a run without answering (attempt %d of %d): %s — asking again with the same arguments in %s",
			attempt, ClaimAttempts, claimTimeoutReason(err), span(wait))
		select {
		case <-ctx.Done():
			qr.logf("stopped retrying claim_run: Rein is shutting down")
			return nil, err
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// claimTimeoutReason is a claim that may have landed, in words safe to log
// and to show Elk: Elk's own sentence, or the status a gateway answered, never
// a transport error, which carries the connector URL and so the queue's token.
func claimTimeoutReason(err error) string {
	var (
		tool *elk.ToolError
		hErr *elk.HTTPError
	)
	switch {
	case errors.As(err, &tool):
		return firstLine(tool.Text)
	case errors.As(err, &hErr):
		return fmt.Sprintf("Elk answered %s", hErr.Status)
	}
	return "Elk's reply did not arrive in time"
}
