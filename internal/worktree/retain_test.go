package worktree_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/elk-work/rein/internal/worktree"
)

func TestReapKeepsBranchAfterFailedPush(t *testing.T) {
	for _, name := range []string{"failed", "pushed", "failed-after-earlier-push"} {
		pushed := name == "pushed"
		t.Run(name, func(t *testing.T) {
			repo, origin := newRepoWithOrigin(t)
			m := &worktree.Manager{Root: t.TempDir()}
			wt, err := m.Create(context.Background(), worktree.Request{Repo: repo, RunID: "retain"})
			if err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(wt.Dir, "work.txt"), "valuable work")
			commit(t, wt.Dir, "run work")
			if name == "failed-after-earlier-push" {
				git(t, wt.Dir, "push", "origin", "HEAD")
				write(t, filepath.Join(wt.Dir, "work.txt"), "more valuable work")
				commit(t, wt.Dir, "more work")
			}
			tip := git(t, wt.Dir, "rev-parse", "HEAD")
			if !pushed {
				git(t, wt.Dir, "remote", "set-url", "origin", filepath.Join(origin, "missing.git"))
			}
			_, pushErr := runGit(wt.Dir, "push", "origin", "HEAD")
			if (pushErr == nil) != pushed {
				t.Fatalf("push result: %v", pushErr)
			}
			if err := m.Reap(context.Background(), wt); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
				t.Fatal("worktree was not removed")
			}
			got, err := runGit(repo, "rev-parse", "--verify", "refs/heads/"+wt.Branch)
			if pushed && err == nil {
				t.Fatal("pushed branch retained")
			}
			if !pushed && (err != nil || len(got) < len(tip) || got[:len(tip)] != tip) {
				t.Fatalf("unpushed commit lost: %s %v", got, err)
			}
		})
	}
}
