package worktree

import (
	"context"
	"fmt"
	"os/exec"
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
// reads nothing the agent can write: the directory to run in, the environment
// entries to add, and the ref that stands for the run's work.
//
// A linked worktree finds its repository through files an agent can rewrite:
// `.git` in the worktree, and `commondir` and `config.worktree` in the
// worktree's own entry under the repository's `worktrees/` directory, which
// some adapters' sandboxes may write (grok's does). Pointed at a git
// directory of the agent's own making, or given a config of its own, git
// would run its core.sshCommand or remote.origin.uploadpack for the landing
// check's `ls-remote`, as the person. Checking those files first is not
// enough: a relative path can resolve differently in Go than in git, and a
// process the agent left running can swap them between the check and the
// call (the independent review of ark:rein#152 reproduced all three).
//
// So git never looks at the worktree or its entry: GIT_DIR is the source
// repository's own git directory, git runs in the source repository, and the
// run's work is its branch, refs/heads/<branch>, which lives there. The source
// repository is the person's checkout and is trusted.
//
// A hosted worktree is a plain clone inside a container: it runs in place,
// and its work is HEAD.
func (wt *Worktree) PinnedGit(ctx context.Context, git string) (dir string, env []string, ref string, err error) {
	if wt == nil || wt.Dir == "" {
		return "", nil, "", fmt.Errorf("worktree: no worktree")
	}
	if wt.Hosted {
		return wt.Dir, nil, "HEAD", nil
	}
	if wt.Repo == "" || wt.Branch == "" {
		return "", nil, "", fmt.Errorf("worktree: %s names no source repository and branch; refusing to run git there", wt.Dir)
	}
	if git == "" {
		git = "git"
	}
	cmd := exec.CommandContext(ctx, git, SafeGit("-C", wt.Repo, "rev-parse", "--path-format=absolute", "--git-common-dir")...)
	out, err := cmd.Output()
	if err != nil {
		return "", nil, "", fmt.Errorf("worktree: resolve %s's git directory: %w", wt.Repo, err)
	}
	common := strings.TrimSpace(string(out))
	return wt.Repo, []string{"GIT_DIR=" + common}, "refs/heads/" + wt.Branch, nil
}
