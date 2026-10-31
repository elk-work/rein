package check

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// What a check leaves behind outside its worktree, and when it goes
// (ark:rein#91). The run loop reaps the worktree itself; this is the rest.
//
//   - Its record directory, `<Rein home>/checks/<run>/`: the record or records
//     and the whole of act.log. Kept to the newest log_keep_runs, like the run
//     logs, because a record's copy in Elk is the one anybody reads and the
//     file is for looking into a run recent enough to still be asking about.
//   - act's scratch directory for a host-executor job,
//     `<act cache>/<16 hex>/{act,hostexecutor,tmp}`. act names it with eight
//     random bytes and removes it itself when the job ends, which Rein's
//     `--rm` makes true of a failed job too. What is still there was left by
//     an act that was killed, so it is removed once nothing has touched it
//     for [ActWorkdirMaxAge].
//   - act's action clones (`<act cache>/<owner>-<repo>@<ref>`) and its tool
//     cache (`<act cache>/tool_cache`) are left alone. They are a cache in the
//     real sense: bounded by the actions and tool versions the workflows
//     use, refreshed in place by act, and expensive to lose — the Linux lab's
//     first check spent 88 of its 123 seconds downloading node. On a desktop
//     the same directory is also a person's own act cache.

// ActWorkdirMaxAge is how long a host-executor directory act left behind is
// kept after anything last touched it. A day is far longer than any check
// runs: one is bounded by its timeout, an hour unless the queue says.
const ActWorkdirMaxAge = 24 * time.Hour

// actWorkdirRE is the name act gives a host-executor job's directory:
// hex.EncodeToString of eight random bytes (pkg/runner/run_context.go,
// startHostEnvironment, act v0.2.91).
var actWorkdirRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ReapRun implements [adapter.RunReaper]: it removes the oldest check records
// past policy.KeepRuns, and the host-executor directories act left behind
// more than [ActWorkdirMaxAge] ago. Neither ever touches a live run's: a live
// run's record is counted against the budget and kept, and no act directory
// is old enough while a run that could have made it is still going.
func (a *Adapter) ReapRun(spec adapter.RunSpec, p adapter.ReapPolicy) ([]string, error) {
	var lines []string
	var errs []error

	if root, err := a.recordRoot(); err != nil {
		errs = append(errs, fmt.Errorf("locating the check records: %w", err))
	} else if n, err := pruneRecords(root, p.KeepRuns, p.Live); err != nil {
		errs = append(errs, fmt.Errorf("pruning check records in %s: %w", root, err))
	} else if n > 0 {
		lines = append(lines, fmt.Sprintf("removed the records of %d old check run(s) from %s, keeping %d", n, root, p.KeepRuns))
	}

	root := a.CacheRoot
	if root == "" {
		root = actCacheRoot(a.env(spec))
	}
	cutoff := p.Now.Add(-ActWorkdirMaxAge)
	for _, started := range p.Live {
		// A live run that started earlier could have made a directory nothing
		// has touched since, so nothing that recent is old enough. A start
		// nobody recorded is the zero time, which keeps everything.
		if started.Before(cutoff) {
			cutoff = started
		}
	}
	if n, err := pruneActWorkdirs(root, cutoff); err != nil {
		errs = append(errs, fmt.Errorf("pruning act's directories in %s: %w", root, err))
	} else if n > 0 {
		lines = append(lines, fmt.Sprintf("removed %d host-executor directory(ies) act left in %s, untouched for over %s", n, root, ActWorkdirMaxAge))
	}
	return lines, errors.Join(errs...)
}

// pruneRecords keeps the newest keep record directories under root and
// removes the rest, the same shape as [runlog.Store.Prune]: newest by when the
// directory last changed, live runs counted against the budget and never
// removed. keep <= 0 keeps everything; a root that does not exist yet has
// nothing in it.
func pruneRecords(root string, keep int, live map[string]time.Time) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	liveDirs := map[string]bool{}
	for id := range live {
		liveDirs[safeName(id)] = true
	}
	type dir struct {
		name string
		mod  time.Time
	}
	var candidates []dir
	held := 0
	for _, e := range entries {
		// ReadDir does not follow links, so a symlink is never a directory
		// here and is never removed.
		if !e.IsDir() {
			continue
		}
		if liveDirs[e.Name()] {
			held++
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // gone since the listing
		}
		candidates = append(candidates, dir{e.Name(), info.ModTime()})
	}
	budget := keep - held
	if budget < 0 {
		budget = 0
	}
	if len(candidates) <= budget {
		return 0, nil
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].mod.After(candidates[j].mod) })
	var removed int
	var errs []error
	for _, d := range candidates[budget:] {
		if err := os.RemoveAll(filepath.Join(root, d.name)); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// pruneActWorkdirs removes the host-executor directories act left under root
// that nothing has touched since before cutoff. Only a directory act would
// have made is a candidate: named sixteen hex digits, a real directory and
// not a link, with a hostexecutor directory inside. Its age is the newest of
// its own modification time and its children's, because act writes into
// act/, hostexecutor/ and tmp/ while the job runs. root must be absolute and
// named act, so a misresolved root cannot point this at anything else.
func pruneActWorkdirs(root string, cutoff time.Time) (int, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Base(root) != "act" {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var removed int
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !actWorkdirRE.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if fi, err := os.Lstat(filepath.Join(dir, "hostexecutor")); err != nil || !fi.IsDir() {
			continue
		}
		last, ok := lastTouched(dir)
		if !ok || !last.Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// lastTouched is the newest modification time of dir and its immediate
// children. ok is false when dir cannot be read, which keeps it.
func lastTouched(dir string) (time.Time, bool) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return time.Time{}, false
	}
	last := fi.ModTime()
	children, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false
	}
	for _, c := range children {
		info, err := c.Info()
		if err != nil {
			return time.Time{}, false
		}
		if info.ModTime().After(last) {
			last = info.ModTime()
		}
	}
	return last, true
}

// actCacheRoot is where act keeps its cache, resolved from env exactly as act
// resolves it (cmd/dir.go and RunContext.ActionCacheDir, act v0.2.91):
// XDG_CACHE_HOME when it is set and not empty, else the home directory's
// .cache, then act. env is the environment Rein hands act, which carries
// HOME, USERPROFILE and XDG_CACHE_HOME through as system variables
// (internal/secretenv). "" when neither is there.
func actCacheRoot(env []string) string {
	get := func(name string) string {
		v := ""
		for _, kv := range env {
			k, val, ok := strings.Cut(kv, "=")
			if ok && (k == name || runtime.GOOS == "windows" && strings.EqualFold(k, name)) {
				v = val
			}
		}
		return v
	}
	if v := get("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "act")
	}
	home := get("HOME")
	if runtime.GOOS == "windows" {
		home = get("USERPROFILE")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".cache", "act")
}
