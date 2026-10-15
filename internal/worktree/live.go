package worktree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrInUse is returned by [Manager.Reap] when a process is still working in
// the worktree. The worktree is left exactly where it is (ark:rein#80).
var ErrInUse = errors.New("worktree: still in use")

// inside reports whether path is dir or below it, after resolving symlinks on
// both — on macOS a temp directory is reachable as /tmp and /private/tmp.
func inside(path, dir string) bool {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		dir = d
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkLive refuses a reap while any process other than Rein itself has its
// working directory inside dir. A finished run's session is gone by now, so
// whatever is left there is work that outlived it — a detached background
// shell, a helper that was not the session's child — and removing the
// directory under it is how Oct 14 lost five lanes of work.
func (m *Manager) checkLive(dir string) error {
	probe := m.LiveProcesses
	if probe == nil {
		probe = processesIn
	}
	procs, err := probe(dir)
	if err != nil {
		// Fails open: a probe that cannot run must not wedge every reap.
		m.log("worktree: could not check %s for live processes: %v", dir, err)
		return nil
	}
	self := fmt.Sprintf("%d ", os.Getpid())
	var others []string
	for _, p := range procs {
		if !strings.HasPrefix(p, self) {
			others = append(others, p)
		}
	}
	if len(others) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %d process(es) still working in %s, so it was left in place: %s",
		ErrInUse, len(others), dir, strings.Join(others, "; "))
}
