package runner_test

import (
	"testing"
	"time"

	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
)

// ark:rein#99 and ark:rein#95: a worktree left in place is reaped once the
// runner can, and a run whose runner died is settled and its worktree reaped.
func TestWorktreesLeftInPlaceAreReapedLater(t *testing.T) {
	h := newHarness(t)
	emptyQueue(h)
	logs, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// A cancelled run whose reap was refused as in use.
	w := logs.Create(runlog.Meta{RunID: "arun-busy"})
	w.Update(func(m *runlog.Meta) {
		m.Repo, m.Worktree, m.Branch = "/src", "/work/arun-busy", "rein/run-arun-busy"
		m.ReapPending = true
	})
	w.Close(runlog.StatusCancelled)

	// A run whose daemon was killed: still `running`, its pid long gone.
	d := logs.Create(runlog.Meta{RunID: "arun-dead", PID: 99999999})
	t.Cleanup(func() { d.Close(runlog.StatusInterrupted) }) // Windows cannot remove an open log
	d.Update(func(m *runlog.Meta) { m.Repo, m.Worktree, m.Branch = "/src", "/work/arun-dead", "rein/run-arun-dead" })

	// A finished run whose worktree was reaped normally is left alone.
	ok := logs.Create(runlog.Meta{RunID: "arun-clean"})
	ok.Update(func(m *runlog.Meta) { m.Repo, m.Worktree = "/src", "/work/arun-clean" })
	ok.Close(runlog.StatusSubmitted)

	met, err := h.runLoopUntil(runner.Options{
		Logs:              logs,
		PollInterval:      time.Millisecond,
		ReclaimInterval:   time.Hour,
		ReapRetryInterval: 5 * time.Millisecond,
	}, 30*time.Second, func() bool {
		owed, _ := logs.ReapOwed()
		return len(owed) == 0
	})
	if err != nil || !met {
		t.Fatalf("the owed reaps never completed (err %v)\nlog:\n%s", err, h.log)
	}

	h.wts.mu.Lock()
	reaped := append([]string(nil), h.wts.reaped...)
	h.wts.mu.Unlock()
	got := map[string]bool{}
	for _, dir := range reaped {
		got[dir] = true
	}
	if !got["/work/arun-busy"] || !got["/work/arun-dead"] || got["/work/arun-clean"] || len(reaped) != 2 {
		t.Errorf("reaped = %v, want exactly the busy and the dead run's worktrees", reaped)
	}
	m, err := logs.ReadMeta("arun-dead")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != runlog.StatusInterrupted || m.ReapPending {
		t.Errorf("the dead run = %q pending=%v, want interrupted and settled", m.Status, m.ReapPending)
	}
}
