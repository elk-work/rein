package worktree_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/worktree"
)

// A check names a commit the machine's checkout may not have yet — pushed to
// origin after the clone was last fetched. Create fetches it by name and cuts
// the worktree detached there, with no branch.
func TestCreateAtACommitFetchesItAndDetaches(t *testing.T) {
	clone, origin := newRepoWithOrigin(t)
	write(t, filepath.Join(origin, "later.txt"), "pushed after the clone\n")
	commit(t, origin, "later")
	sha := git(t, origin, "rev-parse", "HEAD")
	if out, err := runGit(clone, "cat-file", "-e", sha+"^{commit}"); err == nil {
		t.Fatalf("the clone already has the commit; the test proves nothing (%s)", out)
	}

	root := t.TempDir()
	m := &worktree.Manager{Root: root}
	wt, err := m.Create(context.Background(), worktree.Request{Repo: clone, RunID: "run-check", Commit: sha})
	if err != nil {
		t.Fatal(err)
	}
	if !wt.Detached || wt.Branch != "" || wt.BaseSHA != sha || wt.BaseRef != sha {
		t.Errorf("worktree: %+v", wt)
	}
	if got := git(t, wt.Dir, "rev-parse", "HEAD"); got != sha {
		t.Errorf("HEAD = %s, want %s", got, sha)
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, "later.txt")); err != nil {
		t.Errorf("the worktree is not the fetched commit: %v", err)
	}
	if !strings.Contains(wt.BaseNote, "fetched from origin") {
		t.Errorf("note = %q", wt.BaseNote)
	}
	if err := m.Reap(context.Background(), wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Errorf("the worktree survived the reap: %v", err)
	}
}

// A commit origin does not have is a stuck run naming repo_access, and
// nothing is left on disk.
func TestCreateAtAnUnfetchableCommitLeavesNothing(t *testing.T) {
	clone, _ := newRepoWithOrigin(t)
	root := filepath.Join(t.TempDir(), "work")
	m := &worktree.Manager{Root: root}
	missing := strings.Repeat("0123456789", 4)
	_, err := m.Create(context.Background(), worktree.Request{Repo: clone, RunID: "run-gone", Commit: missing})
	if err == nil {
		t.Fatal("Create succeeded at a commit nobody has")
	}
	var stop worktree.InitStop
	if !errors.As(err, &stop) || stop.Code != "repo_access" || !strings.Contains(stop.Line, "could not be fetched") {
		t.Fatalf("err = %v, want an InitStop naming repo_access", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Errorf("Create left %s behind: %v", root, statErr)
	}
}

// runGit is git without failing the test, for probes whose failure is the
// answer.
func runGit(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}

func TestCreateRefusesAnAbbreviatedCommit(t *testing.T) {
	clone, _ := newRepoWithOrigin(t)
	short := git(t, clone, "rev-parse", "--short", "HEAD")
	m := &worktree.Manager{Root: t.TempDir()}
	_, err := m.Create(context.Background(), worktree.Request{Repo: clone, RunID: "run-short", Commit: short})
	var stop worktree.InitStop
	if !errors.As(err, &stop) || stop.Code != "repo_access" {
		t.Fatalf("err = %v, want a refusal", err)
	}
}
