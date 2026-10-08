//go:build darwin || linux

package tail

import (
	"os"

	"golang.org/x/sys/unix"
)

// termSize asks the tty how wide it is. TIOCGWINSZ is the only portable answer:
// $COLUMNS is a shell variable and is not exported to a child process by any
// shell Rein's users run.
func termSize(f *os.File) (width, height int, ok bool) {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws == nil || ws.Col == 0 {
		return 0, 0, false
	}
	return int(ws.Col), int(ws.Row), true
}

// enableVirtualTerminal is a no-op outside Windows: every terminal Rein meets
// here already understands the two escapes screen uses.
func enableVirtualTerminal(*os.File) {}
