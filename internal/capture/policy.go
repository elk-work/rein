// Package capture is how a run photographs something it launched itself.
//
// On 2026-08-29 a run under permission mode full took screenshots of the
// developer's desktop while he was using it, then deleted them. The standing
// prompt has forbidden that ever since: the person's screen, camera and
// microphone are not a run's to observe. The same rule also blocked the one
// capture that does not observe the person — a window of a process the run
// itself started, or a page it opened in the headless Chrome Rein gives it.
//
// This package is the check, because a prompt is not a sandbox (docs/run-loop.md).
// A window may be photographed only when its owner is a descendant of the rein
// that started this run. The display, a rectangle of it, and every other
// window are refused. A page is chrome --screenshot through a fresh profile,
// which writes a file and never opens a window.
package capture

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/elk-work/rein/internal/browser"
)

// EnvBin is the directory of the screencapture gate on a run's PATH, set only
// on macOS. The directory is removed when the run ends.
const EnvBin = "REIN_CAPTURE_BIN"

// How far up a process tree a walk will go before it decides the tree is
// nonsense rather than walk it forever. A real launch chain is a handful of
// processes; a cycle is a bug in the table.
const maxWalk = 64

// Window is one on-screen window, minus anything that could carry content.
// Titles and owner names are never read.
type Window struct {
	ID, PID, Layer, Width, Height int
}

// Tree is a process table: each pid's parent and its short name (ps ucomm,
// or the executable base name). A missing pid is unknown, and unknown is
// not launched.
type Tree struct {
	PPID map[int]int
	Name map[int]string
}

// ParseProcesses reads `ps -o pid=,ppid=,ucomm=` output. A line that is not
// three fields fails the whole table: a partial table could call a stranger
// a descendant.
func ParseProcesses(text string) (Tree, error) {
	t := Tree{PPID: map[int]int{}, Name: map[int]string{}}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return Tree{}, fmt.Errorf("process table line %q is not pid, parent, name", line)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return Tree{}, fmt.Errorf("process table pid %q", fields[0])
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return Tree{}, fmt.Errorf("process table parent %q", fields[1])
		}
		if pid <= 0 {
			return Tree{}, fmt.Errorf("process table pid %d", pid)
		}
		t.PPID[pid] = ppid
		t.Name[pid] = strings.Join(fields[2:], " ")
	}
	if len(t.PPID) == 0 {
		return Tree{}, errors.New("process table was empty")
	}
	return t, nil
}

// ParseWindows reads lines of "id pid layer width height". Anything else fails
// the list, and the caller refuses the capture rather than guess.
func ParseWindows(text string) ([]Window, error) {
	var out []Window
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 5 {
			return nil, errors.New("window list was not in the expected form")
		}
		nums := make([]int, 5)
		for i, f := range fields {
			n, err := strconv.Atoi(f)
			if err != nil {
				return nil, errors.New("window list was not in the expected form")
			}
			nums[i] = n
		}
		out = append(out, Window{ID: nums[0], PID: nums[1], Layer: nums[2], Width: nums[3], Height: nums[4]})
	}
	return out, nil
}

// Root is the process whose descendants this capture may photograph: the
// oldest ancestor of self named rein (the runner that started the run), or
// self's parent when a person invoked the command by hand and no rein is
// above it. The bool is false when self is not in the table.
func (t Tree) Root(self int) (int, bool) {
	parent, ok := t.PPID[self]
	if !ok || parent <= 0 {
		return 0, false
	}
	root := parent
	seen := map[int]bool{}
	cur := parent
	for steps := 0; steps < maxWalk; steps++ {
		if cur <= 0 || seen[cur] {
			break
		}
		seen[cur] = true
		if isRein(t.Name[cur]) {
			root = cur
		}
		next, ok := t.PPID[cur]
		if !ok || next <= 0 || next == cur {
			break
		}
		cur = next
	}
	return root, true
}

func isRein(name string) bool {
	base := filepath.Base(name)
	return base == "rein" || base == "rein.exe"
}

// Launched reports whether pid is a process this run started: a strict
// descendant of [Tree.Root] of self. The runner itself, the capture process,
// and anything reparented to launchd (open -a does this) are not.
func (t Tree) Launched(pid, self int) bool {
	root, ok := t.Root(self)
	if !ok || pid <= 0 || pid == root || pid == self {
		return false
	}
	seen := map[int]bool{}
	cur := pid
	for steps := 0; steps < maxWalk; steps++ {
		if cur == root {
			return true
		}
		if cur <= 0 || seen[cur] {
			return false
		}
		seen[cur] = true
		next, ok := t.PPID[cur]
		if !ok || next == cur {
			return false
		}
		cur = next
	}
	return false
}

// ChooseWindow is pid's main window: the largest ordinary (layer 0) window
// big enough not to be the title-bar strip AppKit keeps per app. A window of
// any other pid is never a candidate, however large it is.
func ChooseWindow(windows []Window, pid int) (Window, bool) {
	var best Window
	found := false
	for _, w := range windows {
		if w.PID != pid || w.Layer != 0 || w.Width < 80 || w.Height < 80 || w.ID <= 0 {
			continue
		}
		if !found || w.Width*w.Height > best.Width*best.Height {
			best = w
			found = true
		}
	}
	return best, found
}

// ChooseByID finds one window. The bool is false when it is not in the list.
func ChooseByID(windows []Window, id int) (Window, bool) {
	for _, w := range windows {
		if w.ID == id {
			return w, true
		}
	}
	return Window{}, false
}

// WindowShot is the window id to photograph for pid, or an error that names
// no other process. out must already be an absolute path.
func WindowShot(self, pid int, out string, tree Tree, windows []Window) (int, error) {
	if out == "" {
		return 0, errors.New("no output path")
	}
	if !tree.Launched(pid, self) {
		return 0, fmt.Errorf("pid %d is not a process this run launched", pid)
	}
	w, ok := ChooseWindow(windows, pid)
	if !ok {
		if len(windows) == 0 {
			return 0, errNoWindows
		}
		return 0, fmt.Errorf("pid %d has no on-screen window of its own", pid)
	}
	return w.ID, nil
}

// IDShot is the same check for a window id a caller already holds, which is
// what the screencapture gate does with -l.
func IDShot(self, id int, out string, tree Tree, windows []Window) (int, error) {
	if out == "" {
		return 0, errors.New("no output path")
	}
	if id <= 0 {
		return 0, errors.New("screencapture must name one window with -l; the display and other windows are off limits")
	}
	if len(windows) == 0 {
		return 0, errNoWindows
	}
	w, ok := ChooseByID(windows, id)
	if !ok {
		return 0, fmt.Errorf("window %d is not on screen", id)
	}
	if !tree.Launched(w.PID, self) {
		return 0, fmt.Errorf("window %d belongs to pid %d, which this run did not launch", id, w.PID)
	}
	return w.ID, nil
}

// errNoWindows is what an empty list means. On macOS that is Screen Recording
// not being granted, and the alternative — photographing the display — is the
// thing this package exists to refuse.
var errNoWindows = errors.New("no windows are visible to this process, so there is nothing to photograph. Refusing rather than capturing the display")

// ParseScreencaptureArgs accepts only a window capture: -l <id>, an output
// path, and the quiet flags (-x, -o, -t png|jpg|jpeg|tif|tiff|pdf). A bare
// path, a rectangle (-R), an interactive pick (-w, -i) and the clipboard (-c)
// are the display, or a window the caller did not name, and are refused.
func ParseScreencaptureArgs(args []string) (id int, out, format string, err error) {
	var haveID bool
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			continue
		case a == "-x" || a == "-o":
			continue
		case a == "-l":
			i++
			if i >= len(args) {
				return 0, "", "", errors.New("screencapture -l needs a window id")
			}
			id, haveID, err = oneWindowID(haveID, args[i])
			if err != nil {
				return 0, "", "", err
			}
		case strings.HasPrefix(a, "-l") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			id, haveID, err = oneWindowID(haveID, a[2:])
			if err != nil {
				return 0, "", "", err
			}
		case a == "-t":
			i++
			if i >= len(args) {
				return 0, "", "", errors.New("screencapture -t needs a format")
			}
			format, err = oneFormat(format, args[i])
			if err != nil {
				return 0, "", "", err
			}
		case strings.HasPrefix(a, "-"):
			return 0, "", "", fmt.Errorf("screencapture flag %q is off limits", a)
		default:
			if out != "" {
				return 0, "", "", errors.New("screencapture takes one output file")
			}
			out = a
		}
	}
	if !haveID {
		return 0, "", "", errors.New("screencapture must name one window with -l; the display and other windows are off limits")
	}
	if out == "" {
		return 0, "", "", errors.New("screencapture needs an output file")
	}
	return id, out, format, nil
}

func oneWindowID(have bool, s string) (int, bool, error) {
	if have {
		return 0, false, errors.New("screencapture names more than one window")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false, fmt.Errorf("screencapture window id %q", s)
	}
	return n, true, nil
}

func oneFormat(have, s string) (string, error) {
	if have != "" {
		return "", errors.New("screencapture names more than one format")
	}
	switch strings.ToLower(s) {
	case "png", "jpg", "jpeg", "tif", "tiff", "pdf":
		return strings.ToLower(s), nil
	default:
		return "", fmt.Errorf("screencapture format %q is off limits", s)
	}
}

// PlanSite is the chrome invocation that screenshots one http(s) page into
// out, through profile, which the caller created and which must be empty of
// the person's cookies. The flags are [browser.Flags]: headless, mock
// keychain, updater asleep. A file URL is refused — it would be a way to read
// a file outside the worktree by photographing it.
func PlanSite(chrome, rawURL, out, profile string) ([]string, error) {
	if chrome == "" {
		return nil, errors.New("no chrome: rein browser install gives a run one")
	}
	if out == "" || profile == "" {
		return nil, errors.New("a page shot needs an output file and a fresh profile")
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("a page shot takes an http(s) url, not %q", rawURL)
	}
	args := make([]string, 0, len(browser.Flags)+3)
	args = append(args, browser.Flags...)
	args = append(args, "--user-data-dir="+profile, "--screenshot="+out, rawURL)
	return args, nil
}
