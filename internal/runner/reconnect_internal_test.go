package runner

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/keyring"
)

const (
	oldCred = "old-credential-value"
	newCred = "new-credential-value"
)

// flakyStore is a keyring that can refuse a write or lose it.
type flakyStore struct {
	keyring.Store
	refuseSet bool
	loseSet   bool
}

func (f *flakyStore) Set(w, q, t string) error {
	if f.refuseSet {
		return errors.New("the keychain is locked, value " + t)
	}
	if f.loseSet {
		return nil
	}
	return f.Store.Set(w, q, t)
}

type reconnectRig struct {
	qr    *queueRunner
	srv   *elktest.Server
	store *flakyStore
	logs  *strings.Builder
}

func newReconnectRig(t *testing.T) *reconnectRig {
	t.Helper()
	srv := elktest.New(t, oldCred)
	srv.Reply(elk.ToolReconnectOwnCredential, elktest.Reply{
		Text:       "A new connection is ready.",
		Structured: map[string]any{"token": newCred, "expires_at": "2027-02-01T00:00:00Z"},
	})
	store := &flakyStore{Store: keyring.NewFileStore(filepath.Join(t.TempDir(), "tokens.json"))}
	if err := store.Store.Set("Elk Scout", "mac-claude", oldCred); err != nil {
		t.Fatal(err)
	}
	client, err := elk.New(srv.URL, oldCred, elk.WithoutInitialize())
	if err != nil {
		t.Fatal(err)
	}
	logs := &strings.Builder{}
	r := &Runner{opts: Options{Store: store, Out: logs, Config: config.Config{}}}
	qr := &queueRunner{r: r, q: config.Queue{Name: "mac-claude"}, workspace: "Elk Scout", elk: client, elkToken: oldCred}
	return &reconnectRig{qr: qr, srv: srv, store: store, logs: logs}
}

func (rig *reconnectRig) stored(t *testing.T) string {
	t.Helper()
	v, err := rig.store.Get("Elk Scout", "mac-claude")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestApplyReconnectIsQuietUntilElkAsks(t *testing.T) {
	rig := newReconnectRig(t)
	rig.qr.applyReconnect(context.Background())
	if n := len(rig.srv.CallsTo(elk.ToolReconnectOwnCredential)); n != 0 {
		t.Fatalf("asked Elk %d times with no request", n)
	}
}

func TestApplyReconnectStoresThenSwitches(t *testing.T) {
	rig := newReconnectRig(t)
	rig.qr.noteReconnect(&elk.Heartbeat{Reconnect: true})
	rig.qr.applyReconnect(context.Background())

	if got := rig.stored(t); got != newCred {
		t.Fatalf("stored = %q", got)
	}
	if rig.qr.elkToken != newCred {
		t.Fatalf("the run token was not replaced, so a later run would hold the ended one")
	}
	if rig.qr.reconnect.wanted.Load() {
		t.Fatal("still wanted after it finished")
	}
	// The next beat goes out on the new credential, which is what ends the old.
	rig.srv.AcceptToken(newCred)
	if _, err := rig.qr.elk.HeartbeatExecutor(context.Background(), elk.HeartbeatRequest{Queue: "mac-claude"}); err != nil {
		t.Fatalf("the next beat did not use the new credential: %v", err)
	}
	if strings.Contains(rig.logs.String(), newCred) {
		t.Fatal("the log holds the new credential")
	}
}

func TestApplyReconnectKeepsTheOldCredentialWhenTheKeychainFails(t *testing.T) {
	for name, set := range map[string]func(*flakyStore){
		"refused": func(s *flakyStore) { s.refuseSet = true },
		"lost":    func(s *flakyStore) { s.loseSet = true },
	} {
		t.Run(name, func(t *testing.T) {
			rig := newReconnectRig(t)
			set(rig.store)
			rig.qr.noteReconnect(&elk.Heartbeat{Reconnect: true})
			rig.qr.applyReconnect(context.Background())

			if rig.qr.elkToken != oldCred {
				t.Fatal("switched to a credential the keychain did not keep")
			}
			if _, err := rig.qr.elk.HeartbeatExecutor(context.Background(), elk.HeartbeatRequest{Queue: "mac-claude"}); err != nil {
				t.Fatalf("the old credential stopped being used: %v", err)
			}
			if !rig.qr.reconnect.wanted.Load() {
				t.Fatal("gave up on the request")
			}
			if strings.Contains(rig.logs.String(), newCred) {
				t.Fatal("the log holds the new credential")
			}
			// Not again on the next beat: each attempt mints a credential.
			rig.qr.applyReconnect(context.Background())
			if n := len(rig.srv.CallsTo(elk.ToolReconnectOwnCredential)); n != 1 {
				t.Fatalf("asked %d times inside the retry wait", n)
			}
			// After the wait, it tries again and finishes.
			rig.store.refuseSet, rig.store.loseSet = false, false
			rig.qr.reconnect.nextTry = time.Now().Add(-time.Second)
			rig.qr.applyReconnect(context.Background())
			if got := rig.stored(t); got != newCred {
				t.Fatalf("after the wait, stored = %q", got)
			}
		})
	}
}

func TestApplyReconnectLeavesEverythingWhenElkRefuses(t *testing.T) {
	rig := newReconnectRig(t)
	rig.srv.Reply(elk.ToolReconnectOwnCredential, elktest.Reply{Text: "Nothing was asked for.", IsError: true})
	rig.qr.noteReconnect(&elk.Heartbeat{Reconnect: true})
	rig.qr.applyReconnect(context.Background())
	if got := rig.stored(t); got != oldCred || rig.qr.elkToken != oldCred {
		t.Fatalf("stored = %q, run token = %q", got, rig.qr.elkToken)
	}
}

func TestApplyReconnectTreatsAnEmptyReplyAsAFailure(t *testing.T) {
	rig := newReconnectRig(t)
	rig.srv.Reply(elk.ToolReconnectOwnCredential, elktest.Reply{Text: "ok"})
	rig.qr.noteReconnect(&elk.Heartbeat{Reconnect: true})
	rig.qr.applyReconnect(context.Background())
	if got := rig.stored(t); got != oldCred {
		t.Fatalf("stored = %q", got)
	}
}

func TestApplyReconnectStopsAskingWhenItHoldsTheReplacement(t *testing.T) {
	rig := newReconnectRig(t)
	rig.srv.Reply(elk.ToolReconnectOwnCredential, elktest.Reply{Text: "Nothing to replace.", Structured: map[string]any{"already": true}})
	rig.qr.noteReconnect(&elk.Heartbeat{Reconnect: true})
	rig.qr.applyReconnect(context.Background())
	if got := rig.stored(t); got != oldCred || rig.qr.elkToken != oldCred {
		t.Fatalf("stored = %q, run token = %q", got, rig.qr.elkToken)
	}
	if rig.qr.reconnect.wanted.Load() {
		t.Fatal("still wanted after Elk said there was nothing to do")
	}
	if !rig.qr.reconnect.nextTry.IsZero() {
		t.Fatal("treated as a failure")
	}
}

func TestApplyReconnectNeverRunsHosted(t *testing.T) {
	rig := newReconnectRig(t)
	rig.qr.r.opts.Hosted = true
	rig.qr.noteReconnect(&elk.Heartbeat{Reconnect: true})
	rig.qr.applyReconnect(context.Background())
	if n := len(rig.srv.CallsTo(elk.ToolReconnectOwnCredential)); n != 0 {
		t.Fatalf("a hosted run, whose token is not in a keychain, asked %d times", n)
	}
}

func TestABeatWithdrawsTheRequest(t *testing.T) {
	rig := newReconnectRig(t)
	rig.qr.noteReconnect(&elk.Heartbeat{Reconnect: true})
	rig.qr.noteReconnect(&elk.Heartbeat{})
	rig.qr.applyReconnect(context.Background())
	if n := len(rig.srv.CallsTo(elk.ToolReconnectOwnCredential)); n != 0 {
		t.Fatalf("acted on a withdrawn request %d times", n)
	}
}

func TestASessionCannotAskForTheCredential(t *testing.T) {
	if !slices.Contains(adapter.RunConnectorDeniedTools, elk.ToolReconnectOwnCredential) {
		t.Fatal("a run's session could ask for this queue's credential through the run connector")
	}
}

func TestEveryBeatDeclaresReconnectExceptHosted(t *testing.T) {
	r := &Runner{opts: Options{Config: config.Config{}, TelemetryOff: true}}
	host := &HostCapabilities{how: map[string]string{}}
	qr := &queueRunner{r: r, q: config.Queue{Name: "mac-claude", AgentKind: "no-such-agent"}, workspace: "Elk Scout", host: host}
	if !slices.Contains(qr.heartbeatRequest().DeclaredCapabilities, CapabilityReconnect) {
		t.Fatal("an older Rein and this one look the same to Elk")
	}
	r.opts.Hosted = true
	if slices.Contains(qr.heartbeatRequest().DeclaredCapabilities, CapabilityReconnect) {
		t.Fatal("a hosted run claimed it can reconnect")
	}
}

// A keychain that took the write and then could not read it back must not be left
// holding the replacement: the next attempt revokes it, and a restart would read it.
type unreadableStore struct {
	keyring.Store
	failReads int
}

func (u *unreadableStore) Get(w, q string) (string, error) {
	if u.failReads > 0 {
		u.failReads--
		return "", errors.New("locked")
	}
	return u.Store.Get(w, q)
}

func TestApplyReconnectPutsTheWorkingCredentialBack(t *testing.T) {
	rig := newReconnectRig(t)
	u := &unreadableStore{Store: rig.store.Store, failReads: 1}
	rig.qr.r.opts.Store = u
	rig.qr.noteReconnect(&elk.Heartbeat{Reconnect: true})
	rig.qr.applyReconnect(context.Background())
	if got, err := u.Store.Get("Elk Scout", "mac-claude"); err != nil || got != oldCred {
		t.Fatalf("the keychain holds %q, err %v", got, err)
	}
	if rig.qr.elkToken != oldCred {
		t.Fatal("switched to a credential the keychain could not read back")
	}
}
