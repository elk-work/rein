package runner_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// win01 is win-01's GitHub runner, as scout's lab workflows address it.
func win01(grace time.Duration) config.YieldToGitHub {
	return config.YieldToGitHub{
		Runner:    "win-01",
		Labels:    []string{"self-hosted", "windows", "win11", "ui-lab", "win-01"},
		Repos:     []string{"elk-work/scout"},
		Workflows: []string{"port-train.yml"},
		Grace:     config.Duration(grace),
		Interval:  config.Duration(50 * time.Millisecond),
	}
}

// windowsPort is a Windows port leg queued for win-01.
var windowsPort = runner.PriorityJob{
	Repo: "elk-work/scout", Workflow: "port-train.yml", Name: "port-windows (win-01)",
	Status: "queued", Labels: []string{"self-hosted", "windows", "win11", "ui-lab", "win-01"},
}

// Casey's ruling of 2026-11-23: win-01 takes Elk agent work only when no port
// is waiting for it. While one is, the queue stays online and claims nothing.
func TestNoClaimWhileAGitHubJobWaitsForThisRunner(t *testing.T) {
	h := newHarness(t)
	h.cfg.YieldToGitHub = win01(time.Minute)
	h.elk.Text("claim_run", order("run-1"))
	lookup := func(context.Context, config.YieldToGitHub) ([]runner.PriorityJob, error) {
		return []runner.PriorityJob{windowsPort}, nil
	}
	if err := h.runLoop(runner.Options{PriorityJobs: lookup, PollInterval: 20 * time.Millisecond}, 400*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if n := len(h.elk.CallsTo("claim_run")); n != 0 {
		t.Fatalf("claimed %d time(s) while a port waited for win-01\nlog:\n%s", n, h.log)
	}
	if len(h.elk.CallsTo("heartbeat_executor")) == 0 {
		t.Error("the queue stopped beating; it should stay online and say why it is not claiming")
	}
	if !strings.Contains(h.log.String(), `"port-train.yml: port-windows (win-01)" is waiting for runner win-01`) {
		t.Errorf("the log does not name the job it is waiting behind:\n%s", h.log)
	}
}

// Failing closed: when GitHub cannot be asked, the machine stays on its
// GitHub jobs rather than guess that none is waiting.
func TestNoClaimWhenGitHubCannotBeAsked(t *testing.T) {
	h := newHarness(t)
	h.cfg.YieldToGitHub = win01(time.Minute)
	h.elk.Text("claim_run", order("run-1"))
	lookup := func(context.Context, config.YieldToGitHub) ([]runner.PriorityJob, error) {
		return nil, errors.New("gh api: HTTP 401: Bad credentials")
	}
	if err := h.runLoop(runner.Options{PriorityJobs: lookup, PollInterval: 20 * time.Millisecond}, 400*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if n := len(h.elk.CallsTo("claim_run")); n != 0 {
		t.Fatalf("claimed %d time(s) with GitHub unreadable", n)
	}
	if !strings.Contains(h.log.String(), "could not ask GitHub whether runner win-01 has a job waiting") {
		t.Errorf("the log does not say why it is not claiming:\n%s", h.log)
	}
}

// Another machine's job, a hosted job and a finished one leave this machine
// free to claim.
func TestJobsForOtherRunnersDoNotHoldClaims(t *testing.T) {
	h := newHarness(t)
	h.cfg.YieldToGitHub = win01(time.Minute)
	h.elk.Text("claim_run", order("run-1"))
	lookup := func(context.Context, config.YieldToGitHub) ([]runner.PriorityJob, error) {
		return []runner.PriorityJob{
			{Name: "port-windows (win-02)", Status: "queued", Labels: []string{"self-hosted", "windows", "win11", "ui-lab", "win-02"}},
			{Name: "android", Status: "queued", Labels: []string{"self-hosted", "macos", "mac-lab"}},
			{Name: "plan", Status: "queued", Labels: []string{"ubuntu-latest"}},
			{Name: "port-windows (win-02)", Status: "in_progress", Runner: "win-02", Labels: []string{"self-hosted", "windows", "win11", "ui-lab", "win-02"}},
		}, nil
	}
	if err := h.run(runner.Options{PriorityJobs: lookup}); err != nil {
		t.Fatal(err)
	}
	if h.submitted().Arg("status") != "ready" {
		t.Fatalf("the run did not finish on an idle win-01\nlog:\n%s", h.log)
	}
}

// The other half of the ruling: a port queued while an agent task is under
// way still runs without delay. GitHub's runner starts it at once on its own;
// Rein's part is to get the agent off the machine within the grace period,
// push what it committed, and hand the run back to Elk to start again, then
// claim nothing more while the port is there.
func TestAJobArrivingMidRunStopsTheAgentWithinTheGrace(t *testing.T) {
	h := newHarness(t)
	const grace = 300 * time.Millisecond
	h.cfg.YieldToGitHub = win01(grace)
	var claims atomic.Int32
	h.elk.Handle("claim_run", func(map[string]any) elktest.Reply {
		claims.Add(1)
		return elktest.Reply{Text: order("run-1")}
	})
	h.agent.Delay = time.Minute

	var (
		mu     sync.Mutex
		seenAt time.Time
	)
	lookup := func(context.Context, config.YieldToGitHub) ([]runner.PriorityJob, error) {
		if len(h.agent.Sessions()) == 0 {
			return nil, nil
		}
		mu.Lock()
		if seenAt.IsZero() {
			seenAt = time.Now()
		}
		mu.Unlock()
		return []runner.PriorityJob{windowsPort}, nil
	}
	var backAt time.Time
	handedBack := func() bool {
		if len(handBacks(h)) == 1 && backAt.IsZero() {
			backAt = time.Now()
		}
		// Run on past the hand-back, to see that nothing more is claimed.
		return !backAt.IsZero() && time.Since(backAt) > 300*time.Millisecond
	}
	met, err := h.runLoopUntil(runner.Options{PriorityJobs: lookup, PollInterval: 20 * time.Millisecond}, 30*time.Second, handedBack)
	if !met {
		t.Fatalf("the run was never handed back: %v\nlog:\n%s", err, h.log)
	}

	if s := h.agent.Sessions(); s[0].Interrupts() == 0 {
		t.Error("the agent was not stopped")
	}
	mu.Lock()
	took := backAt.Sub(seenAt)
	mu.Unlock()
	// The watch asks again the moment the grace runs out, floored at a
	// second between asks; anything past grace plus that floor plus the
	// hand-back's own calls is a regression.
	if took < grace || took > grace+2*time.Second {
		t.Errorf("handed back %s after the job was seen, want between the %s grace and %s", took, grace, grace+2*time.Second)
	}
	c := handBacks(h)[0]
	if c.Arg("run_id") != "run-1" || c.Arg("status") != "stuck" || c.Arg("title") != "Stopped for a GitHub Actions job" {
		t.Errorf("hand-back = run %q status %q title %q", c.Arg("run_id"), c.Arg("status"), c.Arg("title"))
	}
	if d := c.Arg("deliverable"); !strings.Contains(d, "runner win-01") || !strings.Contains(d, "did not fail") {
		t.Errorf("the hand-back does not say why the run stopped:\n%s", d)
	}
	if all := h.elk.CallsTo("submit_deliverable"); len(all) != 1 {
		t.Errorf("submit_deliverable calls = %d, want only the hand-back", len(all))
	}
	if n := claims.Load(); n != 1 {
		t.Errorf("claims = %d, want 1: nothing more while the port waits", n)
	}
	if _, reaped := h.wts.counts(); reaped != 1 {
		t.Errorf("worktrees reaped = %d, want 1", reaped)
	}
}
