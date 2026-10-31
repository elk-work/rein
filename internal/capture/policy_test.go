package capture

import (
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/browser"
)

// A run's tree, the shape giveCapture's check is about.
//
//	1 launchd
//	10 rein (the runner) <- 20 agent <- 30 shell <- 40 the app the run started
//	                                    \- 50 rein capture (self)
//	99 the person's browser, parented on launchd
func runTree() Tree {
	// The person's browser name has a space, which the parser must keep, and
	// its parent is launchd — it is not in the run's tree.
	t, err := ParseProcesses("10 1 rein\n20 10 agent\n30 20 zsh\n40 30 MyApp\n50 30 rein\n99 1 Google Chrome\n")
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseProcessesKeepsANameWithASpace(t *testing.T) {
	tree, err := ParseProcesses("  99  1  Google Chrome\n10 1 rein\n")
	if err != nil {
		t.Fatal(err)
	}
	if tree.Name[99] != "Google Chrome" || tree.PPID[99] != 1 || tree.PPID[10] != 1 {
		t.Fatalf("tree = %+v", tree)
	}
}

func TestParseProcessesRejectsAPartialLine(t *testing.T) {
	if _, err := ParseProcesses("10 1 rein\nnope\n"); err == nil {
		t.Fatal("a line that is not pid, parent, name was accepted")
	}
}

func TestLaunchedIsOnlyDescendantsOfTheRunner(t *testing.T) {
	tree := runTree()
	const self = 50
	for _, pid := range []int{20, 30, 40} {
		if !tree.Launched(pid, self) {
			t.Errorf("pid %d is a process the run started", pid)
		}
	}
	for _, pid := range []int{0, 1, 10, 50, 99, 12345} {
		if tree.Launched(pid, self) {
			t.Errorf("pid %d must not be photographable", pid)
		}
	}
}

func TestHandInvocationAllowsOnlyThatShellsChildren(t *testing.T) {
	// No rein above self: a person ran the command from a shell. The shell's
	// other children are theirs to name; the terminal that owns the shell is not.
	tree, err := ParseProcesses("8 1 Terminal\n30 8 zsh\n40 30 MyApp\n50 30 rein\n99 1 Safari\n")
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Launched(40, 50) {
		t.Error("an app the invoking shell started was refused")
	}
	if tree.Launched(8, 50) || tree.Launched(99, 50) || tree.Launched(30, 50) {
		t.Error("the terminal, the shell itself, or another app was allowed")
	}
}

func TestAReparentedAppIsNotLaunched(t *testing.T) {
	// open -a reparents the app onto launchd. It has left the run's tree.
	tree, err := ParseProcesses("10 1 rein\n20 10 agent\n40 1 MyApp\n50 20 rein\n")
	if err != nil {
		t.Fatal(err)
	}
	if tree.Launched(40, 50) {
		t.Error("an app reparented to launchd was treated as launched")
	}
}

func TestACycleIsNotLaunched(t *testing.T) {
	tree := Tree{PPID: map[int]int{2: 3, 3: 2}, Name: map[int]string{}}
	if tree.Launched(3, 2) {
		t.Error("a cycle was treated as a launch chain")
	}
}

func TestChooseWindowIgnoresEveryOtherProcess(t *testing.T) {
	windows := []Window{
		{ID: 1, PID: 99, Layer: 0, Width: 2000, Height: 2000}, // the person's, larger
		{ID: 2, PID: 40, Layer: 0, Width: 40, Height: 20},     // title-bar strip
		{ID: 3, PID: 40, Layer: 3, Width: 900, Height: 700},   // overlay
		{ID: 4, PID: 40, Layer: 0, Width: 800, Height: 600},
		{ID: 5, PID: 40, Layer: 0, Width: 400, Height: 300},
	}
	got, ok := ChooseWindow(windows, 40)
	if !ok || got.ID != 4 {
		t.Fatalf("chose %+v ok=%v, want window 4", got, ok)
	}
	if _, ok := ChooseWindow(windows, 41); ok {
		t.Fatal("a pid with no window produced one")
	}
	strip := []Window{{ID: 2, PID: 40, Layer: 0, Width: 40, Height: 20}}
	if _, ok := ChooseWindow(strip, 40); ok {
		t.Fatal("a title-bar strip was treated as the app's window")
	}
}

func TestWindowShotRefusesWhatTheRunDidNotLaunch(t *testing.T) {
	tree := runTree()
	windows := []Window{{ID: 7, PID: 99, Layer: 0, Width: 1200, Height: 800}}
	if _, err := WindowShot(50, 99, "/tmp/shot.png", tree, windows); err == nil || !strings.Contains(err.Error(), "not a process this run launched") {
		t.Fatalf("err = %v", err)
	}
	if _, err := WindowShot(50, 40, "/tmp/shot.png", tree, nil); err == nil || !strings.Contains(err.Error(), "Refusing rather than capturing the display") {
		t.Fatalf("empty list err = %v", err)
	}
	id, err := WindowShot(50, 40, "/tmp/shot.png", tree, []Window{
		{ID: 7, PID: 99, Layer: 0, Width: 2000, Height: 2000},
		{ID: 8, PID: 40, Layer: 0, Width: 800, Height: 600},
	})
	if err != nil || id != 8 {
		t.Fatalf("id=%d err=%v, want the app's own window", id, err)
	}
}

func TestIDShotRefusesAnotherAppsWindow(t *testing.T) {
	tree := runTree()
	windows := []Window{
		{ID: 7, PID: 99, Layer: 0, Width: 1200, Height: 800},
		{ID: 8, PID: 40, Layer: 0, Width: 800, Height: 600},
	}
	if _, err := IDShot(50, 7, "/tmp/shot.png", tree, windows); err == nil || !strings.Contains(err.Error(), "did not launch") {
		t.Fatalf("err = %v", err)
	}
	id, err := IDShot(50, 8, "/tmp/shot.png", tree, windows)
	if err != nil || id != 8 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if _, err := IDShot(50, 8, "/tmp/shot.png", tree, nil); err == nil || !strings.Contains(err.Error(), "Refusing") {
		t.Fatalf("empty list err = %v", err)
	}
}

func TestScreencaptureArgsAllowOnlyANamedWindow(t *testing.T) {
	id, out, format, err := ParseScreencaptureArgs([]string{"-x", "-o", "-l", "42", "-t", "png", "shot.png"})
	if err != nil || id != 42 || out != "shot.png" || format != "png" {
		t.Fatalf("got %d %q %q err=%v", id, out, format, err)
	}
	id, out, format, err = ParseScreencaptureArgs([]string{"-l99", "out.jpg"})
	if err != nil || id != 99 || out != "out.jpg" || format != "" {
		t.Fatalf("attached -l: %d %q %q err=%v", id, out, format, err)
	}
	for _, args := range [][]string{
		{"shot.png"},
		{"-R", "0,0,100,100", "shot.png"},
		{"-w", "shot.png"},
		{"-i", "shot.png"},
		{"-c"},
		{"-l", "42", "-c", "shot.png"},
		{"-l", "42"},
		{"-l", "42", "a.png", "b.png"},
		{"-l", "42", "-l", "43", "a.png"},
		{"-x"},
		nil,
	} {
		if _, _, _, err := ParseScreencaptureArgs(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

func TestPlanSiteIsHeadlessFreshProfileAndHttpOnly(t *testing.T) {
	args, err := PlanSite("/opt/chrome", "https://example.com/a", "/tmp/shot.png", "/tmp/profile")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	for _, want := range append(browser.Flags, "--user-data-dir=/tmp/profile", "--screenshot=/tmp/shot.png", "https://example.com/a") {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "javascript:alert(1)", "example.com", "", "chrome://settings"} {
		if _, err := PlanSite("/opt/chrome", raw, "/tmp/shot.png", "/tmp/profile"); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if _, err := PlanSite("", "https://example.com", "/tmp/shot.png", "/tmp/profile"); err == nil {
		t.Fatal("an empty chrome path was accepted")
	}
}

func TestParseWindowsRejectsATitle(t *testing.T) {
	if _, err := ParseWindows("1 2 0 800 600 Mail\n"); err == nil {
		t.Fatal("a window line carrying a title was accepted")
	}
	got, err := ParseWindows("1 2 0 800 600\n\n3 4 0 100 100\n")
	if err != nil || len(got) != 2 || got[1].ID != 3 {
		t.Fatalf("got %+v err=%v", got, err)
	}
}
