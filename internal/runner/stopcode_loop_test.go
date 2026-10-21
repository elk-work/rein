package runner_test

import (
	"fmt"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/runner"
	"github.com/elk-work/rein/internal/worktree"
)

func TestStuckSubmissionsCarryAStopCode(t *testing.T) {
	t.Run("init marker names the cause", func(t *testing.T) {
		h := newHarness(t)
		h.elk.Text("claim_run", order("run-1"))
		h.wts.err = fmt.Errorf("worktree: init failed: %w",
			worktree.InitStop{Code: "repo_access", Line: "GitHub can't be reached for scout"})
		if err := h.run(runner.Options{}); err != nil {
			t.Fatal(err)
		}
		sub := h.submitted()
		if sub.Arg("stop_code") != "repo_access" || sub.Arg("title") != "GitHub can't be reached for scout" {
			t.Errorf("stop_code = %q, title = %q", sub.Arg("stop_code"), sub.Arg("title"))
		}
	})
	t.Run("preflight login", func(t *testing.T) {
		h := newHarness(t)
		h.elk.Text("claim_run", order("run-1"))
		h.agent.PreflightErr = fmt.Errorf("%w: claude is not logged in", adapter.ErrPreflight)
		if err := h.run(runner.Options{}); err != nil {
			t.Fatal(err)
		}
		if got := h.submitted().Arg("stop_code"); got != "agent_login" {
			t.Errorf("stop_code = %q, want agent_login", got)
		}
	})
}
