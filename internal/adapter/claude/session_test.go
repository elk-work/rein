//go:build !windows

package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// These tests drive the real [Session] — the process, the reader goroutine, the
// terminal event, the timeout ladder — against a stub standing in for `claude`.
// The stub replays a recorded fixture, so the whole path from NDJSON to
// [adapter.Result] is exercised on every machine, with no vendor binary, no
// login and no tokens spent. The one thing it cannot prove is that the real
// binary still speaks this dialect; that is what the integration build tag is
// for.

// stubBinary writes an executable that prints script and then drains stdin
// until it is closed, which is exactly how `claude -p --input-format
// stream-json` behaves: it waits for another turn until stdin goes away.
func stubBinary(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-stub")
	body := "#!/bin/sh\n" + script + "\ncat > /dev/null\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("writing the stub: %v", err)
	}
	return path
}

// stubReplaying prints one of the testdata fixtures.
func stubReplaying(t *testing.T, fixture string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatalf("resolving the fixture: %v", err)
	}
	return stubBinary(t, "cat "+abs)
}

func drain(t *testing.T, s adapter.Session) []adapter.Event {
	t.Helper()
	var evs []adapter.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range s.Events() {
			evs = append(evs, ev)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the event channel never closed")
	}
	return evs
}

func startStub(t *testing.T, binary string, spec adapter.RunSpec) adapter.Session {
	t.Helper()
	a := &Adapter{Binary: binary, DisableWatchdog: true}
	if spec.WorktreeDir == "" {
		spec.WorktreeDir = t.TempDir()
	}
	if spec.Prompt == "" {
		spec.Prompt = "do the thing"
	}
	if spec.PermissionMode == "" {
		spec.PermissionMode = adapter.PermissionFull
	}
	sess, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return sess
}

func TestSessionReplaysAFixtureToATerminalEvent(t *testing.T) {
	sess := startStub(t, stubReplaying(t, "tools.jsonl"), adapter.RunSpec{})
	evs := drain(t, sess)

	if len(evs) == 0 {
		t.Fatal("no events")
	}

	// Exactly one terminal event, and it is last.
	var terminals int
	for i, ev := range evs {
		if ev.Kind.Terminal() {
			terminals++
			if i != len(evs)-1 {
				t.Errorf("terminal event at %d of %d; it must be last", i, len(evs)-1)
			}
		}
	}
	if terminals != 1 {
		t.Errorf("terminal events = %d, want exactly 1", terminals)
	}

	// Seq counts from 1 without gaps, so a consumer can tell a dropped event
	// from a quiet agent.
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			t.Errorf("event %d has Seq %d, want %d", i, ev.Seq, i+1)
		}
		if ev.At.IsZero() {
			t.Errorf("event %d has no timestamp", i)
		}
	}

	last := evs[len(evs)-1]
	if last.Kind != adapter.EventDone {
		t.Fatalf("last event = %q, want done", last.Kind)
	}
	if last.Result == nil {
		t.Fatal("the done event carries no result")
	}

	// A consumer reading events and a consumer calling Wait must never
	// disagree.
	res, err := sess.Wait()
	if err != nil {
		t.Errorf("Wait err = %v, want nil", err)
	}
	if res.Status != adapter.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", res.Status)
	}
	if res.Status != last.Result.Status || res.SessionID != last.Result.SessionID {
		t.Errorf("Wait result %+v disagrees with the done event %+v", res, *last.Result)
	}
	if res.SessionID == "" {
		t.Error("the result carries no vendor session id")
	}
	if sess.ID() != res.SessionID {
		t.Errorf("ID() = %q but the result says %q", sess.ID(), res.SessionID)
	}

	// Wait is idempotent.
	res2, err2 := sess.Wait()
	if res2 != res || !errors.Is(err2, err) {
		t.Errorf("Wait is not idempotent: %+v/%v then %+v/%v", res, err, res2, err2)
	}
}

// TestSessionStopsAtTheFirstResult is the multi-turn rule in force: the
// recording has two result lines, and one Rein run is one session, so the
// adapter must stop at the first and close stdin.
func TestSessionStopsAtTheFirstResult(t *testing.T) {
	sess := startStub(t, stubReplaying(t, "multiturn.jsonl"), adapter.RunSpec{})
	evs := drain(t, sess)

	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Summary == "" || res.Summary != "FIRST" {
		t.Errorf("summary = %q, want the FIRST turn's result", res.Summary)
	}

	var terminals int
	for _, ev := range evs {
		if ev.Kind.Terminal() {
			terminals++
		}
	}
	if terminals != 1 {
		t.Errorf("terminal events = %d; the second turn must not produce one", terminals)
	}
}

// TestSessionWithoutAResultFails covers a process that dies immediately — a
// rejected flag, an instant crash — and is also the regression test for the
// flake that broke main on 2026-08-29.
//
// The stub exits before Start can write the opening prompt, so the write gets
// EPIPE. Start used to return that, which made it fail intermittently with
// "broken pipe" and threw away the stderr that says what actually happened. It
// must instead hand back a session whose terminal error carries the stderr, so
// startStub failing here is itself the assertion.
func TestSessionWithoutAResultFails(t *testing.T) {
	// Exits straight away, having said nothing at all.
	stub := stubBinary(t, "echo 'claude: something went wrong' >&2\nexit 3")
	sess := startStub(t, stub, adapter.RunSpec{})
	evs := drain(t, sess)

	if len(evs) == 0 {
		t.Fatal("no events; a failure must still be reported")
	}
	last := evs[len(evs)-1]
	if last.Kind != adapter.EventError {
		t.Fatalf("last event = %q, want error", last.Kind)
	}
	if last.Err == nil {
		t.Error("the error event carries no error")
	}

	res, err := sess.Wait()
	// A failed session sets BOTH, so a caller inspecting either one learns the
	// truth.
	if err == nil {
		t.Error("Wait returned no error for a failed session")
	}
	if res.Status == adapter.StatusSucceeded {
		t.Errorf("status = %q, want a non-succeeded status", res.Status)
	}
	if err != nil && !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("the error drops the process's own stderr: %v", err)
	}
}

func TestSessionInterruptIsNotAFailure(t *testing.T) {
	// Says hello, then hangs. Only an interrupt ends it.
	stub := stubBinary(t, `printf '{"type":"system","subtype":"init","session_id":"s-int","claude_code_version":"2.1.251","apiKeySource":"none"}\n'`+"\nsleep 60")
	sess := startStub(t, stub, adapter.RunSpec{})

	// Let the init line land so the session is properly under way.
	deadline := time.Now().Add(10 * time.Second)
	events := make(chan adapter.Event, 64)
	go func() {
		for ev := range sess.Events() {
			events <- ev
		}
		close(events)
	}()
	for sess.ID() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if sess.ID() != "s-int" {
		t.Fatalf("ID() = %q, want the id from the init line", sess.ID())
	}

	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	res, err := sess.Wait()
	if res.Status != adapter.StatusInterrupted {
		t.Errorf("status = %q, want interrupted", res.Status)
	}
	// An interruption is a decision, not a fault, so it carries no error —
	// matching internal/adapter/fake.
	if err != nil {
		t.Errorf("Wait err = %v, want nil for an interrupted session", err)
	}

	for range events { //nolint:revive // draining
	}
}

func TestSessionSendIsRefusedAfterTheEnd(t *testing.T) {
	sess := startStub(t, stubReplaying(t, "hello.jsonl"), adapter.RunSpec{})
	drain(t, sess)
	if _, err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	err := sess.Send(context.Background(), "one more thing")
	if !errors.Is(err, adapter.ErrSessionClosed) {
		t.Errorf("Send after the terminal event = %v, want ErrSessionClosed", err)
	}
}

func TestSessionStartupTimeout(t *testing.T) {
	// Never emits an init line.
	stub := stubBinary(t, "sleep 60")
	a := &Adapter{Binary: stub, DisableWatchdog: true}
	sess, err := a.Start(context.Background(), adapter.RunSpec{
		WorktreeDir:    t.TempDir(),
		Prompt:         "go",
		PermissionMode: adapter.PermissionFull,
		Timeouts:       adapter.Timeouts{Startup: 500 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain(t, sess)

	res, waitErr := sess.Wait()
	if res.Status != adapter.StatusTimedOut {
		t.Errorf("status = %q, want timed_out", res.Status)
	}
	if waitErr == nil {
		t.Error("a timed-out session must report an error")
	}
}

func TestSessionRunsInTheWorktree(t *testing.T) {
	dir := t.TempDir()
	// Report the working directory as the session id so the test can read it
	// back through the normal path.
	stub := stubBinary(t, `printf '{"type":"result","subtype":"success","session_id":"%s","result":"ok"}\n' "$(pwd)"`)

	sess := startStub(t, stub, adapter.RunSpec{WorktreeDir: dir})
	drain(t, sess)

	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// macOS hands out /var symlinks to /private/var, so compare resolved paths.
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(res.SessionID)
	if got != want {
		t.Errorf("the session ran in %q, want the worktree %q", got, want)
	}
}

func TestSessionCleansUpItsTempDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub binaries are shell scripts")
	}
	a := &Adapter{Binary: stubReplaying(t, "hello.jsonl")}
	sess, err := a.Start(context.Background(), adapter.RunSpec{
		WorktreeDir:    t.TempDir(),
		Prompt:         "go",
		PermissionMode: adapter.PermissionFull,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	tmp := sess.(*Session).tmpDir
	if tmp == "" {
		t.Fatal("the session made no temp directory")
	}
	drain(t, sess)
	if _, err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("the generated settings survived the session at %s", tmp)
	}
}

func TestWranglerSessionCleansConfigOnExitAndInterrupt(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "exit", true: "interrupt"}[interrupt], func(t *testing.T) {
			binary := stubReplaying(t, "hello.jsonl")
			if interrupt {
				binary = stubBinary(t, "sleep 60")
			}
			a := &Adapter{Binary: binary, DisableWatchdog: true, ResolveWranglerConnector: func(context.Context, string) (string, error) { return "https://example.invalid/secret", nil }}
			sess, err := a.Start(context.Background(), adapter.RunSpec{WorktreeDir: t.TempDir(), Prompt: "go", PermissionMode: adapter.PermissionFull, WranglerConnectorAccount: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			s := sess.(*Session)
			path := filepath.Join(s.tmpDir, "mcp.json")
			if interrupt {
				go func() { _ = sess.Interrupt(context.Background()) }()
			}
			drain(t, sess)
			_, _ = sess.Wait()
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("Wrangler config survived session termination")
			}
		})
	}
}

func TestWranglerRefusesWorktreeTempDirectory(t *testing.T) {
	worktree := t.TempDir()
	t.Setenv("TMPDIR", worktree)
	resolved := false
	a := &Adapter{Binary: stubReplaying(t, "hello.jsonl"), DisableWatchdog: true, ResolveWranglerConnector: func(context.Context, string) (string, error) {
		resolved = true
		return "https://example.invalid/secret", nil
	}}
	_, err := a.Start(context.Background(), adapter.RunSpec{WorktreeDir: worktree, Prompt: "go", PermissionMode: adapter.PermissionFull, WranglerConnectorAccount: "owner"})
	if err == nil || !strings.Contains(err.Error(), "outside the worktree") || resolved {
		t.Fatal("unsafe TMPDIR not refused before secret resolution")
	}
}

// A start-up line naming any auth but the plan login stops the session: the
// process is killed, the session fails with ErrNotPlanAuth, and the reason is
// on the terminal event a run loop shows.
func TestSessionStopsWhenTheInitLineIsNotThePlanLogin(t *testing.T) {
	for _, tc := range []struct{ name, field string }{
		{"api key", `,"apiKeySource":"ANTHROPIC_API_KEY"`},
		{"helper", `,"apiKeySource":"apiKeyHelper"`},
		{"not reported", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Says it is on an API key, then would carry on working.
			stub := stubBinary(t, `printf '{"type":"system","subtype":"init","session_id":"s-key","claude_code_version":"2.1.251"`+
				tc.field+`}\n'`+"\nsleep 60")
			sess := startStub(t, stub, adapter.RunSpec{})
			evs := drain(t, sess)
			res, err := sess.Wait()
			if !errors.Is(err, adapter.ErrNotPlanAuth) {
				t.Fatalf("Wait err = %v; want ErrNotPlanAuth", err)
			}
			if res.Status != adapter.StatusFailed {
				t.Errorf("status = %q; want failed", res.Status)
			}
			last := evs[len(evs)-1]
			if last.Kind != adapter.EventError || !strings.Contains(last.Text, "plan login") {
				t.Errorf("terminal event %v %q does not say why it stopped", last.Kind, last.Text)
			}
		})
	}
}
