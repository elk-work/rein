package runner

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/elk-work/rein/internal/elk"
)

// Reconnect from the Elk app (ark:scout#4417).
//
// A person presses Reconnect on a worker's card, on any computer, and nothing
// is copied or pasted: Elk says so on this queue's next heartbeat, Rein asks for
// a replacement with reconnect_own_credential, stores it, and carries on. The
// old credential keeps working until the new one is first used, or for a day,
// so nothing is lost if this computer stops halfway: Elk keeps asking, and a
// later attempt replaces the replacement nobody used.
//
// CapabilityReconnect is what tells Elk this Rein can do it, so an older Rein is
// never asked and the app falls back to handing the person the new connection.

// CapabilityReconnect is declared on every heartbeat by a Rein that acts on
// Elk's reconnect request.
const CapabilityReconnect = "rein-reconnect"

// reconnectRetry is how long a failed attempt waits before the next one, so a
// keychain that stays locked does not mint a credential on every beat.
const reconnectRetry = 10 * time.Minute

// reconnectState is one queue's pending request, set by whichever clock sent the
// beat and acted on only by the queue's own loop.
type reconnectState struct {
	wanted atomic.Bool
	// nextTry is touched only by the queue's own loop.
	nextTry time.Time
}

// noteReconnect records what this beat's reply said. It is overwritten every
// beat, so a request withdrawn in Elk stops here too.
func (qr *queueRunner) noteReconnect(hb *elk.Heartbeat) {
	qr.reconnect.wanted.Store(hb.Reconnect)
}

// applyReconnect replaces this queue's credential if Elk asked. The queue's loop
// calls it between runs, and only there: a run's connector holds the token it
// started with, and the new credential's first use ends the old one, so swapping
// under a live run would cut that run off from Elk.
//
// Store first and read it back before switching. A credential the keychain did
// not keep is a credential this computer would lose at its next restart, after
// the old one has already ended.
func (qr *queueRunner) applyReconnect(ctx context.Context) {
	if !qr.reconnect.wanted.Load() || qr.r.opts.Hosted || qr.r.opts.Store == nil {
		return
	}
	now := time.Now()
	if now.Before(qr.reconnect.nextTry) {
		return
	}
	if err := qr.swapCredential(ctx); err != nil {
		qr.reconnect.nextTry = now.Add(reconnectRetry)
		if ctx.Err() == nil {
			// Never the credential: errors here name a step, not a value. The
			// old credential still works, so this is a note, not a stop.
			qr.logf("reconnect: Elk asked for a new connection and it did not finish, trying again in %d minutes: %v",
				int(reconnectRetry.Minutes()), err)
		}
		return
	}
	qr.reconnect.wanted.Store(false)
	qr.reconnect.nextTry = time.Time{}
	qr.logf("reconnect: this queue now uses a new connection to Elk")
}

func (qr *queueRunner) swapCredential(ctx context.Context) error {
	got, err := qr.elk.ReconnectOwnCredential(ctx)
	if err != nil {
		return err
	}
	if got.Already {
		return nil
	}
	if err := qr.r.opts.Store.Set(qr.workspace, qr.q.Name, got.Token); err != nil {
		return errors.New("the new connection could not be stored in this computer's keychain")
	}
	back, err := qr.r.opts.Store.Get(qr.workspace, qr.q.Name)
	if err != nil || back != got.Token {
		return errors.New("the keychain did not keep the new connection")
	}
	qr.elk.SetToken(got.Token)
	qr.elkToken = got.Token
	return nil
}
