package elk_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/elk/elktest"
)

// Elk's own words (withClaimDeadline and heldNote, elk-mcp/queue.ts).
const (
	deadlineText = "claim_run did not finish within 5 seconds. It may still have claimed a run for you. " +
		"Call claim_run again with the same arguments: a retry hands back a run you already hold, " +
		"or claims one if this call did not."
	clearHeldText = `Queue "mac-claude" is clear: no runs waiting in "Elk Scout".` +
		"\n\nYou hold 2 runs on a live lease: `arun-1`, `arun-2`. If a claim_run reply never reached you, " +
		"call claim_run with that run_id to read its work order again."
)

func TestAClaimPastElksDeadlineMayHaveLanded(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Reply("claim_run", elktest.Reply{Text: deadlineText, IsError: true})
	_, err := newClient(t, s).ClaimRun(context.Background(), elk.ClaimRequest{Queue: "mac-claude"})
	if !elk.ClaimMayHaveLanded(err) {
		t.Fatalf("ClaimMayHaveLanded(%v) = false, want true", err)
	}
}

func TestOnlyALostReplyMayHaveLanded(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&elk.ToolError{Text: deadlineText}, true},
		{&elk.HTTPError{StatusCode: 504, Status: "504 Gateway Timeout"}, true},
		{&elk.HTTPError{StatusCode: 502, Status: "502 Bad Gateway"}, true},
		{fmt.Errorf("claim: %w", &elk.HTTPError{StatusCode: 503}), true},
		{&elk.ToolError{Text: "This queue is parked."}, false},
		{&elk.HTTPError{StatusCode: 401, Status: "401 Unauthorized"}, false},
		{&elk.NoWorkError{Text: "Queue is clear"}, false},
		{errors.New("dial tcp: connection refused"), false},
		{nil, false},
	} {
		if got := elk.ClaimMayHaveLanded(tc.err); got != tc.want {
			t.Errorf("ClaimMayHaveLanded(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestAClearQueueNamesTheRunsTheCallerAlreadyHolds(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Text("claim_run", clearHeldText)
	_, err := newClient(t, s).ClaimRun(context.Background(), elk.ClaimRequest{Queue: "mac-claude"})
	if !errors.Is(err, elk.ErrNoWork) {
		t.Fatalf("err = %v, want ErrNoWork", err)
	}
	var clear *elk.NoWorkError
	if !errors.As(err, &clear) {
		t.Fatalf("err = %T, want *NoWorkError", err)
	}
	if strings.Join(clear.Held, ",") != "arun-1,arun-2" {
		t.Errorf("held = %v, want [arun-1 arun-2]", clear.Held)
	}
	// The clear sentence the loop shows is still only the first line.
	if strings.Contains(clear.Error(), "live lease") || !strings.HasPrefix(clear.Error(), elk.ErrNoWork.Error()+": Queue") {
		t.Errorf("Error() = %q", clear.Error())
	}
}

func TestAPlainClearQueueHoldsNothing(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Text("claim_run", `Queue "mac-claude" is clear: no runs waiting in "Elk Scout".`)
	_, err := newClient(t, s).ClaimRun(context.Background(), elk.ClaimRequest{Queue: "mac-claude"})
	var clear *elk.NoWorkError
	if !errors.As(err, &clear) || len(clear.Held) != 0 {
		t.Fatalf("err = %v, want a clear answer holding nothing", err)
	}
}

func TestAHandedBackRunReadsAsItsWorkOrder(t *testing.T) {
	s := elktest.New(t, testToken)
	order := "Claimed Elk run `arun-1` (contract v2).\n\n**Direction:** Fix it\n**Action id:** `act-1`\n"
	s.Text("claim_run", "You already hold run `arun-1`: it was claimed under your name and its lease is live, "+
		"so this is its work order again (a claim whose reply was lost still counts). "+
		"If another session of yours is already working it, stop here.\n\n"+order)
	wo, err := newClient(t, s).ClaimRun(context.Background(), elk.ClaimRequest{RunID: "arun-1"})
	if err != nil {
		t.Fatal(err)
	}
	if wo.RunID != "arun-1" || !wo.Rehanded || wo.Direction != "Fix it" {
		t.Errorf("work order = %+v", wo)
	}
	if strings.Contains(wo.Text, "already hold") {
		t.Errorf("the note to the claimer reached the work order:\n%s", wo.Text)
	}
	if s.CallsTo("claim_run")[0].Arg("run_id") != "arun-1" {
		t.Errorf("args = %#v", s.CallsTo("claim_run")[0].Args)
	}
}
