package runner_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/runner"
	"github.com/elk-work/rein/internal/worktree"
)

func TestEmptyRunAndArkCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, text, reply, wantSubmit, wantArk string
		commits                                int
		fail, cancel                           bool
	}{
		{name: "empty", wantSubmit: "stuck", wantArk: "failed"},
		{name: "committed without text", commits: 1, wantSubmit: "ready", wantArk: "succeeded"},
		{name: "boilerplate", text: "Done.", wantSubmit: "stuck", wantArk: "failed"},
		{name: "ready reason", text: "Nothing to change: the requested behavior is already implemented.", wantSubmit: "ready", wantArk: "succeeded"},
		{name: "done", text: "Nothing to change: already implemented.", reply: "done", wantSubmit: "ready", wantArk: "succeeded"},
		{name: "stuck reason", text: "Blocked: the required credential is unavailable.", wantSubmit: "stuck", wantArk: "failed"},
		{name: "failed", fail: true, wantSubmit: "stuck", wantArk: "failed"},
		{name: "cancelled", cancel: true, wantArk: "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.elk.Text("claim_run", order("run-1"))
			h.wts.ark = worktree.ArkState{Wanted: true, OK: true}
			h.agent.Script = nil
			h.agent.Result.Summary = tc.text
			if tc.name == "stuck reason" {
				h.agent.Result.Status = adapter.StatusFailed
			}
			if tc.fail {
				h.agent.StartErr = errors.New("agent failed to start")
			}
			if tc.cancel {
				h.elk.Text("report_progress", askCancelled)
			}
			if tc.reply != "" {
				h.elk.Text("submit_deliverable", "Deliverable saved: run `run-1` is `"+tc.reply+"`.")
			}
			var finishes int
			opts := runner.Options{
				Landing: &fakeLanding{state: runner.LandingState{CommitsKnown: true, Commits: tc.commits}},
				ArkCommand: func(ctx context.Context, dir string, args ...string) (string, error) {
					if ctx.Err() != nil {
						t.Error("Ark cleanup inherited cancellation")
					}
					if len(args) > 1 && args[1] == "list" {
						return `[{"id":"ours","branch_name":"rein/run-run-1","status":"running"},{"id":"other","branch_name":"main","status":"running"},{"id":"finished","branch_name":"rein/run-run-1","status":"succeeded"}]`, nil
					}
					finishes++
					if args[2] != "ours" || args[4] != tc.wantArk || strings.TrimSpace(args[6]) == "" {
						t.Errorf("Ark finish = %v", args)
					}
					if _, reaped := h.wts.counts(); reaped != 0 {
						t.Error("Ark finished after reap")
					}
					return "", nil
				},
			}
			if err := h.run(opts); err != nil {
				t.Fatal(err)
			}
			if finishes != 1 {
				t.Fatalf("Ark finishes = %d, want 1", finishes)
			}
			if tc.cancel {
				if len(h.elk.CallsTo("submit_deliverable")) != 0 {
					t.Error("cancelled run submitted")
				}
				return
			}
			sub := h.submitted()
			if sub.Arg("status") != tc.wantSubmit {
				t.Errorf("status = %s", sub.Arg("status"))
			}
			if tc.wantSubmit == "stuck" && !tc.fail && tc.name != "stuck reason" && !strings.Contains(sub.Arg("deliverable"), "This run produced nothing") {
				t.Error("missing empty-result explanation")
			}
			if tc.text != "" && tc.wantSubmit == "ready" && sub.Arg("deliverable") != tc.text {
				t.Errorf("reason replaced: %s", sub.Arg("deliverable"))
			}
		})
	}
}
