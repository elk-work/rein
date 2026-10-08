//go:build integration

package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/claude"
	"github.com/elk-work/rein/internal/control"
)

// The attach hatch against the real `claude` binary, on the developer's own
// login. Behind a build tag, and CI never runs it:
//
//	go test -tags integration -run TestIntegrationAttach ./internal/runner/
//
// The fake-adapter tests in attach_test.go prove the control plane, the
// pause, the permission handover and the deliverable's note. What only a real
// vendor can prove is the one thing the design rests on: that a line typed by
// a person, carried over the control socket and handed to
// [adapter.Session.Send], reaches a session that is already running in another
// goroutine and is acted on.
//
// It is internal (package runner) because liveSession is what the control
// plane serves, and building one directly is what keeps this test about the
// hatch rather than about Elk.
//
// It spends a small number of subscription tokens. The prompt is trivial and
// the session is read_only — `plan` mode — so a failing test can never leave a
// file behind.

func requireClaudeAdapter(t *testing.T) *claude.Adapter {
	t.Helper()
	a := claude.New()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("no claude on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := a.Preflight(ctx); err != nil {
		t.Skipf("preflight: %v", err)
	}
	return a
}

// integrationRepo is a git worktree-shaped directory: a session is told it owns
// the directory it is in, and a git repository is what that looks like.
func integrationRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=rein@example.test", "-c", "user.name=rein", "commit", "-qm", "init", "--allow-empty"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestIntegrationAttachSteersALiveClaudeSession(t *testing.T) {
	a := requireClaudeAdapter(t)

	if !a.Manifest().Has(adapter.CapSteer) {
		t.Fatal("the claude adapter no longer declares steer; attach would be watch-only")
	}

	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:       "integration-attach",
		WorktreeDir: integrationRepo(t),
		Prompt: "Reply with exactly the two words REIN ONE, then stop and wait. " +
			"If you are later sent another message, reply with exactly the two words REIN TWO.",
		PermissionMode: adapter.PermissionReadOnly,
		Timeouts:       adapter.Timeouts{Startup: 2 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drain the events the way the run loop does — an adapter that cannot
	// deliver an event blocks — while recording the assistant text.
	var (
		mu    sync.Mutex
		text  strings.Builder
		drain = make(chan struct{})
	)
	go func() {
		defer close(drain)
		for ev := range sess.Events() {
			t.Logf("event %s", ev)
			if ev.Kind == adapter.EventText {
				mu.Lock()
				text.WriteString(ev.Text + "\n")
				mu.Unlock()
			}
		}
	}()
	said := func() string {
		mu.Lock()
		defer mu.Unlock()
		return text.String()
	}

	// The control plane, over a real endpoint, with the session registered
	// exactly as the run loop registers it.
	home, err := os.MkdirTemp("", "rn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	ln, err := control.Listen(home)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	reg := newSessionRegistry()
	reg.add(&liveSession{
		runID: "integration-attach", queue: "int-claude", agentKind: a.Name(),
		direction: "an integration test", started: time.Now(),
		sess: sess, manifest: a.Manifest(),
		logf: func(f string, args ...any) { t.Logf(f, args...) },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = control.Serve(ctx, ln, reg, nil)
	}()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dialCancel()
	c, err := control.Dial(dialCtx, home)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	att, err := c.Attach("")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !att.Run.Interactive {
		t.Fatal("a claude session reported itself non-interactive")
	}

	// Wait for the first answer, so the steer lands on a session that is
	// genuinely running rather than one that has not started talking yet.
	waitForText(t, said, "REIN ONE", 3*time.Minute)

	if err := c.Input("Now do the second part."); err != nil {
		t.Fatalf("the person's line did not reach the session: %v", err)
	}
	waitForText(t, said, "REIN TWO", 3*time.Minute)

	c.Close()
	cancel()
	<-served

	_ = sess.Interrupt(context.Background())
	select {
	case <-drain:
	case <-time.After(2 * time.Minute):
		t.Fatal("the session never closed its event channel")
	}
	if _, err := sess.Wait(); err != nil {
		t.Logf("Wait: %v", err)
	}
	_ = filepath.Clean(home)
}

func waitForText(t *testing.T, said func() string, want string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if strings.Contains(said(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never said %q within %s. What it said:\n%s", want, budget, said())
		}
		time.Sleep(250 * time.Millisecond)
	}
}
