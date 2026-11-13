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

// fakeGitDir makes a git directory of the agent's own making whose origin
// runs a marker as its upload-pack, the way an agent would get a command run
// by Rein's `ls-remote`.
func fakeGitDir(t *testing.T, at, origin, uploadpack string) {
	t.Helper()
	for _, d := range []string{"objects", "refs/heads"} {
		if err := os.MkdirAll(filepath.Join(at, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(at, "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(at, "config"), "[core]\n\trepositoryformatversion = 0\n[remote \"origin\"]\n\turl = "+origin+"\n\tuploadpack = "+uploadpack+"\n")
}

// lsRemote runs the landing check's ls-remote the way Rein does: pinned, or
// (for the control) plainly inside the worktree.
func lsRemote(t *testing.T, wt *worktree.Worktree, pinned bool) {
	t.Helper()
	cmd := exec.Command("git", worktree.SafeGit("ls-remote", "--heads", "origin")...)
	cmd.Dir = wt.Dir
	if pinned {
		dir, env, _, err := wt.PinnedGit(context.Background(), "")
		if err != nil {
			t.Fatalf("PinnedGit: %v", err)
		}
		cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
	}
	_ = cmd.Run()
}

// The three routes the independent review of ark:rein#152 reproduced: an agent
// rewrites its worktree's .git (by a relative path through a symlink, or
// outright), or the commondir or config.worktree file in the worktree's own
// entry, to reach a config whose upload-pack runs as the person. Pinned git
// reads none of them.
func TestPinnedGitReadsNothingTheAgentWrote(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in upload-pack is a shell script")
	}
	ctx := context.Background()
	for _, route := range []string{"dot-git", "commondir", "config-worktree"} {
		t.Run(route, func(t *testing.T) {
			clone, origin := newRepoWithOrigin(t)
			m := &worktree.Manager{Root: t.TempDir(), Ark: "definitely-not-installed"}
			wt, err := m.Create(ctx, worktree.Request{Repo: clone, RunID: "pin-" + route})
			if err != nil {
				t.Fatal(err)
			}
			script, ranAt := marker(t, "uploadpack")
			fake := filepath.Join(wt.Dir, "x")
			fakeGitDir(t, fake, origin, script)
			switch route {
			case "dot-git":
				write(t, filepath.Join(wt.Dir, ".git"), "gitdir: x\n")
			case "commondir":
				common := git(t, clone, "rev-parse", "--path-format=absolute", "--git-common-dir")
				write(t, filepath.Join(common, "worktrees", filepath.Base(wt.Dir), "commondir"), fake+"\n")
			case "config-worktree":
				// `git sparse-checkout init` turns this on in the person's repository.
				git(t, clone, "config", "extensions.worktreeConfig", "true")
				common := git(t, clone, "rev-parse", "--path-format=absolute", "--git-common-dir")
				write(t, filepath.Join(common, "worktrees", filepath.Base(wt.Dir), "config.worktree"),
					"[remote \"origin\"]\n\tuploadpack = "+script+"\n")
			}

			lsRemote(t, wt, true)
			if ran(ranAt) {
				t.Fatal("pinned git ran the agent's upload-pack")
			}
			lsRemote(t, wt, false)
			if !ran(ranAt) {
				t.Fatal("control: plain git did not run the agent's upload-pack, so the test proves nothing")
			}
		})
	}
}

func TestPinnedGitSeesTheRunsCommits(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	m := &worktree.Manager{Root: t.TempDir(), Ark: "definitely-not-installed"}
	wt, err := m.Create(ctx, worktree.Request{Repo: repo, RunID: "pin-count"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt.Dir, "new"), "x\n")
	commit(t, wt.Dir, "the run's change")
	dir, env, ref, err := wt.PinnedGit(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if dir != wt.Repo || ref != "refs/heads/"+wt.Branch {
		t.Errorf("pinned git runs in %s on %s, want the source repository %s on the run's branch", dir, ref, wt.Repo)
	}
	cmd := exec.Command("git", worktree.SafeGit("rev-list", "--count", wt.BaseSHA+".."+ref)...)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "1" {
		t.Errorf("pinned rev-list = %q, %v; want 1", out, err)
	}

	// A worktree that names no source repository is refused, not run in place.
	bare := *wt
	bare.Repo = ""
	if _, _, _, err := bare.PinnedGit(ctx, ""); err == nil {
		t.Error("a worktree with no source repository was run unpinned")
	}

	hosted := &worktree.Worktree{Dir: t.TempDir(), Repo: "x", Hosted: true}
	if d, env, ref, err := hosted.PinnedGit(ctx, ""); err != nil || d != hosted.Dir || env != nil || ref != "HEAD" {
		t.Errorf("a hosted clone was pinned: %s %v %s %v", d, env, ref, err)
	}
}
