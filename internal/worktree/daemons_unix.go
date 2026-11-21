//go:build !windows

package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var selfPID = os.Getpid()

// processOwnerAndCommand reports whether pid belongs to the user Rein runs as,
// and its whole command line, from ps.
func processOwnerAndCommand(pid int) (mine bool, commandLine string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-ww", "-o", "uid=,args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false, "", err
	}
	fields := strings.SplitN(strings.TrimSpace(string(out)), " ", 2)
	uid, err := strconv.Atoi(fields[0])
	if err != nil {
		return false, "", fmt.Errorf("ps: unreadable owner for pid %d", pid)
	}
	if len(fields) == 2 {
		commandLine = strings.TrimSpace(fields[1])
	}
	return uid == os.Getuid(), commandLine, nil
}

// stopProcess asks pid to exit and waits up to grace for it to go, then kills
// it. A process that is already gone is a success.
func stopProcess(pid int, grace time.Duration) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	if waitGone(pid, grace) {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if !waitGone(pid, 5*time.Second) {
		return fmt.Errorf("still running after SIGKILL")
	}
	return nil
}

// waitGone polls with signal 0 until the kernel says pid no longer exists.
func waitGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
