package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/worktree"
)

// finishArkRuns owns completion of records the agent opened on this run's
// unique branch. Never finish another branch's records, or invent an Ark
// repository when provisioning was unavailable. It is deferred before reap,
// covering success, refusal, failure and cancellation, including review exits.
func (qr *queueRunner) finishArkRuns(ctx context.Context, wt *worktree.Worktree) {
	if wt == nil || !wt.Ark.OK || wt.Branch == "" {
		return
	}
	// The run's context is commonly cancelled on this path; cleanup must still
	// execute, but a broken Ark command must not keep the worker alive forever.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	command := qr.r.opts.ArkCommand
	if command == nil {
		command = func(ctx context.Context, dir string, args ...string) (string, error) {
			return runIn(ctx, dir, "ark", args...)
		}
	}
	out, err := command(ctx, wt.Dir, "run", "list", "--json")
	var runs []struct {
		ID     string `json:"id"`
		Branch string `json:"branch_name"`
		Status string `json:"status"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(out), &runs)
	}
	if err != nil {
		qr.logf("could not read Ark runs for completion: %v", err)
		qr.log.Runner(runlog.KindNote, "could not read Ark runs for completion: %v", err)
		return
	}
	status, summary := qr.arkResult()
	for _, run := range runs {
		if run.Branch != wt.Branch || run.Status != "running" {
			continue
		}
		if _, err := command(ctx, wt.Dir, "run", "finish", run.ID, "--status", status,
			"--result", qr.redactString(summary), "--no-review"); err != nil {
			qr.logf("could not finish Ark run %s: %v", run.ID, err)
			qr.log.Runner(runlog.KindNote, "could not finish Ark run %s: %v", run.ID, err)
		}
	}
}

func (qr *queueRunner) arkResult() (string, string) {
	status := "failed"
	switch qr.logStatus {
	case runlog.StatusSubmitted:
		status = "succeeded"
	case runlog.StatusCancelled:
		status = "cancelled"
	}
	summary := qr.resultSummary
	if summary == "" {
		summary = fmt.Sprintf("Rein ended the run (%s) without a submitted deliverable.", qr.logStatus)
	} else if status == "cancelled" {
		summary = "The run was cancelled. Last attempted deliverable:\n\n" + summary
	}
	return status, summary
}
