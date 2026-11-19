package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
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
// Two checks keep such a run queued, neither of which ends it stuck:
//
//   - Before claiming, the serve loop asks [repoAccessBlock]: a queue whose
//     default folder (or every folder, with no default) cannot be read claims
//     nothing and says why on the queue's beat.
//   - After claiming, [queueRunner.declineUnreadable] resolves the run's own
//     repository before anything names the run. Elk's claim_run only HOLDS a
//     run (scout 0790): it stays `queued` until the runner's first run-scoped
//     call, and a hold nobody acknowledges lapses in a minute and the run is
//     offered again. So a run whose folder cannot be read is declined by
//     saying nothing about it. Elk's router moves a run nobody has started to
//     its next choice after thirty minutes (scout 0299), and the repo:
//     capability for an unreadable folder is not declared (hostcap.go), so the
//     router's next choice is a machine that can read it.

// maxDeclinesPerClaim bounds how many unreadable runs one claim passes over
// before giving up until the next poll.
const maxDeclinesPerClaim = 3

// declineUnreadable reports whether wo names a repository this process may
// not read, and if so leaves it unstarted: it makes no call that names the
// run, so Elk's hold lapses and the run stays queued for another agent. It
// must run before anything reports on the run.
func (qr *queueRunner) declineUnreadable(wo *elk.WorkOrder) bool {
	if qr.r.opts.Hosted {
		return false
	}
	repo, err := ResolveRepo(qr.r.opts.Config, qr.q, wo.Text)
	if err != nil {
		return false
	}
	ok, rerr := repoReadable(repo.Path)
	if ok {
		return false
	}
	why := "not starting a run: " + repoAccessMessage(repo.Path, rerr) +
		" Runs that need this folder are left queued for another agent."
	if qr.declined == nil {
		qr.declined = map[string]time.Time{}
	}
	for id, at := range qr.declined {
		if time.Since(at) > 24*time.Hour {
			delete(qr.declined, id)
		}
	}
	if _, seen := qr.declined[wo.RunID]; !seen {
		qr.logf("left run %s queued: %s", wo.RunID, why)
	}
	qr.declined[wo.RunID] = time.Now()
	qr.declineWhy, qr.declineAt = why, time.Now()
	return true
}

// isDeclined reports whether this queue left runID unstarted for an
// unreadable repository.
func (qr *queueRunner) isDeclined(runID string) bool {
	_, ok := qr.declined[runID]
	return ok
}

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
// the folder at path. It goes in the log and on the queue's beat.
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
