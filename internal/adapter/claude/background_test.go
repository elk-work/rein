//go:build !windows

package claude

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// These tests are ark:rein#80, the Oct 14 pattern: the agent starts
// background work, ends its turn while it runs, and Rein must not take that
// turn's result as the run's. testdata/background.jsonl is a real recording
// (claude 2.1.296, 2026-10-14) of exactly that: a background `sleep; echo`,
// a first result while it runs, the task notification, and the second turn
// Claude Code starts by itself once stdin is still open.

// splitBackgroundFixture writes the recording's two halves to files: through
// the first result line, and everything after it.
func splitBackgroundFixture(t *testing.T) (first, rest string) {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("testdata", "background.jsonl"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	lines := bytes.SplitAfter(blob, []byte("\n"))
	cut := -1
	for i, l := range lines {
		if bytes.Contains(l, []byte(`"type":"result"`)) {
			cut = i + 1
			break
		}
	}
	if cut < 0 {
		t.Fatal("the fixture has no result line")
	}
	dir := t.TempDir()
	first = filepath.Join(dir, "first.jsonl")
	rest = filepath.Join(dir, "rest.jsonl")
	if err := os.WriteFile(first, bytes.Join(lines[:cut], nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rest, bytes.Join(lines[cut:], nil), 0o600); err != nil {
		t.Fatal(err)
	}
	return first, rest
}

// fixtureLines returns the recording's lines after the first result that
// match keep, joined back into a file.
func restMatching(t *testing.T, rest string, keep func(string) bool) string {
	t.Helper()
	blob, err := os.ReadFile(rest)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(blob), "\n") {
		if l != "" && keep(l) {
			out = append(out, l)
		}
	}
	path := filepath.Join(t.TempDir(), "partial.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startBackgroundStub(t *testing.T, script string, a *Adapter) adapter.Session {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a.Binary = path
	a.DisableWatchdog = true
	sess, err := a.Start(t.Context(), adapter.RunSpec{
		WorktreeDir:    t.TempDir(),
		Prompt:         "do the thing",
		PermissionMode: adapter.PermissionFull,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return sess
}

// TestSessionWaitsForBackgroundWork is the fix itself. The stub behaves as
// claude does: the second turn arrives only if stdin is still open when the
// background work finishes, and closing stdin ends the process — taking the
// background work with it. Before ark:rein#80 the run ended on FIRST.
func TestSessionWaitsForBackgroundWork(t *testing.T) {
	first, rest := splitBackgroundFixture(t)
	sess := startBackgroundStub(t, "cat "+first+"\n"+
		"( sleep 1; cat "+rest+" ) &\nbg=$!\n"+
		"cat > /dev/null\nkill $bg 2>/dev/null\nexit 0", &Adapter{})
	evs := drain(t, sess)

	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !strings.HasPrefix(res.Summary, "SECOND") {
		t.Errorf("summary = %q; the run ended on the turn that left background work running", res.Summary)
	}
	if res.Status != adapter.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", res.Status)
	}
	var terminals int
	var said bool
	for _, ev := range evs {
		if ev.Kind.Terminal() {
			terminals++
		}
		if strings.Contains(ev.Text, "background work still running") {
			said = true
		}
	}
	if terminals != 1 {
		t.Errorf("terminal events = %d, want 1", terminals)
	}
	if !said {
		t.Error("no progress event said the session was being kept open for background work")
	}
}

// TestSessionAbandonedBackgroundWorkFails: the agent's background work never
// finishes and the agent never answers. Past the wait it is told to finish in
// the foreground; when it does not, the run fails — so Rein submits stuck,
// never an empty "ready".
func TestSessionAbandonedBackgroundWorkFails(t *testing.T) {
	first, _ := splitBackgroundFixture(t)
	stdinLog := filepath.Join(t.TempDir(), "stdin")
	sess := startBackgroundStub(t, "cat "+first+"\ncat > "+stdinLog, &Adapter{
		BackgroundWait:   300 * time.Millisecond,
		BackgroundSettle: 300 * time.Millisecond,
		BackgroundBeat:   100 * time.Millisecond,
	})
	evs := drain(t, sess)

	res, err := sess.Wait()
	if err == nil || !strings.Contains(err.Error(), "background") {
		t.Fatalf("Wait err = %v, want a failure naming the background work", err)
	}
	if res.Status != adapter.StatusFailed {
		t.Errorf("status = %q, want failed", res.Status)
	}
	if last := evs[len(evs)-1]; last.Kind != adapter.EventError {
		t.Errorf("last event = %q, want error", last.Kind)
	}
	var beats int
	for _, ev := range evs {
		if strings.HasPrefix(ev.Text, "waiting for the agent's background work") {
			beats++
		}
	}
	if beats == 0 {
		t.Error("no progress while waiting; the run loop would read the wait as a stall")
	}
	if got, _ := os.ReadFile(stdinLog); !strings.Contains(string(got), "cannot wait any longer") {
		t.Errorf("the agent was never told to finish in the foreground; stdin got %q", got)
	}
}

// TestSessionNudgesWhenBackgroundWorkFinishesQuietly: the work finishes but no
// new turn starts. The agent is nudged once, and if it still says nothing the
// last result stands — the work finished, so nothing was lost.
func TestSessionNudgesWhenBackgroundWorkFinishesQuietly(t *testing.T) {
	first, rest := splitBackgroundFixture(t)
	finished := restMatching(t, rest, func(l string) bool {
		return strings.Contains(l, `"task_notification"`) || strings.Contains(l, `"background_tasks_changed"`)
	})
	stdinLog := filepath.Join(t.TempDir(), "stdin")
	sess := startBackgroundStub(t, "cat "+first+"\ncat "+finished+"\ncat > "+stdinLog, &Adapter{
		BackgroundSettle: 300 * time.Millisecond,
	})
	drain(t, sess)

	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !strings.HasPrefix(res.Summary, "FIRST") || res.Status != adapter.StatusSucceeded {
		t.Errorf("result = %q/%q, want the first turn's result, succeeded", res.Status, res.Summary)
	}
	if got, _ := os.ReadFile(stdinLog); !strings.Contains(string(got), "background work has finished") {
		t.Errorf("the agent was never nudged; stdin got %q", got)
	}
}

// TestDecoderTracksBackgroundWork pins the set against the recording: one
// task live at the first result, none at the end.
func TestDecoderTracksBackgroundWork(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join("testdata", "background.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	d := &decoder{}
	var atResults []int
	for _, l := range bytes.Split(blob, []byte("\n")) {
		_, res, _ := d.decode(l)
		if res != nil {
			atResults = append(atResults, len(d.background))
		}
	}
	if len(atResults) != 2 || atResults[0] != 1 || atResults[1] != 0 {
		t.Errorf("background tasks at each result = %v, want [1 0]", atResults)
	}
}
