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
	dead, err := s.SettleDeadRuns()
	return len(dead), err
}

// DeadRun is a run [Store.SettleDeadRuns] settled: its meta as settled, and
// when its log was last written — the last moment the runner that died is
// known to have been driving it.
type DeadRun struct {
	Meta
	LastActivity time.Time
}

// SettleDeadRuns is [Store.SettleDead], naming the runs it settled, so the
// runner can hand each one's claim back to Elk rather than leave Elk showing
// a run nobody drives until its lease lapses.
func (s *Store) SettleDeadRuns() ([]DeadRun, error) {
	return s.settleDead(os.Getpid(), pidAlive)
}

func (s *Store) settleDead(self int, alive func(pid int) bool) ([]DeadRun, error) {
	if s == nil {
		return nil, nil
	}
	active, err := s.Active()
	if err != nil {
		return nil, err
	}
	var dead []DeadRun
	for _, m := range active {
		if m.PID != 0 && m.PID != self && alive(m.PID) {
			continue
		}
		if m.PID == 0 {
			// A meta from before the pid field existed says nothing about
			// who drives it; guessing would settle a live run.
			continue
		}
		// Read before the meta is rewritten below: the newer of the two
		// files is the last thing the dead runner wrote about this run.
		last := m.StartedAt
		for _, path := range []string{s.LogPath(m.RunID), s.MetaPath(m.RunID)} {
			if fi, err := os.Stat(path); err == nil && fi.ModTime().After(last) {
				last = fi.ModTime()
			}
		}
		m.Status = StatusInterrupted
		// Its worktree, if it got that far, was never reaped by the runner
		// that died.
		m.ReapPending = m.Worktree != ""
		if m.EndedAt.IsZero() {
			m.EndedAt = time.Now()
		}
		if err := writeMetaFile(s.MetaPath(m.RunID), m); err != nil {
			return dead, err
		}
		dead = append(dead, DeadRun{Meta: m, LastActivity: last})
	}
	return dead, nil
}
