//go:build windows

package runlog

import (
	"errors"

	"golang.org/x/sys/windows"
)

// pidAlive opens the process for the one right that needs no privilege and
// asks whether it has exited. Access denied means it exists.
func pidAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == 259 // STILL_ACTIVE
}
