package runlog

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestSettleDeadMarksOnlyRunsWhoseRunnerIsGone(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id string, pid int, st Status) {
		t.Helper()
		if err := writeMetaFile(s.MetaPath(id), Meta{RunID: id, PID: pid, Status: st, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	mk("dead", 4001, StatusRunning)
	mk("alive-elsewhere", 4002, StatusRunning)
	mk("reused-pid", 4003, StatusRunning) // this process's number, from before a reboot
	mk("no-pid", 0, StatusRunning)
	mk("done", 4001, StatusSubmitted)

	n, err := s.settleDead(4003, func(pid int) bool { return pid == 4002 || pid == 4003 })
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("settled %d, want 2", n)
	}
	want := map[string]Status{
		"dead": StatusInterrupted, "reused-pid": StatusInterrupted,
		"alive-elsewhere": StatusRunning, "no-pid": StatusRunning, "done": StatusSubmitted,
	}
	for id, st := range want {
		m, err := s.ReadMeta(id)
		if err != nil {
			t.Fatal(err)
		}
		if m.Status != st {
			t.Errorf("%s = %q, want %q", id, m.Status, st)
		}
	}
	if m, _ := s.ReadMeta("dead"); m.EndedAt.IsZero() {
		t.Error("a settled run has no ended_at")
	}
	active, _ := s.Active()
	if len(active) != 2 {
		t.Errorf("active = %d, want the 2 left alone", len(active))
	}
}

func TestPidAliveSeesARealProcessComeAndGo(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Fatal("this process reads as dead")
	}
	cmd := exec.Command(os.Args[0], "-test.run=NONE")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if pidAlive(cmd.Process.Pid) {
		t.Skip("the kernel already reused the pid")
	}
}
