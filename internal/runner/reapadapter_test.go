package runner_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/fake"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
)

// reapingFake is the fake adapter with a [adapter.RunReaper] on it, recording
// what the run loop told it.
type reapingFake struct {
	*fake.Adapter
	err error

	mu       sync.Mutex
	specs    []adapter.RunSpec
	policies []adapter.ReapPolicy
}

func (a *reapingFake) ReapRun(spec adapter.RunSpec, p adapter.ReapPolicy) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.specs = append(a.specs, spec)
	a.policies = append(a.policies, p)
	return []string{"removed 3 old things"}, a.err
}

// withReaper puts a reapingFake in front of the harness's fake adapter.
func withReaper(t *testing.T, h *harness, err error) *reapingFake {
	t.Helper()
	r := &reapingFake{Adapter: h.agent, err: err}
	adapter.Unregister(kind)
	if err := adapter.Register(r); err != nil {
		t.Fatal(err)
	}
	return r
}

// An adapter that keeps something outside the worktree is asked to tidy it
// where the worktree is reaped: once, with the run's spec, the machine's
// log_keep_runs, and every run the logs say is live — the one ending among
// them, and never one that has finished.
func TestAnAdapterTidiesUpWhereTheWorktreeIsReaped(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	logs := openRunLogs(t)
	logs.Create(runlog.Meta{RunID: "done-1"}).Close(runlog.StatusSubmitted)
	other := logs.Create(runlog.Meta{RunID: "other-queue-run"})
	t.Cleanup(func() { other.Close(runlog.StatusSubmitted) })
	r := withReaper(t, h, nil)

	if err := h.run(runner.Options{Logs: logs, LogKeepRuns: 7}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if len(r.policies) != 1 {
		t.Fatalf("ReapRun called %d times, want 1\n%s", len(r.policies), h.log)
	}
	p, spec := r.policies[0], r.specs[0]
	if p.KeepRuns != 7 || p.Now.IsZero() {
		t.Errorf("policy = %+v", p)
	}
	if spec.RunID != "run-1" || spec.WorktreeDir == "" {
		t.Errorf("spec: run %q, worktree %q", spec.RunID, spec.WorktreeDir)
	}
	for _, id := range []string{"run-1", "other-queue-run"} {
		if started, ok := p.Live[id]; !ok || started.IsZero() {
			t.Errorf("live runs lack %s: %v", id, p.Live)
		}
	}
	if _, ok := p.Live["done-1"]; ok {
		t.Errorf("a finished run was passed as live: %v", p.Live)
	}
	if !strings.Contains(h.log.String(), "removed 3 old things") {
		t.Errorf("what the adapter removed is not in the daemon's log:\n%s", h.log)
	}
}

// Tidying up can fail without the run failing: the deliverable is saved
// before it runs, and the failure is the daemon log's to report.
func TestAFailedTidyUpStillSubmitsTheRun(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	withReaper(t, h, errors.New("the disk said no"))

	if err := h.run(runner.Options{Logs: openRunLogs(t), LogKeepRuns: 50}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if sub := h.submitted(); sub.Arg("status") != "ready" {
		t.Errorf("status = %q, want ready", sub.Arg("status"))
	}
	if !strings.Contains(h.log.String(), "tidying up after run run-1: the disk said no") {
		t.Errorf("the failure is not in the daemon's log:\n%s", h.log)
	}
}

// With no run logs Rein cannot tell which runs are live, so it asks no
// adapter to remove anything.
func TestNoRunLogsMeansNothingIsTidied(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	r := withReaper(t, h, nil)

	if err := h.run(runner.Options{LogKeepRuns: 50}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if len(r.policies) != 0 {
		t.Errorf("ReapRun was called %d times without a log store", len(r.policies))
	}
}
