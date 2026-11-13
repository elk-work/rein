package worktree_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/worktree"
)

// marker writes an executable that records it ran, and returns its path and
// the file it leaves behind.
func marker(t *testing.T, name string) (script, ran string) {
	t.Helper()
	dir := t.TempDir()
	ran = filepath.Join(dir, name+".ran")
	script = filepath.Join(dir, name)
	write(t, script, "#!/bin/sh\ntouch "+ran+"\nexit 1\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	return script, ran
}

func ran(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// gitStatus runs `git status` in dir, with or without Rein's overrides, and
// does not care whether it succeeds: the question is what it executed.
func gitStatus(dir string, safe bool) {
	args := []string{"-C", dir, "status", "--porcelain"}
	if safe {
		args = worktree.SafeGit(args...)
	}
	_ = exec.Command("git", args...).Run()
}

// A nested repository the agent adds as a submodule carries its own config.
// Its core.fsmonitor runs on any recursive status in the parent; Rein's git
// must not run it (ark:rein#152).
func TestSafeGitRunsNoFsmonitorFromASubmodule(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in fsmonitor is a shell script")
	}
	repo := newRepo(t)
	nested := newRepo(t)
	git(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", nested, "vendor/nested")
	commit(t, repo, "add a submodule")
	sub := filepath.Join(repo, "vendor", "nested")
	script, ranAt := marker(t, "fsmonitor")
	git(t, sub, "config", "core.fsmonitor", script)

	gitStatus(repo, true)
	if ran(ranAt) {
		t.Fatal("Rein's git status ran the submodule's fsmonitor")
	}
	// The control: without the overrides git does run it, so the assertion
	// above is about the overrides and not about this git's behaviour.
	gitStatus(repo, false)
	if !ran(ranAt) {
		t.Skip("this git does not run a submodule's fsmonitor on status; nothing to prove")
	}
}

// Hooks and the filesystem monitor configured on the repository itself are
// switched off too.
func TestSafeGitRunsNoHookAndNoFsmonitor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-ins are shell scripts")
	}
	repo := newRepo(t)
	fsmon, fsmonRan := marker(t, "fsmonitor")
	git(t, repo, "config", "core.fsmonitor", fsmon)
	hooks := t.TempDir()
	hook, hookRan := marker(t, "hook")
	if err := os.Rename(hook, filepath.Join(hooks, "pre-commit")); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "core.hooksPath", hooks)

	gitStatus(repo, true)
	write(t, filepath.Join(repo, "f"), "x\n")
	_ = exec.Command("git", worktree.SafeGit("-C", repo, "add", "f")...).Run()
	out, err := exec.Command("git", worktree.SafeGit("-C", repo, "commit", "-qm", "c")...).CombinedOutput()
	if err != nil {
		t.Fatalf("commit with the overrides failed: %v\n%s", err, out)
	}
	if ran(fsmonRan) {
		t.Error("Rein's git ran the repository's fsmonitor")
	}
	if ran(hookRan) {
		t.Error("Rein's git ran the repository's pre-commit hook")
	}
}

func TestCheckGitLink(t *testing.T) {
	repo := newRepo(t)
	m := &worktree.Manager{Root: t.TempDir(), Ark: "definitely-not-installed"}
	wt, err := m.Create(context.Background(), worktree.Request{Repo: repo, RunID: "link-1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := wt.CheckGitLink(ctx, ""); err != nil {
		t.Fatalf("a fresh worktree failed the link check: %v", err)
	}

	link := filepath.Join(wt.Dir, ".git")
	original, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	restore := func() {
		_ = os.RemoveAll(link)
		write(t, link, string(original))
	}

	// Repointed at a git directory of the agent's own making.
	elsewhere := newRepo(t)
	write(t, link, "gitdir: "+filepath.Join(elsewhere, ".git")+"\n")
	if err := wt.CheckGitLink(ctx, ""); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("a repointed .git passed: %v", err)
	}

	// Repointed at another worktree's entry is still inside worktrees/, which
	// the agent cannot write, so it is not this check's business — but a path
	// that climbs out of worktrees/ is.
	common := git(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	write(t, link, "gitdir: "+filepath.Join(common, "worktrees", "..")+"\n")
	if err := wt.CheckGitLink(ctx, ""); err == nil {
		t.Error("a .git pointing at the common directory itself passed")
	}

	// Replaced by a directory.
	restore()
	_ = os.Remove(link)
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := wt.CheckGitLink(ctx, ""); err == nil {
		t.Error("a .git directory in place of the link passed")
	}

	restore()
	if err := wt.CheckGitLink(ctx, ""); err != nil {
		t.Errorf("the restored link failed: %v", err)
	}

	hosted := &worktree.Worktree{Dir: t.TempDir(), Repo: "x", Hosted: true}
	if err := hosted.CheckGitLink(ctx, ""); err != nil {
		t.Errorf("a hosted clone was link-checked: %v", err)
	}
}
