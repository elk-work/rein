package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// An agent can write files in its worktree that git later executes: a nested
// repository it adds as a submodule whose own config sets core.fsmonitor, or
// hooks it points git at. Rein's own git calls on that worktree — status,
// rev-parse, rev-list, ls-remote, worktree remove — run outside any agent's
// sandbox, so that code would run as the person, on every adapter
// (`ark:rein#152 (01M74SV026QTFD220PNM4J2Q92)`).
//
// [SafeGitArgs] closes that: every git command Rein runs where an agent has
// been goes through it. Command-line config outranks every config file, and
// git hands it to the child processes it starts in submodules through
// GIT_CONFIG_PARAMETERS, so a submodule's own config cannot undo it either.

// noHooks is the null device, for core.hooksPath: git looks for hooks under
// it and finds none, and unlike a directory nothing can ever be put there —
// not by an agent whose sandbox may write to the temporary directory, and not
// by anyone else (ark:rein#152's review).
func noHooks() string { return os.DevNull }

// SafeGitArgs are the options Rein puts in front of every git command it runs
// on a run's worktree: no hooks, no filesystem monitor, and no recursion into
// submodules, whose configuration the agent controls.
func SafeGitArgs() []string {
	return []string{
		"-c", "core.hooksPath=" + noHooks(),
		"-c", "core.fsmonitor=false",
		"-c", "submodule.recurse=false",
		"-c", "diff.ignoreSubmodules=all",
		"-c", "status.submoduleSummary=false",
	}
}

// SafeGit prefixes args with [SafeGitArgs].
func SafeGit(args ...string) []string {
	return append(SafeGitArgs(), args...)
}

// PinnedGit says where and how to run git on a run's worktree so that git
// reads nothing the agent can write: the directory to run in and the
// environment entries to add.
//
// A linked worktree finds its repository through two files an agent can
// rewrite — `.git` in the worktree, and `commondir` in the worktree's own
// entry, which some adapters' sandboxes may write (grok's does). Pointed at a
// git directory of the agent's own making, either would hand git a config
// whose core.sshCommand or remote.origin.uploadpack runs for the landing
// check's `ls-remote`, as the person. Checking those files first is not
// enough: a relative path can be resolved differently from how git resolves
// it, and a process the agent left running can swap them between the check
// and the call (both found by the independent review of ark:rein#152).
//
// So git is told where everything is instead: GIT_DIR is the worktree's entry
// under the source repository's `worktrees/` directory and GIT_COMMON_DIR the
// source repository's own git directory, which outrank both files, and git
// runs in the source repository, never in the worktree. The source repository
// is the person's checkout and is trusted. The entry is the one git made for
// this directory: it must exist, be a real directory, and name this worktree.
//
// A hosted worktree is a plain clone inside a container: it runs in place.
func (wt *Worktree) PinnedGit(ctx context.Context, git string) (dir string, env []string, err error) {
	if wt == nil || wt.Dir == "" {
		return "", nil, fmt.Errorf("worktree: no worktree")
	}
	if wt.Hosted || wt.Repo == "" {
		return wt.Dir, nil, nil
	}
	if git == "" {
		git = "git"
	}
	cmd := exec.CommandContext(ctx, git, SafeGit("-C", wt.Repo, "rev-parse", "--path-format=absolute", "--git-common-dir")...)
	out, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("worktree: resolve %s's git directory: %w", wt.Repo, err)
	}
	common := strings.TrimSpace(string(out))
	entry := filepath.Join(common, "worktrees", filepath.Base(wt.Dir))
	fi, err := os.Lstat(entry)
	if err != nil || !fi.IsDir() {
		return "", nil, fmt.Errorf("worktree: %s has no worktree entry %s; refusing to run git there", wt.Repo, entry)
	}
	// `gitdir` names the worktree the entry belongs to. It is advisory — an
	// agent able to write the entry can change it — so it only guards against
	// pinning git to some other run's entry; it decides nothing about safety.
	back, err := os.ReadFile(filepath.Join(entry, "gitdir"))
	if err != nil || !sameFile(strings.TrimSpace(string(back)), filepath.Join(wt.Dir, ".git")) {
		return "", nil, fmt.Errorf("worktree: %s does not belong to %s; refusing to run git there", entry, wt.Dir)
	}
	return wt.Repo, []string{"GIT_DIR=" + entry, "GIT_COMMON_DIR=" + common}, nil
}

func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(filepath.Dir(a))
	rb, errB := filepath.EvalSymlinks(filepath.Dir(b))
	return errA == nil && errB == nil && filepath.Join(ra, filepath.Base(a)) == filepath.Join(rb, filepath.Base(b))
}
