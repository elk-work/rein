package runner

import (
	"context"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/elk/elktest"
)

func TestQuestionResumeEvidence(t *testing.T) {
	for _, tc := range []struct {
		kind adapter.EventKind
		text string
		want bool
	}{
		{adapter.EventText, "answer received", true},
		{adapter.EventText, "", false},
		{adapter.EventToolUse, "", true},
		{adapter.EventPermissionRequest, "", true},
		{adapter.EventDone, "", true},
		{adapter.EventIdle, "", false},
		{adapter.EventUsage, "", false},
		{adapter.EventProgress, "vendor bookkeeping", false},
		{adapter.EventQuestion, "another question", false},
		{adapter.EventError, "", false},
	} {
		t.Run(string(tc.kind)+tc.text, func(t *testing.T) {
			if got := resumesQuestion(adapter.Event{Kind: tc.kind, Text: tc.text}); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWithdrawalRetry(t *testing.T) {
	s := elktest.New(t, "test-token")
	c, err := elk.New(s.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	a := &sessionAsk{client: c, runID: "run-1", pending: true}
	s.Reply("ask_elk", elktest.Reply{Text: "try again", IsError: true})
	if err := a.retract(context.Background(), false); err == nil || !a.pending {
		t.Fatal("failed withdrawal discarded pending ask")
	}
	s.Text("ask_elk", "Retracted.")
	if err := a.retract(context.Background(), false); err != nil || a.pending {
		t.Fatalf("retry: %v, pending=%v", err, a.pending)
	}
	if err := a.retract(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if n := len(s.CallsTo("ask_elk")); n != 2 {
		t.Fatalf("calls=%d", n)
	}
	// Every attachment retracts even when Rein has no pending question locally.
	for i := 0; i < 2; i++ {
		if err := a.retract(context.Background(), true); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(s.CallsTo("ask_elk")); n != 4 {
		t.Fatalf("calls=%d", n)
	}
}

func TestAttachWithdrawalFailureDoesNotClaimSession(t *testing.T) {
	s := elktest.New(t, "test-token")
	c, err := elk.New(s.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	s.Reply("ask_elk", elktest.Reply{Text: "unavailable", IsError: true})
	a := &sessionAsk{client: c, runID: "run-1", pending: true}
	l := &liveSession{onAttach: func() error { return a.retract(context.Background(), true) }}
	if _, release, err := l.attach(); err == nil || release != nil {
		t.Fatal("attachment accepted despite failed withdrawal")
	}
	if l.paused() || !a.pending {
		t.Fatal("failed attachment left watchdog paused or lost ask")
	}
}
