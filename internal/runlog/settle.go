package runlog

import (
	"os"
	"time"
)

// SettleDead marks every run still `running` in its meta whose runner is gone
// as [StatusInterrupted] (ark:rein#95), and reports how many it settled.
//
// A run's meta is written `running` when it is claimed and set to its outcome
// only when the run unwinds. A runner that dies without unwinding (SIGKILL, an
// OOM kill, power loss) leaves `running` behind for ever, and a run that is
// forever active is never pruned, takes a slot of the keep budget and holds
// back the check adapter's tidy-up.
//
// Call it once at startup, before any run of this process exists: a meta
// carrying this process's own PID then belongs to an earlier life that the
// kernel handed the same number (a reboot, a container restart), and is dead
// like any other. A meta whose PID is alive in another process is left alone —
// a second `rein run` on the same home may own it.
func (s *Store) SettleDead() (int, error) {
	return s.settleDead(os.Getpid(), pidAlive)
}

func (s *Store) settleDead(self int, alive func(pid int) bool) (int, error) {
	active, err := s.Active()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range active {
		if m.PID != 0 && m.PID != self && alive(m.PID) {
			continue
		}
		if m.PID == 0 {
			// A meta from before the pid field existed says nothing about
			// who drives it; guessing would settle a live run.
			continue
		}
		m.Status = StatusInterrupted
		// Its worktree, if it got that far, was never reaped by the runner
		// that died.
		m.ReapPending = m.Worktree != ""
		if m.EndedAt.IsZero() {
			m.EndedAt = time.Now()
		}
		if err := writeMetaFile(s.MetaPath(m.RunID), m); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
