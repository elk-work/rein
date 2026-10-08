//go:build windows

package tail

import (
	"os"

	"golang.org/x/sys/windows"
)

// termSize reads the console's window size. The screen buffer is taller than
// the window and its width is the one that matters for truncation, so this
// takes the window rectangle rather than the buffer's dwSize.
func termSize(f *os.File) (width, height int, ok bool) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(f.Fd()), &info); err != nil {
		return 0, 0, false
	}
	w := int(info.Window.Right-info.Window.Left) + 1
	h := int(info.Window.Bottom-info.Window.Top) + 1
	if w <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// enableVirtualTerminal turns on ANSI processing for a Windows console.
//
// Windows Terminal and PowerShell 7 arrive with it on; conhost.exe on an older
// Windows 10 does not, and without it the cursor-up and erase sequences are
// printed as literal text. Best effort by design: a console that refuses gets
// the same treatment as a pipe, which is plain scrolling output.
func enableVirtualTerminal(f *os.File) {
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return
	}
	_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}
