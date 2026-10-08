package runner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/worktree"
)

// ark:rein#60: a Wrangler queue's claims go to the front of the line, and a
// front place spent on a build is lent back until another claim has had a slot.
func TestAWranglerQueueLendsItsPlaceAfterABuild(t *testing.T) {
	r := testRunner(fullMachine())
	qr := testQueueRunner(r)
	if got := qr.claimPriority(); got != prioBuild {
		t.Fatalf("a build queue claims at %s priority", got)
	}

	qr.q.Wrangler = true
	if got := qr.claimPriority(); got != prioWrangler {
		t.Fatalf("a Wrangler queue claims at %s priority, want Wrangler", got)
	}

	// Its claim came back a build, so the place is lent.
	h := take(t, r.gate, slotRequest{queue: qr.q.Name, prio: prioWrangler})
	qr.lent, qr.lentAt = true, h.grant
	if got := qr.claimPriority(); got != prioBuild {
		t.Fatalf("after spending the front of the line on a build the queue claims at %s, want build", got)
	}

	// Another queue has a turn; the place comes back.
	take(t, r.gate, slotRequest{queue: "mac-codex"})
	if got := qr.claimPriority(); got != prioWrangler {
		t.Fatalf("after another queue's turn the Wrangler queue claims at %s, want Wrangler", got)
	}
}

// ark:rein#59: while a queue waits for a slot, its beat says what it is
// waiting on — rather than "idle", which on 2026-10-06 is all anyone saw for
// hours while two runs sat queued.
func TestAQueueWaitingForASlotSaysWhyOnItsBeat(t *testing.T) {
	r := testRunner(fullMachine())
	r.gate.hard = 1
	r.gate.retry = time.Millisecond
	other := take(t, r.gate, slotRequest{queue: "mac-codex"})
	other.setRun("arun-48e658fb")
	qr := testQueueRunner(r)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		claimed, _, err := qr.claimAndDrive(ctx)
		if claimed || err != nil {
			t.Errorf("claimAndDrive = %v, %v; want nothing claimed after a cancelled wait", claimed, err)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for qr.waiting() == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s := qr.sessionReading()
	cancel()
	<-done

	if s.State != string(StateIdle) {
		t.Errorf("state = %q, want idle — the apps render no other word for a queue with no run", s.State)
	}
	for _, want := range []string{slotWaitPrefix, "in use by mac-codex run arun-48e658fb"} {
		if !strings.Contains(s.WaitingOn, want) {
			t.Errorf("waiting_on = %q, missing %q", s.WaitingOn, want)
		}
	}
	if s.WaitingSince == "" {
		t.Error("waiting_on without waiting_since: nobody can tell how long it has been")
	}
}

func TestWaitingIsClearedAndOmittedWhenNothingIsHeldUp(t *testing.T) {
	qr := testQueueRunner(testRunner(fullMachine()))
	if !qr.setWaiting("claim_run refused: parked") {
		t.Error("a new reason was not reported as a change")
	}
	if qr.setWaiting("claim_run refused: parked") {
		t.Error("the same reason was reported as a change")
	}
	if qr.setWaiting("waiting for a slot: in use for 3m") != true {
		t.Error("a different reason was not reported as a change")
	}
	if qr.setWaiting("waiting for a slot: in use for 4m") {
		t.Error("a reason whose only change is a number was reported as a new one")
	}

	// A run in flight can be the one waiting — for a slot to work revisions.
	qr.startRun("arun-1")
	if s := qr.sessionReading(); s.State != string(StateRunning) || s.WaitingOn == "" {
		t.Errorf("a running queue's wait was not reported: %+v", s)
	}
	qr.endRun("")

	qr.setWaiting("")
	s := qr.sessionReading()
	if s.WaitingOn != "" || s.WaitingSince != "" {
		t.Errorf("a cleared wait is still reported: %q since %q", s.WaitingOn, s.WaitingSince)
	}
}

// Only Elk's words or Rein's own reach the wire: a transport error carries the
// connector URL, and the connector URL carries the queue's token.
func TestClaimFailureNeverShipsTheConnectorURL(t *testing.T) {
	const secret = "tok_live_5f2c9e"
	transport := &url.Error{Op: "Post", URL: "https://elk.example/functions/v1/elk-mcp/" + secret,
		Err: errors.New("dial tcp: i/o timeout")}

	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"transport": {fmt.Errorf("claim: %w", transport), "Elk could not be reached"},
		"parked": {fmt.Errorf("claim: %w", &elk.ToolError{Text: `Queue "mac-grok" is disabled: its agent was parked in Settings → Connected agents, and nothing is handed to a parked agent until someone enables it again.`}),
			`claim_run refused: Queue "mac-grok" is disabled`},
		"database": {fmt.Errorf("claim: %w", &elk.RPCError{Code: -32603, Message: "canceling statement due to statement timeout"}),
			"Elk answered error -32603: canceling statement due to statement timeout"},
		"http": {fmt.Errorf("claim: %w", &elk.HTTPError{StatusCode: 502, Status: "502 Bad Gateway", Body: "<html>"}),
			"Elk answered 502 Bad Gateway"},
		"tier": {&elk.TierRefusalError{Tool: "claim_run", Required: "run-scoped", Held: "read-only"},
			"it is read-only, and claim_run needs run-scoped"},
	} {
		t.Run(name, func(t *testing.T) {
			got := claimFailure(tc.err)
			if !strings.Contains(got, tc.want) {
				t.Errorf("claimFailure = %q, want it to contain %q", got, tc.want)
			}
			if strings.Contains(got, secret) || strings.Contains(got, "https://") {
				t.Errorf("claimFailure shipped the connector URL: %q", got)
			}
		})
	}
}

// A run that waited for its slot says so on the run itself, in its first
// report, so a person who watched it sit queued can read what it was behind.
func TestTheOpeningReportSaysWhatTheRunWaitedFor(t *testing.T) {
	qr := testQueueRunner(testRunner(fullMachine()))
	wt := &worktree.Worktree{Dir: "/w/run-1", Branch: "rein/run-run-1", BaseRef: "origin/main", BaseSHA: "abc1234567"}
	repo := RepoResolution{Path: "/repo"}

	if got := qr.openingReport(repo, wt); strings.Contains(got, "Waited") {
		t.Errorf("a run that never waited says it did:\n%s", got)
	}

	qr.slot = &slotHold{waited: 72 * time.Minute,
		waitedOn: slotWaitPrefix + "1 slot — held down by 5.0 GiB RAM free (cap 4); in use by mac-codex run arun-1 for 40m"}
	got := qr.openingReport(repo, wt)
	if !strings.Contains(got, "Waited 1h12m for a slot on this machine (1 slot — held down by 5.0 GiB RAM free (cap 4); in use by mac-codex run arun-1 for 40m)") {
		t.Errorf("the opening report does not say what the run waited for:\n%s", got)
	}
}
