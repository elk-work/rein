package runner_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runlog"
)

func TestPendingReviewSurvivesRestart(t *testing.T) {
	h := reviewHarness(t)
	opts := reviewOptions()
	opts.StateDir = t.TempDir()
	logs, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts.Logs = logs
	opts.ReapRetryInterval = time.Millisecond
	opts.ReviewTimeout = time.Hour
	// Stop after a real review poll, then construct a new Runner with the same
	// state directory. Do it twice to prove the updated round survives too.
	for round := 1; round <= 2; round++ {
		before := len(h.elk.CallsTo("ask_elk"))
		if round == 2 {
			n := 0
			h.elk.Handle("ask_elk", func(map[string]any) elktest.Reply {
				n++
				if n == 1 {
					return elktest.Reply{Text: askRevisions}
				}
				return elktest.Reply{Text: askReviewHeld}
			})
		}
		target := before + 1
		if round == 2 {
			target++
		}
		met, err := h.runLoopUntil(opts, 10*time.Second, func() bool { return len(h.elk.CallsTo("ask_elk")) >= target })
		if err != nil || !met {
			t.Fatalf("stop round %d: %v, met %v", round, err, met)
		}
		files, _ := filepath.Glob(filepath.Join(opts.StateDir, "reviews", "*.json"))
		if len(files) != 1 {
			t.Fatalf("pending reviews = %v", files)
		}
		if _, reaped := h.wts.counts(); reaped != 0 {
			t.Fatalf("reaped pending review worktree in round %d", round)
		}
		// A stale log may owe a reap after a crash. Pending review state
		// must protect this worktree even before the queue starts up.
		meta, err := logs.ReadMeta("run-1")
		if err != nil {
			t.Fatal(err)
		}
		meta.ReapPending = true
		logs.Create(meta).Close(runlog.StatusInterrupted)
		if len(h.agent.Specs()) != round {
			t.Fatalf("agent starts = %d, want %d", len(h.agent.Specs()), round)
		}
	}
	h.elk.Text("claim_run", `Queue "`+queue+`" is clear: no runs waiting in "`+space+`".`)
	n := 0
	h.elk.Handle("ask_elk", func(map[string]any) elktest.Reply {
		n++
		if n == 1 {
			return elktest.Reply{Text: askRevisions}
		}
		return elktest.Reply{Text: askReviewPassed}
	})
	if err := h.run(opts); err != nil {
		t.Fatal(err)
	}
	specs := h.agent.Specs()
	if len(specs) != 3 {
		t.Fatalf("agent starts = %d, want 3", len(specs))
	}
	for _, spec := range specs[1:] {
		if spec.WorktreeDir != specs[0].WorktreeDir || spec.ResumeID == "" || !strings.Contains(spec.Prompt, "does not say which tests were run") {
			t.Fatal("revision lost worktree, session, or verdict")
		}
	}
	if !strings.Contains(h.log.String(), "running the agent again (round 3)") {
		t.Fatal("restart did not preserve round")
	}
	created, reaped := h.wts.counts()
	if created != 1 || reaped != 1 {
		t.Fatalf("worktrees created/reaped = %d/%d", created, reaped)
	}
	files, _ := filepath.Glob(filepath.Join(opts.StateDir, "reviews", "*.json"))
	if len(files) != 0 {
		t.Fatalf("settled reviews remain: %v", files)
	}
}

func TestRestartDiscardsReviewsWhoseRunIsGone(t *testing.T) {
	h := reviewHarness(t)
	opts := reviewOptions()
	opts.StateDir = t.TempDir()
	opts.ReviewTimeout = time.Hour
	met, err := h.runLoopUntil(opts, 10*time.Second, func() bool { return len(h.elk.CallsTo("ask_elk")) > 0 })
	if err != nil || !met {
		t.Fatalf("stopping: %v", err)
	}
	emptyQueue(h)
	h.elk.Text("claim_run", `Queue "`+queue+`" is clear: no runs waiting in "`+space+`".`)
	h.elk.Handle("ask_elk", func(map[string]any) elktest.Reply { return elktest.Reply{Text: "Not recorded: Run not found."} })
	if err := h.run(opts); err != nil {
		t.Fatal(err)
	}
	if len(h.agent.Specs()) != 1 {
		t.Fatalf("started an agent for a missing run: %d\n%s", len(h.agent.Specs()), h.log)
	}
	files, err := os.ReadDir(filepath.Join(opts.StateDir, "reviews"))
	if err != nil || len(files) != 0 {
		t.Fatalf("pending reviews after missing run: %v, %v", files, err)
	}
}
