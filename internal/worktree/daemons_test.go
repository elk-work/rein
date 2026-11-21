//go:build !windows

package worktree_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/worktree"
)

const (
	gradleDaemonMain = "org.gradle.launcher.daemon.bootstrap.GradleDaemon"
	standInEnv       = "REIN_TEST_STAND_IN_PROCESS"
)

// TestMain lets the test binary stand in for a long-lived process: started
// again with standInEnv set, it only waits. What it is taken for is decided by
// the arguments it was started with, as it is for a real daemon.
func TestMain(m *testing.M) {
	if os.Getenv(standInEnv) != "" {
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// standIn starts a process whose working directory is dir and whose command
// line carries marker. done is closed when it has exited.
func standIn(t *testing.T, dir, marker string) (pid int, done <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "--", marker)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), standInEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	gone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(gone) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-gone })
	return cmd.Process.Pid, gone
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func needLsof(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof is not installed")
	}
}

// The 2026-11-20 finding: a Gradle daemon the agent's build started inside the
// worktree outlived the run, and every reap left the worktree for it.
func TestReapStopsABuildDaemonLeftInTheWorktree(t *testing.T) {
	needLsof(t)
	repo := newRepo(t)
	var log []string
	m := &worktree.Manager{Root: t.TempDir(), Logf: func(f string, a ...any) { log = append(log, f) }}
	ctx := context.Background()
	wt, err := m.Create(ctx, worktree.Request{Repo: repo, RunID: "run-gradle"})
	if err != nil {
		t.Fatal(err)
	}
	// One daemon held by this run's worktree, and the same daemon working
	// somewhere else: another run's, or the person's own build.
	elsewhere := t.TempDir()
	inside, insideGone := standIn(t, wt.Dir, gradleDaemonMain)
	outside, _ := standIn(t, elsewhere, gradleDaemonMain)

	if err := m.Reap(ctx, wt); err != nil {
		t.Fatalf("Reap with a build daemon left in the worktree: %v", err)
	}
	select {
	case <-insideGone:
	case <-time.After(10 * time.Second):
		t.Fatalf("the daemon in the worktree (pid %d) was not stopped", inside)
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Errorf("the worktree survived the reap: %v", err)
	}
	if strings.Contains(git(t, repo, "branch", "--list", wt.Branch), wt.Branch) {
		t.Error("the branch survived the reap")
	}
	if !alive(outside) {
		t.Fatalf("a daemon working outside the worktree (pid %d) was stopped", outside)
	}
	if !strings.Contains(strings.Join(log, "\n"), "stopped the %s") {
		t.Errorf("the log does not say a daemon was stopped: %q", log)
	}
}

// What ark:rein#80 protects is unchanged: a process in the worktree that is
// not a build daemon is work, is left running, and still stops the reap.
func TestReapStopsNothingButBuildDaemons(t *testing.T) {
	needLsof(t)
	repo := newRepo(t)
	m := &worktree.Manager{Root: t.TempDir()}
	ctx := context.Background()
	wt, err := m.Create(ctx, worktree.Request{Repo: repo, RunID: "run-lane"})
	if err != nil {
		t.Fatal(err)
	}
	lane, _ := standIn(t, wt.Dir, "a-background-lane-of-the-run")

	err = m.Reap(ctx, wt)
	if !errors.Is(err, worktree.ErrInUse) {
		t.Fatalf("Reap = %v, want ErrInUse while the run's own work is in the worktree", err)
	}
	if !alive(lane) {
		t.Fatal("a process that is not a build daemon was stopped")
	}
	if _, err := os.Stat(wt.Dir); err != nil {
		t.Fatalf("the worktree was removed under a working process: %v", err)
	}
}
