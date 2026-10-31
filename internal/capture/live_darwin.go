//go:build darwin

package capture

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// The window list is a short Swift program rather than cgo: release.yml
// builds with cgo off, and CoreGraphics is the call that answers which pid
// owns a window. Swift is the same program scripts in elk-work/scout already
// trust for this (mac-screen.swift). It prints numbers only — id, pid, layer,
// width, height — and never a title or an owner name.
const windowListSwift = `
import CoreGraphics
import Foundation
let options: CGWindowListOption = [.optionOnScreenOnly, .excludeDesktopElements]
let raw = CGWindowListCopyWindowInfo(options, kCGNullWindowID) as? [[String: Any]] ?? []
for info in raw {
    guard let id = (info[kCGWindowNumber as String] as? NSNumber)?.intValue,
          let pid = (info[kCGWindowOwnerPID as String] as? NSNumber)?.intValue,
          let bounds = info[kCGWindowBounds as String] as? NSDictionary,
          let rect = CGRect(dictionaryRepresentation: bounds as CFDictionary) else { continue }
    let layer = (info[kCGWindowLayer as String] as? NSNumber)?.intValue ?? -1
    print("\(id) \(pid) \(layer) \(Int(rect.width.rounded())) \(Int(rect.height.rounded()))")
}
`

func systemTree() (Tree, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-ax", "-o", "pid=,ppid=,ucomm=").Output()
	if err != nil {
		return Tree{}, fmt.Errorf("could not read the process table: %w", err)
	}
	return ParseProcesses(string(out))
}

func systemWindows() ([]Window, error) {
	swift, err := exec.LookPath("swift")
	if err != nil {
		if _, statErr := os.Stat("/usr/bin/swift"); statErr == nil {
			swift = "/usr/bin/swift"
		} else {
			return nil, fmt.Errorf("cannot list windows without swift, so there is nothing to photograph. Refusing rather than capturing the display")
		}
	}
	dir, err := os.MkdirTemp("", "rein-windows-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "windows.swift")
	if err := os.WriteFile(script, []byte(windowListSwift), 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, swift, script)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("could not list windows, so there is nothing to photograph: %w", err)
	}
	return ParseWindows(string(out))
}

// shoot is /usr/sbin/screencapture by absolute path, so the PATH shim cannot
// call itself. -x keeps the shutter sound off. The id has already passed
// [IDShot] or [WindowShot].
func shoot(windowID int, out, format string) error {
	args := []string{"-x", "-o", "-l", strconv.Itoa(windowID)}
	if format != "" {
		args = append(args, "-t", format)
	}
	args = append(args, out)
	cmd := exec.Command("/usr/sbin/screencapture", args...)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("could not photograph window %d: %v: %s", windowID, err, msg)
	}
	return nil
}

// LiveWindow photographs pid's main window into out.
func LiveWindow(pid int, out string) error {
	out, err := absOut(out)
	if err != nil {
		return err
	}
	tree, err := systemTree()
	if err != nil {
		return err
	}
	self := os.Getpid()
	if !tree.Launched(pid, self) {
		return fmt.Errorf("pid %d is not a process this run launched", pid)
	}
	windows, err := systemWindows()
	if err != nil {
		return err
	}
	id, err := WindowShot(self, pid, out, tree, windows)
	if err != nil {
		return err
	}
	return shoot(id, out, "")
}

// LiveScreencapture is the PATH shim's entry: the same check, for a window
// id the caller already chose.
func LiveScreencapture(args []string) error {
	id, out, format, err := ParseScreencaptureArgs(args)
	if err != nil {
		return err
	}
	out, err = absOut(out)
	if err != nil {
		return err
	}
	tree, err := systemTree()
	if err != nil {
		return err
	}
	windows, err := systemWindows()
	if err != nil {
		return err
	}
	shot, err := IDShot(os.Getpid(), id, out, tree, windows)
	if err != nil {
		return err
	}
	return shoot(shot, out, format)
}

func absOut(out string) (string, error) {
	if out == "" {
		return "", fmt.Errorf("no output path")
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	return abs, nil
}
