package tail_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/tail"
)

// syncBuffer is a bytes.Buffer a test goroutine may read while Run writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls the output until every substring has appeared, and fails with
// what was actually written if it does not.
func waitFor(t *testing.T, out *syncBuffer, stop context.CancelFunc, want ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := out.String()
		missing := ""
		for _, w := range want {
			if !strings.Contains(got, w) {
				missing = w
				break
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("never saw %q. Output:\n%q", missing, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newStore(t *testing.T) *runlog.Store {
	t.Helper()
	s, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A finished run replays from its log and exits — which is how a person reads
// back what happened in a worktree that no longer exists.
func TestFinishedRunReplaysAndExits(t *testing.T) {
	s := newStore(t)
	w := s.Create(runlog.Meta{RunID: "run-1", Queue: "mac-claude", Direction: "Fix the sheet"})
	w.Runner(runlog.KindClaimed, "claimed on queue mac-claude")
	w.Event(adapter.Event{Kind: adapter.EventText, Text: "reading the test"})
	w.Event(adapter.Event{Kind: adapter.EventToolUse, Tool: &adapter.ToolUse{
		Name: "Bash", Input: json.RawMessage(`{"command":"go test ./..."}`)}})
	w.Event(adapter.Event{Kind: adapter.EventUsage, Usage: &adapter.Usage{
		InputTokens: 900, OutputTokens: 100, Model: "fake-1"}})
	w.Close(runlog.StatusSubmitted)

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tail.Run(ctx, tail.Options{
		Store: s, Ref: "run-1", Out: &out, Interval: time.Millisecond,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"· claimed on queue mac-claude",
		"  reading the test",
		"▸ Bash: go test ./...",
		"finished submitted",
		"900 in / 100 out",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// No status block on a pipe: it could never be erased, so it would be a
	// duplicated line every refresh.
	if strings.Contains(got, "\x1b[") {
		t.Errorf("escape sequences reached a pipe:\n%q", got)
	}
}

// A live run is followed until it settles, and the last thing the agent said
// arrives before the view exits.
func TestLiveRunIsFollowedUntilItSettles(t *testing.T) {
	s := newStore(t)
	w := s.Create(runlog.Meta{RunID: "run-1", Queue: "mac-codex", Direction: "Write the docs"})
	w.Runner(runlog.KindClaimed, "claimed on queue mac-codex")

	out := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- tail.Run(ctx, tail.Options{
			Store: s, Ref: "run-1", Out: out, Interval: 2 * time.Millisecond,
		})
	}()

	time.Sleep(20 * time.Millisecond)
	w.Event(adapter.Event{Kind: adapter.EventText, Text: "halfway through"})
	time.Sleep(20 * time.Millisecond)
	w.Event(adapter.Event{Kind: adapter.EventDone, Result: &adapter.Result{
		Status: adapter.StatusSucceeded, Summary: "wrote docs/tail.md"}})
	w.Close(runlog.StatusSubmitted)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("Run did not exit after the run settled. Output:\n%s", out.String())
	}
	got := out.String()
	for _, want := range []string{"halfway through", "✓ succeeded — wrote docs/tail.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// --raw is the log's own JSON, one record per line, and nothing else: no
// rendering, no footer, no totals.
func TestRawPrintsTheLogsJSON(t *testing.T) {
	s := newStore(t)
	w := s.Create(runlog.Meta{RunID: "run-1"})
	w.Event(adapter.Event{Kind: adapter.EventText, Text: "hello"})
	w.Close(runlog.StatusSubmitted)

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tail.Run(ctx, tail.Options{
		Store: s, Ref: "run-1", Raw: true, Out: &out, Interval: time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1:\n%s", len(lines), out.String())
	}
	var rec runlog.Record
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("--raw did not print JSON: %v\n%s", err, lines[0])
	}
	if rec.Text != "hello" || rec.RunID != "run-1" {
		t.Errorf("record = %+v", rec)
	}
}

// --since bounds the replay. The totals still count everything, because a
// token count that started halfway through the run would be wrong and the
// point of the number is the total.
func TestSinceBoundsTheReplayButNotTheTotals(t *testing.T) {
	s := newStore(t)
	w := s.Create(runlog.Meta{RunID: "run-1"})
	old := time.Now().Add(-time.Hour)
	w.Write(runlog.Record{Source: runlog.SourceAgent, Kind: string(adapter.EventText),
		Text: "an hour ago", At: old})
	w.Write(runlog.Record{Source: runlog.SourceAgent, Kind: string(adapter.EventUsage),
		Usage: &adapter.Usage{InputTokens: 5000}, At: old})
	w.Event(adapter.Event{Kind: adapter.EventText, Text: "just now"})
	w.Close(runlog.StatusSubmitted)

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tail.Run(ctx, tail.Options{
		Store: s, Ref: "run-1", Since: time.Minute, SinceSet: true,
		Out: &out, Interval: time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "an hour ago") {
		t.Errorf("--since 1m replayed an hour-old record:\n%s", got)
	}
	if !strings.Contains(got, "just now") {
		t.Errorf("--since 1m dropped a current record:\n%s", got)
	}
	if !strings.Contains(got, "5,000 in") {
		t.Errorf("the totals did not count the records before the window:\n%s", got)
	}
}

// With no argument it is the all-queues view: every run in flight, each line
// labelled with its run, and a status block redrawn in place on a terminal.
func TestWatchShowsEveryRunInFlight(t *testing.T) {
	s := newStore(t)
	a := s.Create(runlog.Meta{RunID: "aaa11111", Queue: "mac-claude", Direction: "One"})
	b := s.Create(runlog.Meta{RunID: "bbb22222", Queue: "mac-codex", Direction: "Two"})
	defer a.Close(runlog.StatusSubmitted)
	defer b.Close(runlog.StatusSubmitted)

	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tail.Run(ctx, tail.Options{
			Store: s, Out: out, TTY: true, Width: 100, Interval: 2 * time.Millisecond,
		})
	}()

	a.Event(adapter.Event{Kind: adapter.EventText, Text: "from the first run"})
	b.Event(adapter.Event{Kind: adapter.EventText, Text: "from the second run"})

	waitFor(t, out, cancel, "from the first run", "from the second run")

	// Now that the status block is on screen, one more record proves the
	// in-place drawing: the block is erased, the new line scrolls, the block
	// comes back.
	a.Event(adapter.Event{Kind: adapter.EventText, Text: "and one more"})
	waitFor(t, out, cancel, "and one more", "\x1b[J")

	got := out.String()
	// Both runs on screen at once means every line has to say which run it is.
	if !strings.Contains(got, "aaa11111 ") || !strings.Contains(got, "bbb22222 ") {
		t.Errorf("lines are not labelled with their run:\n%s", got)
	}
	if !strings.Contains(got, "QUEUE") || !strings.Contains(got, "mac-claude") {
		t.Errorf("no dashboard block:\n%s", got)
	}
	if !strings.Contains(got, "\x1b[3A") {
		// Up by exactly the block's height — three lines here, a header and
		// two runs. Getting this count wrong is how an in-place renderer
		// starts eating the scrollback above it.
		t.Errorf("the erase did not move up by the block's height:\n%q", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop when the context was cancelled")
	}
}

// An empty machine says so, and says how to read the last run back, rather
// than opening on a blank screen that looks broken.
func TestWatchSaysWhenNothingIsInFlight(t *testing.T) {
	s := newStore(t)
	s.Create(runlog.Meta{RunID: "aaa11111", Queue: "mac-claude"}).Close(runlog.StatusStuck)

	out := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := tail.Run(ctx, tail.Options{Store: s, Out: out, Interval: 5 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "no runs in flight") || !strings.Contains(got, "rein tail aaa11111") {
		t.Errorf("the idle announcement is not useful:\n%s", got)
	}
}

func TestUnknownRefIsNotFound(t *testing.T) {
	s := newStore(t)
	err := tail.Run(context.Background(), tail.Options{Store: s, Ref: "nope", Out: &bytes.Buffer{}})
	if !errors.Is(err, runlog.ErrNotFound) {
		t.Errorf("Run with an unknown ref = %v, want ErrNotFound", err)
	}
}

// A queue name follows every run on that queue, which is what a person means
// by `rein tail mac-claude`.
func TestQueueRefFollowsItsRuns(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"aaa11111", "bbb22222"} {
		w := s.Create(runlog.Meta{RunID: id, Queue: "mac-claude", Direction: "d"})
		w.Event(adapter.Event{Kind: adapter.EventText, Text: "said by " + id})
		w.Close(runlog.StatusSubmitted)
	}
	s.Create(runlog.Meta{RunID: "ccc33333", Queue: "mac-codex"}).Close(runlog.StatusSubmitted)

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tail.Run(ctx, tail.Options{
		Store: s, Ref: "mac-claude", Out: &out, Interval: time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "said by aaa11111") || !strings.Contains(got, "said by bbb22222") {
		t.Errorf("both runs on the queue should have been replayed:\n%s", got)
	}
	if strings.Contains(got, "ccc33333") {
		t.Errorf("a run on another queue was followed:\n%s", got)
	}
}
