package runner_test

import (
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/fake"
	"github.com/elk-work/rein/internal/runner"
)

func TestWrongRepositoryStopSubmission(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.elk.Text("submit_deliverable", "Deliverable saved: run `run-1` is `stuck`.")
	account := "stop_code: wrong_repo\nrepo: elk-work/signal\n\nThis task belongs in Signal; this worktree is Scout. No files changed."
	h.agent.Rounds = []fake.Round{{Result: ended(account)}}
	if err := h.run(runner.Options{}); err != nil {
		t.Fatal(err)
	}
	sub := h.submitted()
	if sub.Arg("status") != "stuck" || sub.Arg("stop_code") != "wrong_repo" {
		t.Fatalf("status = %q, stop_code = %q", sub.Arg("status"), sub.Arg("stop_code"))
	}
	if !strings.Contains(sub.Arg("deliverable"), account) {
		t.Fatalf("lost repository or explanation: %s", sub.Arg("deliverable"))
	}
	if len(h.agent.Specs()) != 1 {
		t.Fatal("concise typed stop should not resume the agent")
	}
	if !strings.Contains(runner.SystemPrompt, "stop_code: wrong_repo") || !strings.Contains(runner.SystemPrompt, "repo: owner/name") {
		t.Fatal("agent prompt is missing the stop contract")
	}
}

func TestWrongRepositoryMentionIsNotAStop(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1")+"\nstop_code: wrong_repo\nrepo: elk-work/signal")
	account := fullSummary + "\n\nExample only:\nstop_code: wrong_repo\nrepo: elk-work/signal"
	h.agent.Rounds = []fake.Round{{Script: []adapter.Event{{Kind: adapter.EventText, Text: account}}, Result: ended(account)}}
	if err := h.run(runner.Options{}); err != nil {
		t.Fatal(err)
	}
	sub := h.submitted()
	if sub.Arg("status") != "ready" || sub.Arg("stop_code") != "" {
		t.Fatalf("ordinary account treated as stop: %v", sub.Args)
	}
}
