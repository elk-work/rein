package runner

import (
	"context"
	"errors"
	"time"

	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/power"
	"github.com/elk-work/rein/internal/runlog"
)

// A Mac that sleeps under a run (ark:rein#90).
//
// Issac's MacBook, asleep on battery with the lid closed, wakes for about 45
// seconds every 4 to 18 minutes. Rein claimed runs in those wakes; the
// worktree step then took 20 to 30 minutes of wall time with under two of them
// awake, and the claude adapter's idle guard, which measured wall time, killed
// the agent for silence across one long sleep. Four PR sweeps and a Wrangler
// cycle ended "The agent failed" that way between 10-16 and 10-24.
//
// No assertion keeps a closed-lid Mac on battery awake, so there are two
// halves and both are needed:
//
//   - **Keep an open or docked Mac awake for the run.** [power.Assertion]
//     holds PreventUserIdleSystemSleep from the claim to the end of the run.
//     It is taken only once a work order is in hand, never for a claim that
//     comes back empty, so an idle queue polling Elk does not spawn and kill
//     caffeinate every thirty seconds.
//
//   - **Measure the run in awake time, and renew its lease on waking.** Every
//     timeout a run is subject to — the adapter's idle, startup, total and
//     background-work clocks, the run loop's stall timer, the 10-minute lease
//     ticker, the slot clock — runs on time that stops while the machine
//     sleeps. That makes a sleep cost the run nothing, but it also means the
//     lease ticker cannot fire during one, while Elk's 15-minute lease is wall
//     time on the server. So the moment the machine wakes, every run in flight
//     reports, inside the same maintenance wake.
//
// What a lapsed lease costs, read from elk-mcp: report_progress's guarded
// touch checks only that the run is still `running`, so a report after a long
// sleep renews it; nothing on the server requeues a lapsed run by itself. The
// one exposure is claim_run, which may hand a lapsed run to another claimer on
// the same queue while this machine sleeps. Reporting on every wake keeps that
// window to a single stretch of sleep longer than fifteen minutes.

// wakeRenewAttempts and wakeRenewRetry bound the lease renewal after a wake.
// A maintenance wake lasts about 45 seconds and the network can take a few of
// them to come back, so the renewal tries a few times, close together, and
// then leaves the lease to the run's own reports.
const wakeRenewAttempts = 4

var wakeRenewRetry = 5 * time.Second

// holdAcrossSleep holds the power assertion and renews runID's lease on every
// wake until the returned function is called, which it must be exactly once,
// when the run is over.
func (qr *queueRunner) holdAcrossSleep(ctx context.Context, runID string) (release func()) {
	qr.r.awake.Hold()
	if qr.r.sleep == nil {
		return qr.r.awake.Release
	}
	wakes, unsubscribe := qr.r.sleep.Subscribe()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case w := <-wakes:
				qr.renewAfterSleep(ctx, runID, w)
			}
		}
	}()
	return func() {
		cancel()
		<-done
		unsubscribe()
		qr.r.awake.Release()
	}
}

// renewAfterSleep reports on runID once the machine is awake again, which
// renews its lease and puts the sleep on the run's timeline.
//
// It passes no usage: the pending delta belongs to the watch loop, and a
// renewal running beside it must not touch it. A run that is no longer
// running — cancelled while the machine slept, or waiting on Elk's review —
// answers "Not recorded"; that is left to the run's own next report, which
// reads the same answer and acts on it.
func (qr *queueRunner) renewAfterSleep(ctx context.Context, runID string, w power.Wake) {
	slept := span(w.Slept)
	qr.logf("run %s: this machine slept for %s; the run's clocks were paused, and Rein is renewing its lease", runID, slept)
	qr.log.Runner(runlog.KindNote, "this machine slept for %s; the run's clocks were paused", slept)
	body := "This machine slept for " + slept + ". Rein paused the run's clocks while it slept, and the run carries on."
	for attempt := 1; ; attempt++ {
		err := qr.report(ctx, runID, elk.ProgressStep, body, nil)
		if err == nil || errors.Is(err, elk.ErrCancelled) || ctx.Err() != nil {
			return
		}
		if attempt >= wakeRenewAttempts {
			qr.logf("run %s: could not renew the lease after the machine woke: %v", runID, err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wakeRenewRetry):
		}
	}
}

// beatAfterSleep tells Elk the machine is up again after every sleep, for
// every queue. The telemetry tick would get there, but on awake time: a
// minute of it, which a 45-second maintenance wake never has, so a machine
// holding runs would read as offline in list_executors all night.
func (r *Runner) beatAfterSleep(ctx context.Context) {
	wakes, stop := r.sleep.Subscribe()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case w := <-wakes:
			r.logf("this machine slept for %s", span(w.Slept))
			for _, qr := range r.qrs {
				for attempt := 1; ctx.Err() == nil; attempt++ {
					if _, ok := qr.heartbeat(ctx); ok || attempt >= wakeRenewAttempts {
						break
					}
					select {
					case <-ctx.Done():
					case <-time.After(wakeRenewRetry):
					}
				}
			}
		}
	}
}
