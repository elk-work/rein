//go:build integration

// Against the real binary. `go test -tags integration ./internal/adapter/codex/`
//
// It is not in the PR gate and must not be: it needs `codex` installed and
// logged in, and it spends the developer's own subscription quota — which
// Codex meters in a five-hour window shared with the human at the keyboard
// (elk `docs/rein.md` §3). CI runs the recorded-frame tests in replay_test.go,
// which cover the same translation without a model call. This is what you run
// after a `codex` upgrade, to find out whether the recording is still true.

package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

func TestIntegrationPreflight(t *testing.T) {
	if err := New().Preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
}

// TestIntegrationReadHeadroom reads the real account's window the way an idle
// queue does (ark:rein#40). It spends no quota: `account/rateLimits/read` is a
// usage read, not a turn.
func TestIntegrationReadHeadroom(t *testing.T) {
	a := New()
	if err := a.Preflight(context.Background()); err != nil {
		t.Skipf("codex is not ready on this machine: %v", err)
	}
	start := time.Now()
	rl, err := a.ReadHeadroom(context.Background())
	if err != nil {
		t.Fatalf("ReadHeadroom: %v", err)
	}
	t.Logf("read in %s: exhausted=%v until=%s status=%q windows=%+v",
		time.Since(start).Round(time.Millisecond), rl.Exhausted, rl.ExhaustedUntil, rl.Status, rl.Windows)
	if len(rl.Windows) == 0 && !rl.Exhausted {
		t.Error("the account reported no window at all")
	}
	for name, w := range rl.Windows {
		if w.ResetsAt.IsZero() || w.Minutes == 0 {
			t.Errorf("window %s is missing its reset or its length: %+v", name, w)
		}
	}
}

// TestIntegrationPongSession is the trivial round trip the fixtures were
// recorded from: one turn, one word back.
func TestIntegrationPongSession(t *testing.T) {
	a := New()
	if err := a.Preflight(context.Background()); err != nil {
		t.Skipf("codex is not ready on this machine: %v", err)
	}

	dir := t.TempDir()
	began := time.Now()
	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-pong",
		WorktreeDir:    dir,
		Prompt:         "Reply with exactly the word pong and nothing else.",
		PermissionMode: adapter.PermissionReadOnly,
		Timeouts:       adapter.Timeouts{Startup: 2 * time.Minute, Total: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// ark:rein#38: the handshake took 47 s here for weeks — Codex indexing the
	// developer's whole history before answering initialize — and nothing
	// failed until it crossed the 90 s timeout under launchd. A start that is
	// slow but not yet fatal is the finding; say so while it is cheap.
	if took := time.Since(began); took > 20*time.Second {
		t.Errorf("Start took %s; it should be seconds. Something is making codex app-server "+
			"do start-up work before it answers — see ark:rein#38", took.Round(time.Second))
	} else {
		t.Logf("Start took %s", took.Round(10*time.Millisecond))
	}
	if sess.ID() == "" {
		t.Error("no thread id before the session finished")
	}

	var text, terminal int
	for ev := range sess.Events() {
		switch ev.Kind {
		case adapter.EventText:
			text++
			t.Logf("text: %s", ev.Text)
		case adapter.EventUsage:
			t.Logf("usage: %+v", *ev.Usage)
		case adapter.EventDone, adapter.EventError:
			terminal++
		}
	}
	if terminal != 1 {
		t.Errorf("got %d terminal events, want 1", terminal)
	}

	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v (result %+v)", err, res)
	}
	if res.Status != adapter.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", res.Status)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "pong") {
		t.Errorf("Summary = %q, want it to contain pong", res.Summary)
	}
	if res.Usage.InputTokens == 0 {
		t.Errorf("no input tokens were reported: %+v", res.Usage)
	}
	if res.SessionID == "" {
		t.Error("Result carries no session id, so the run cannot be resumed")
	}
}

// TestIntegrationReadOnlySandboxHolds is the one that matters: read_only must
// actually prevent a write, not merely ask nicely. A silent downgrade here is
// the failure the whole contract exists to prevent.
func TestIntegrationReadOnlySandboxHolds(t *testing.T) {
	a := New()
	if err := a.Preflight(context.Background()); err != nil {
		t.Skipf("codex is not ready on this machine: %v", err)
	}

	dir := t.TempDir()
	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-readonly",
		WorktreeDir:    dir,
		Prompt:         "Run the shell command `echo ok > written.txt` in the working directory. If it fails, say so and stop.",
		PermissionMode: adapter.PermissionReadOnly,
		Timeouts:       adapter.Timeouts{Startup: 2 * time.Minute, Total: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for ev := range sess.Events() {
		if ev.Kind == adapter.EventPermissionRequest {
			t.Errorf("read_only raised a permission request; approvalPolicy never should prevent it: %+v", ev.Permission)
		}
	}
	if _, err := sess.Wait(); err != nil {
		t.Logf("session ended with %v, which is a legitimate outcome here", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "written.txt")); err == nil {
		t.Fatal("read_only wrote a file; the sandbox did not hold")
	}
}

// TestIntegrationResume proves the thread id round-trips: a second session
// against the same thread can see what the first one was told.
func TestIntegrationResume(t *testing.T) {
	a := New()
	if err := a.Preflight(context.Background()); err != nil {
		t.Skipf("codex is not ready on this machine: %v", err)
	}
	dir := t.TempDir()
	base := adapter.RunSpec{
		RunID:          "integration-resume",
		WorktreeDir:    dir,
		PermissionMode: adapter.PermissionReadOnly,
		Timeouts:       adapter.Timeouts{Startup: 2 * time.Minute, Total: 5 * time.Minute},
	}

	first := base
	first.Prompt = "Remember the word MARMALADE. Reply with just: ok"
	sess, err := a.Start(context.Background(), first)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range sess.Events() {
	}
	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if res.SessionID == "" {
		t.Fatal("no session id to resume from")
	}

	second := base
	second.ResumeID = res.SessionID
	second.Prompt = "What word were you asked to remember? Reply with just that word."
	sess2, err := a.Start(context.Background(), second)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	for range sess2.Events() {
	}
	res2, err := sess2.Wait()
	if err != nil {
		t.Fatalf("resumed turn: %v", err)
	}
	if !strings.Contains(strings.ToUpper(res2.Summary), "MARMALADE") {
		t.Errorf("resumed session did not remember; summary = %q", res2.Summary)
	}
}

// TestIntegrationSessionSeesOnlyTheSpecsMCPServers is the regression test for
// ark:rein#21 — the dogfood run whose agent found the developer's Elk
// connector in his own config and submitted the deliverable under his identity
// before Rein did.
//
// It asserts twice, at two levels. The protocol assertion is the real one: the
// set of MCP servers Codex actually started, read off
// `mcpServer/startupStatus/updated`, must contain nothing the developer
// configured. The model assertion is a second opinion — the agent is asked what
// tools it has, and its answer must not name an Elk tool.
//
// **This test failing is a security regression, not a flake.** Before the
// private CODEX_HOME landed, this same session started `elk` and `node_repl`
// out of ~/.codex/config.toml.
func TestIntegrationSessionSeesOnlyTheSpecsMCPServers(t *testing.T) {
	a := New()
	if err := a.Preflight(context.Background()); err != nil {
		t.Skipf("codex is not ready on this machine: %v", err)
	}

	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:       "integration-mcp-isolation",
		WorktreeDir: t.TempDir(),
		Prompt: "Without calling any tool, list the names of every MCP tool available to you, " +
			"one per line. If you have none, reply with exactly: none.",
		PermissionMode: adapter.PermissionReadOnly,
		Timeouts:       adapter.Timeouts{Startup: 2 * time.Minute, Total: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	started := map[string]bool{}
	var said strings.Builder
	for ev := range sess.Events() {
		if ev.Kind == adapter.EventProgress && ev.Text == "mcpServer/startupStatus/updated" {
			var n struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(ev.Raw, &n) == nil && n.Name != "" {
				started[n.Name] = true
			}
		}
		if ev.Kind == adapter.EventText {
			said.WriteString(ev.Text)
		}
	}
	if _, err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	t.Logf("MCP servers this session started: %v", started)
	for name := range started {
		// codex_apps is a built-in of the binary, not user configuration; it
		// survives an empty CODEX_HOME and is tracked in ark:rein#22.
		if name == "codex_apps" {
			continue
		}
		t.Errorf("the session started MCP server %q, which the packet did not ask for", name)
	}

	answer := strings.ToLower(said.String())
	for _, forbidden := range []string{"submit_deliverable", "capture_to_elk", "claim_run", "report_progress"} {
		if strings.Contains(answer, forbidden) {
			t.Errorf("the agent reports having the Elk tool %q; the developer's connector reached the session", forbidden)
		}
	}
	t.Logf("the agent's account of its tools: %s", said.String())
}
