//go:build windows

package worktree

import (
	"errors"
	"os"
	"time"
)

var selfPID = os.Getpid()

// Windows has no live-process probe (live_windows.go), so nothing is ever
// offered to these.
func processOwnerAndCommand(int) (bool, string, error) {
	return false, "", errors.New("not implemented on Windows")
}

func stopProcess(int, time.Duration) error { return errors.New("not implemented on Windows") }
