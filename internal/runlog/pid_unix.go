//go:build !windows

package runlog

import (
	"errors"
	"os"
	"syscall"
)

// pidAlive probes with signal 0: no signal is sent, but the kernel says
// whether the process exists. EPERM means it exists and is someone else's.
func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
