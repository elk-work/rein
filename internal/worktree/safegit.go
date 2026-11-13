package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

var (
	noHooksOnce sync.Once
	noHooksDir  string
)

// noHooks is an empty directory for core.hooksPath: git looks for hooks there
// and finds none. It is made once per process; if it cannot be made, the null
// device serves the same purpose (no hook can live under it).
func noHooks() string {
	noHooksOnce.Do(func() {
		dir, err := os.MkdirTemp("", "rein-no-hooks-")
		if err != nil {
			noHooksDir = os.DevNull
			return
		}
		noHooksDir = dir
	})
	return noHooksDir
}

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

// CheckGitLink verifies that a linked worktree's `.git` is still the file git
// wrote when the worktree was made, pointing at this worktree's own entry
// under the source repository's `worktrees/` directory.
//
// The agent can rewrite that file. Pointed at a git directory of its own
// making, git would read that directory's config — core.sshCommand for the
// `ls-remote` the landing check runs, for one — and run it as the person.
// Command-line overrides cover the options Rein knows about; this covers the
// rest by refusing to run git there at all. A hosted worktree is a plain clone
// inside a container and has no link to check.
func (wt *Worktree) CheckGitLink(ctx context.Context, git string) error {
	if wt == nil || wt.Hosted || wt.Repo == "" || wt.Dir == "" {
		return nil
	}
	if git == "" {
		git = "git"
	}
	link := filepath.Join(wt.Dir, ".git")
	fi, err := os.Lstat(link)
	if err != nil {
		return fmt.Errorf("worktree: %s: %w", link, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("worktree: %s is no longer the file git wrote; refusing to run git there", link)
	}
	raw, err := os.ReadFile(link)
	if err != nil {
		return fmt.Errorf("worktree: %s: %w", link, err)
	}
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, "gitdir:") || strings.Contains(line, "\n") {
		return fmt.Errorf("worktree: %s does not hold a single gitdir line; refusing to run git there", link)
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(wt.Dir, gitdir)
	}
	// The source repository is the person's own checkout, not the agent's, so
	// asking it where its common directory is runs nothing the agent wrote.
	cmd := exec.CommandContext(ctx, git, SafeGit("-C", wt.Repo, "rev-parse", "--path-format=absolute", "--git-common-dir")...)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("worktree: resolve %s's git directory: %w", wt.Repo, err)
	}
	common, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	got, err := filepath.EvalSymlinks(gitdir)
	if err != nil {
		return fmt.Errorf("worktree: %s points at %s, which is gone: %w", link, gitdir, err)
	}
	want := filepath.Join(common, "worktrees") + string(filepath.Separator)
	rel := strings.TrimPrefix(got, want)
	if rel == got || rel == "" || strings.ContainsRune(rel, filepath.Separator) {
		return fmt.Errorf("worktree: %s points at %s, outside %s's worktrees; refusing to run git there", link, got, wt.Repo)
	}
	return nil
}
