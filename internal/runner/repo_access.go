package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"sort"

	"github.com/elk-work/rein/internal/config"
)

// A queue whose repository folder this process may not read claims nothing
// (ark:rein#172).
//
// macOS keeps a background program out of ~/Documents, ~/Desktop and
// ~/Downloads unless it has Full Disk Access, and a launchd agent cannot raise
// the question, so the refusal is silent: `git worktree add` fails with
// "Unable to read current working directory: Operation not permitted". Before
// this check every claim on such a queue burned a run — a recurring walk twice
// in a day on one laptop — and put a stopped run on its owner's Decisions.
// Elk has no tool to give a claim back, so the only way to leave a run queued
// for another agent is not to claim it: the serve loop asks [repoAccessBlock]
// before every claim and, while it answers, claims nothing and says why on the
// queue's beat. A run that is claimed anyway — one naming a repository other
// than the queue's default — stops before its worktree with the same sentence
// and the worktree_init stop code, which Elk's mechanical recovery restarts on
// another ready queue.

// repoReadable reports whether this process may list path. Only a permission
// refusal counts as unreadable: a folder that does not exist is a different
// fault, and the worktree step already says so in its own words.
func repoReadable(path string) (ok bool, err error) {
	_, err = os.ReadDir(path)
	if err != nil && errors.Is(err, fs.ErrPermission) {
		return false, err
	}
	return true, nil
}

// repoAccessBlock is the reason a queue must not claim, or "" when it may.
//
// It looks at what a claim is most likely to need. A queue with a default
// repository is blocked when that folder cannot be read; one without a
// default is blocked only when every repository it can reach is unreadable,
// because a run naming a readable one would still succeed.
func repoAccessBlock(cfg config.Config, q config.Queue) string {
	if res, err := ResolveRepo(cfg, q, ""); err == nil {
		if ok, rerr := repoReadable(res.Path); !ok {
			return notClaiming(repoAccessMessage(res.Path, rerr))
		}
		return ""
	}
	repos := cfg.RepositoriesFor(q)
	if len(repos) == 0 {
		return ""
	}
	names := make([]string, 0, len(repos))
	for name := range repos {
		names = append(names, name)
	}
	sort.Strings(names)
	var first string
	var firstErr error
	for _, name := range names {
		path := expand(repos[name])
		ok, rerr := repoReadable(path)
		if ok {
			return ""
		}
		if first == "" {
			first, firstErr = path, rerr
		}
	}
	return notClaiming(repoAccessMessage(first, firstErr))
}

func notClaiming(why string) string {
	return "not claiming: " + why + " Until then this queue claims nothing and its runs stay queued for another agent."
}

// repoAccessMessage says, in words a person can act on, why Rein cannot use
// the folder at path. It goes in the log, the queue's beat and, for a run
// claimed anyway, its stuck report.
func repoAccessMessage(path string, err error) string {
	cause := "permission denied"
	var pe *fs.PathError
	if errors.As(err, &pe) {
		cause = pe.Err.Error()
	}
	if runtime.GOOS != "darwin" {
		return fmt.Sprintf("Rein cannot read the repository folder %s (%s). "+
			"Give the user Rein runs as read access to it, or point the queue at a checkout it can read.", path, cause)
	}
	program := "the rein program"
	if exe, e := os.Executable(); e == nil {
		program = exe
	}
	return fmt.Sprintf("Rein cannot read the repository folder %s (%s). "+
		"macOS keeps background programs out of Documents, Desktop and Downloads, so this folder needs "+
		"Full Disk Access for the Rein service: System Settings > Privacy & Security > Full Disk Access, add %s, "+
		"then restart the worker. Or move the checkout somewhere else in your home folder.", path, cause, program)
}
