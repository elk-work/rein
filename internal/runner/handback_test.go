package runner_test

import (
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
)

// handBacks are the submits that hand a run back: `stuck`, with the stop code
// Elk restarts a run on.
func handBacks(h *harness) []elktest.Call {
	var out []elktest.Call
	for _, c := range h.elk.CallsTo("submit_deliverable") {
		if c.Arg("stop_code") == "claim_lapsed" {
			out = append(out, c)
		}
	}
	return out
}

func reportedInterrupted(h *harness, runID string) bool {
	for _, c := range h.elk.CallsTo("report_progress") {
		if c.Arg("run_id") == runID && strings.Contains(c.Arg("body"), "interrupted by the restart") {
			return true
		}
	}
	return false
}

// leaveDeadRun leaves what a runner that died mid-run leaves: a log, and a
// meta still `running`, last written lastSeen ago. The pid is this process's
// own, which at startup can only be an earlier life's (runlog.SettleDead).
// Written as files, with nothing held open, as after a real death.
func leaveDeadRun(t *testing.T, logs *runlog.Store, m runlog.Meta, lastSeen time.Duration) {
	t.Helper()
	m.Status, m.PID = runlog.StatusRunning, os.Getpid()
	m.StartedAt = time.Now().Add(-lastSeen - time.Minute)
	meta, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-lastSeen)
	for path, body := range map[string][]byte{logs.LogPath(m.RunID): nil, logs.MetaPath(m.RunID): meta} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

// The 2026-11-20 finding on fij-claude: `rein service stop` while a run was in
// an agent session stopped the agent and told Elk nothing, so Elk showed the
// run in progress until its lease lapsed. A stop now hands the claim back at
// once, as interrupted, and never as finished or failed.
func TestStoppingReinHandsALiveRunBack(t *testing.T) {
	h := newHarness(t)
	var claims atomic.Int32
	h.elk.Handle("claim_run", func(map[string]any) elktest.Reply {
		if claims.Add(1) == 1 {
			return elktest.Reply{Text: order("run-1")}
		}
		return elktest.Reply{Text: `Queue "` + queue + `" is clear: no runs waiting in "` + space + `".`}
	})
	// A session that is still working when Rein is told to stop.
	h.agent.Delay = time.Minute

	inSession := func() bool {
		return len(h.agent.Sessions()) == 1 && len(h.elk.CallsTo("report_progress")) > 0
	}
	met, err := h.runLoopUntil(runner.Options{}, 30*time.Second, inSession)
	if !met {
		t.Fatalf("the run never reached its agent session: %v\nlog:\n%s", err, h.log)
	}

	if s := h.agent.Sessions(); s[0].Interrupts() == 0 {
		t.Error("the agent was not stopped")
	}
	back := handBacks(h)
	if len(back) != 1 {
		t.Fatalf("hand-backs = %d, want 1\nlog:\n%s", len(back), h.log)
	}
	if all := h.elk.CallsTo("submit_deliverable"); len(all) != 1 {
		t.Fatalf("submit_deliverable calls = %d, want only the hand-back", len(all))
	}
	c := back[0]
	if c.Arg("run_id") != "run-1" || c.Arg("status") != "stuck" {
		t.Errorf("handed back run %q as %q, want run-1 as stuck (never done, ready or failed)", c.Arg("run_id"), c.Arg("status"))
	}
	if d := c.Arg("deliverable"); !strings.Contains(d, "interrupted") || !strings.Contains(d, "did not fail") {
		t.Errorf("the hand-back does not say the run was interrupted, not failed:\n%s", d)
	}
	if !reportedInterrupted(h, "run-1") {
		t.Errorf("no report line on the run says it was interrupted by a restart\nlog:\n%s", h.log)
	}
	if !strings.Contains(h.log.String(), "handed back to Elk") {
		t.Errorf("the service log does not say the run was handed back:\n%s", h.log)
	}
	if _, reaped := h.wts.counts(); reaped != 1 {
		t.Errorf("worktrees reaped = %d, want 1", reaped)
	}
}

// A run that finishes is never handed back, whatever Rein does afterwards.
func TestAFinishedRunIsNotHandedBack(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatal(err)
	}
	if back := handBacks(h); len(back) != 0 {
		t.Fatalf("a finished run was handed back %d time(s)", len(back))
	}
	if h.submitted().Arg("status") != "ready" {
		t.Errorf("status = %q, want ready", h.submitted().Arg("status"))
	}
}

// A Rein that died without unwinding (a kill, a crash, power loss) leaves runs
// it claimed and no longer drives. The next start hands each back before it
// claims anything, and touches nothing it cannot be sure is still its own.
func TestStartupHandsBackRunsItNoLongerDrives(t *testing.T) {
	h := newHarness(t)
	logs := openRunLogs(t)
	label := h.cfg.QueueLabel(h.cfg.Queues[0])

	leave := func(m runlog.Meta, lastSeen time.Duration) { leaveDeadRun(t, logs, m, lastSeen) }
	leave(runlog.Meta{RunID: "run-dead", Queue: label, Workspace: space, Worktree: "/gone/run-dead",
		Branch: "rein/run-run-dead", SessionID: "s-1"}, 20*time.Second)
	// Too long ago: the lease may have lapsed and somebody else may hold it.
	leave(runlog.Meta{RunID: "run-old", Queue: label, Workspace: space, Worktree: "/gone/run-old"}, 20*time.Minute)
	// Claimed, never started: Elk still has it queued.
	leave(runlog.Meta{RunID: "run-held", Queue: label, Workspace: space}, 20*time.Second)
	// Another queue's.
	leave(runlog.Meta{RunID: "run-other", Queue: "win-codex", Workspace: space, Worktree: "/gone/run-other"}, 20*time.Second)

	var order []string
	h.elk.Handle("submit_deliverable", func(args map[string]any) elktest.Reply {
		order = append(order, "submit")
		return elktest.Reply{Text: "Deliverable saved: run `run-dead` is `stuck`."}
	})
	h.elk.Handle("claim_run", func(map[string]any) elktest.Reply {
		order = append(order, "claim")
		return elktest.Reply{Text: `Queue "` + queue + `" is clear: no runs waiting in "` + space + `".`}
	})

	if err := h.run(runner.Options{Logs: logs}); err != nil {
		t.Fatal(err)
	}

	back := handBacks(h)
	if len(back) != 1 || back[0].Arg("run_id") != "run-dead" {
		var ids []string
		for _, c := range back {
			ids = append(ids, c.Arg("run_id"))
		}
		t.Fatalf("handed back %v, want only run-dead\nlog:\n%s", ids, h.log)
	}
	if back[0].Arg("status") != "stuck" {
		t.Errorf("status = %q, want stuck", back[0].Arg("status"))
	}
	if d := back[0].Arg("deliverable"); !strings.Contains(d, "rein/run-run-dead") {
		t.Errorf("the hand-back does not name the branch the work is on:\n%s", d)
	}
	if !reportedInterrupted(h, "run-dead") {
		t.Errorf("no report line on run-dead says it was interrupted by a restart\nlog:\n%s", h.log)
	}
	for _, id := range []string{"run-old", "run-held", "run-other"} {
		if reportedInterrupted(h, id) {
			t.Errorf("%s was reported on, and is not this start's to hand back", id)
		}
	}
	if len(order) == 0 || order[0] != "submit" {
		t.Errorf("calls ran %v; the hand-back must come before the first claim", order)
	}
}

// Elk answers a report on a run that is no longer running with "Not
// recorded". Then there is no claim to give up, and nothing is submitted.
func TestStartupSubmitsNothingForARunElkSaysIsOver(t *testing.T) {
	h := newHarness(t)
	logs := openRunLogs(t)
	leaveDeadRun(t, logs, runlog.Meta{RunID: "run-dead", Queue: h.cfg.QueueLabel(h.cfg.Queues[0]), Workspace: space,
		Worktree: "/gone/run-dead"}, 20*time.Second)
	h.elk.Text("report_progress", "Not recorded: The user cancelled this run — STOP work on it now.")

	if err := h.run(runner.Options{Logs: logs}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.elk.CallsTo("submit_deliverable")); n != 0 {
		t.Fatalf("submit_deliverable calls = %d, want 0 for a run Elk says is over", n)
	}
}

// Acknowledgement now starts the run before a worktree exists. A crash in
// setup must therefore be recovered just like a crash in an agent session.
func TestStartupHandsBackAcknowledgedRunBeforeWorktree(t *testing.T) {
	h := newHarness(t)
	logs := openRunLogs(t)
	leaveDeadRun(t, logs, runlog.Meta{
		RunID: "run-ack", Queue: h.cfg.QueueLabel(h.cfg.Queues[0]), Workspace: space,
		Acknowledged: true,
	}, 20*time.Second)
	if err := h.run(runner.Options{Logs: logs}); err != nil {
		t.Fatal(err)
	}
	back := handBacks(h)
	if len(back) != 1 || back[0].Arg("run_id") != "run-ack" {
		t.Fatalf("expected acknowledged run to be handed back, got %v", back)
	}
}
