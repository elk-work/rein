package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/worktree"
)

func win02() config.YieldToGitHub {
	return config.YieldToGitHub{
		Runner: "win-02",
		Labels: []string{"self-hosted", "Windows", "win11", "ui-lab", "win-02"},
		Repos:  []string{"elk-work/scout"},
		Grace:  config.Duration(time.Minute),
	}
}

// GitHub's rule for which runner may take a job is that every label in its
// runs-on is on the runner. A job pinned to win-01 is not win-02's.
func TestPriorityJobsMatchThisRunnerOnly(t *testing.T) {
	jobs := []PriorityJob{
		{Name: "device check", Status: "queued", Labels: []string{"self-hosted", "windows", "win11", "ui-lab", "win-02"}},
		{Name: "port win-01", Status: "queued", Labels: []string{"self-hosted", "windows", "win11", "ui-lab", "win-01"}},
		{Name: "android", Status: "queued", Labels: []string{"self-hosted", "macos", "mac-lab"}},
		{Name: "no labels", Status: "queued"},
		{Name: "running here", Status: "in_progress", Runner: "WIN-02"},
		{Name: "running there", Status: "in_progress", Runner: "win-01", Labels: []string{"self-hosted", "windows"}},
	}
	var names []string
	for _, j := range priorityJobs(win02(), jobs) {
		names = append(names, j.Name)
	}
	if got := strings.Join(names, ","); got != "device check,running here" {
		t.Errorf("priority jobs = %q, want device check,running here", got)
	}
}

// A failed lookup holds claims but never stops a run: only a job actually
// seen does.
func TestAFailedLookupNeverStopsARun(t *testing.T) {
	now := time.Now()
	g := newYieldGate(win02(), func(context.Context, config.YieldToGitHub) ([]PriorityJob, error) {
		return nil, errors.New("gh api: HTTP 502")
	}, func(string, ...any) {})
	g.now = func() time.Time { return now }
	wctx, release := g.watch(context.Background(), "run-1")
	defer release()
	for i := 0; i < 3; i++ {
		g.refresh(context.Background())
		now = now.Add(time.Hour)
	}
	if wctx.Err() != nil {
		t.Fatal("a failed lookup stopped the agent")
	}
	if why := g.hold(context.Background()); !strings.Contains(why, "could not ask GitHub") {
		t.Errorf("hold = %q, want the fail-closed sentence", why)
	}
}

// The grace is counted from when the job was first seen, and a session that
// starts after it has run out is stopped at once.
func TestTheGraceRunsFromTheFirstSighting(t *testing.T) {
	now := time.Now()
	var (
		mu  sync.Mutex
		job []PriorityJob
	)
	g := newYieldGate(win02(), func(context.Context, config.YieldToGitHub) ([]PriorityJob, error) {
		mu.Lock()
		defer mu.Unlock()
		return job, nil
	}, func(string, ...any) {})
	g.now = func() time.Time { return now }
	wctx, release := g.watch(context.Background(), "run-1")
	defer release()

	g.refresh(context.Background())
	if g.hold(context.Background()) != "" {
		t.Fatal("held with no job waiting")
	}
	mu.Lock()
	job = []PriorityJob{{Name: "device check", Status: "queued", Labels: []string{"self-hosted", "windows", "win-02"}}}
	mu.Unlock()
	g.refresh(context.Background())
	now = now.Add(59 * time.Second)
	g.refresh(context.Background())
	if wctx.Err() != nil {
		t.Fatal("stopped before the grace ran out")
	}
	now = now.Add(time.Second)
	g.refresh(context.Background())
	if !errors.Is(context.Cause(wctx), errYielded) {
		t.Fatalf("not stopped once the grace ran out: cause %v", context.Cause(wctx))
	}

	late, lateRelease := g.watch(context.Background(), "run-2")
	defer lateRelease()
	if !errors.Is(context.Cause(late), errYielded) {
		t.Error("a session started after the grace ran out was not stopped at once")
	}

	// The job clears: the next sighting starts a fresh grace.
	mu.Lock()
	job = nil
	mu.Unlock()
	g.refresh(context.Background())
	if !g.since.IsZero() || g.hold(context.Background()) != "" {
		t.Error("the gate still holds after the job cleared")
	}
}

// git runs git in dir and fails the test on error.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A stopped run's committed work reaches origin before its worktree goes.
func TestPushRunBranchPushesCommittedWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	src := filepath.Join(root, "src")
	gitIn(t, root, "init", "--bare", "-q", origin)
	gitIn(t, root, "init", "-q", src)
	gitIn(t, src, "commit", "-q", "--allow-empty", "-m", "base")
	gitIn(t, src, "remote", "add", "origin", origin)
	base := gitIn(t, src, "rev-parse", "HEAD")
	dir := filepath.Join(root, "wt")
	gitIn(t, src, "worktree", "add", "-q", "-b", "rein/run-1", dir)
	wt := &worktree.Worktree{Dir: dir, Repo: src, Branch: "rein/run-1", BaseSHA: base}
	qr := &queueRunner{}

	if pushed, note := qr.pushRunBranch(context.Background(), wt); pushed || note != "" {
		t.Fatalf("a branch with no commits was pushed (%v, %q)", pushed, note)
	}
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "work")
	pushed, note := qr.pushRunBranch(context.Background(), wt)
	if !pushed {
		t.Fatalf("not pushed: %s", note)
	}
	if got, want := gitIn(t, origin, "rev-parse", "refs/heads/rein/run-1"), gitIn(t, dir, "rev-parse", "HEAD"); got != want {
		t.Errorf("origin has %s, want %s", got, want)
	}
}

// The real lookup, against a stand-in gh that answers as GitHub's API does.
func TestGitHubPriorityJobsReadsRunsAndJobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in gh is a shell script")
	}
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	script := `#!/bin/sh
case "$*" in
  *"workflows/port-train.yml/runs?per_page=100&status=queued"*) echo '{"id":7,"path":".github/workflows/port-train.yml"}' ;;
  *"workflows/port-train.yml/runs?per_page=100&status=in_progress"*) echo '{"id":7,"path":".github/workflows/port-train.yml"}' ;;
  *"runs/7/jobs"*) echo '{"name":"port-windows (win-02)","status":"queued","runner_name":null,"labels":["self-hosted","windows","win-02"],"html_url":"u"}' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	y := win02()
	y.Workflows = []string{"port-train.yml"}
	jobs, err := GitHubPriorityJobs(gh)(context.Background(), y)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want one (a run listed twice is read once)", jobs)
	}
	if j := jobs[0]; j.Workflow != "port-train.yml" || j.Repo != "elk-work/scout" || j.Status != "queued" {
		t.Errorf("job = %+v", j)
	}

	y.Workflows = []string{"other.yml"}
	if _, err := GitHubPriorityJobs(gh)(context.Background(), y); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Errorf("a gh failure was not reported: %v", err)
	}
}
