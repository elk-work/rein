//go:build !darwin

package capture

import (
	"fmt"
	"runtime"
)

// LiveWindow and LiveScreencapture exist so the command builds everywhere.
// Photographing a window is a macOS screencapture; other platforms refuse
// before they look at a process table.
func LiveWindow(pid int, out string) error {
	return macOSOnly()
}

func LiveScreencapture(args []string) error {
	// A full-screen invocation is refused for what it is, on every platform,
	// before the macOS answer. The parse does not look at the machine.
	if _, _, _, err := ParseScreencaptureArgs(args); err != nil {
		return err
	}
	return macOSOnly()
}

func macOSOnly() error {
	return fmt.Errorf("rein capture photographs a window on macOS. This machine is %s", runtime.GOOS)
}
