//go:build integration

package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// These tests drive the real `claude` binary on the developer's own login, so
// they are behind a build tag and CI never runs them:
//
//	go test -tags integration ./internal/adapter/claude/
//
// They spend a small number of subscription tokens. The prompts are trivial on
// purpose, and every run is read_only — `plan` mode — so a failing test can
// never leave a file behind.

func requireClaude(t *testing.T) *Adapter {
	t.Helper()
	a := New()
	if _, err := exec.LookPath(a.binary()); err != nil {
		t.Skipf("no %s on PATH: %v", a.binary(), err)
	}
	return a
}

// tempRepo is a git worktree-shaped directory: these sessions are told they own
// the directory they are in, and a git repo is what that looks like.
func tempRepo(t *testing.T) string {
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
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("the secret word is banana\n"), 0o600); err != nil {
		t.Fatalf("seeding the repo: %v", err)
	}
	return dir
}

func TestIntegrationPreflight(t *testing.T) {
	a := requireClaude(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := a.Preflight(ctx); err != nil {
		t.Fatalf("Preflight failed on a machine with claude installed: %v", err)
	}
}

// collect drains a session and returns its events and result.
func collect(t *testing.T, sess adapter.Session, budget time.Duration) ([]adapter.Event, adapter.Result, error) {
	t.Helper()
	var evs []adapter.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range sess.Events() {
			t.Logf("event %s", ev)
			evs = append(evs, ev)
		}
	}()
	select {
	case <-done:
	case <-time.After(budget):
		_ = sess.Interrupt(context.Background())
		<-done
		t.Fatalf("the session did not finish within %s", budget)
	}
	res, err := sess.Wait()
	return evs, res, err
}

func TestIntegrationTrivialSession(t *testing.T) {
	a := requireClaude(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Preflight(ctx); err != nil {
		t.Skipf("preflight: %v", err)
	}

	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-trivial",
		WorktreeDir:    tempRepo(t),
		Prompt:         "Reply with exactly the two words: REIN OK",
		PermissionMode: adapter.PermissionReadOnly,
		Timeouts:       adapter.Timeouts{Startup: 2 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	evs, res, waitErr := collect(t, sess, 5*time.Minute)
	if waitErr != nil {
		t.Fatalf("Wait: %v", waitErr)
	}
	if res.Status != adapter.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded (summary %q)", res.Status, res.Summary)
	}
	if res.SessionID == "" {
		t.Error("no vendor session id; --resume and `rein attach` both need one")
	}
	if res.Usage.OutputTokens == 0 {
		t.Error("no output tokens reported; Elk prices from these")
	}
	if res.Usage.Model == "" {
		t.Error("no model reported")
	}

	var sawText, sawTerminal bool
	for _, ev := range evs {
		if ev.Kind == adapter.EventText && strings.Contains(ev.Text, "REIN OK") {
			sawText = true
		}
		if ev.Kind.Terminal() {
			sawTerminal = true
		}
	}
	if !sawText {
		t.Error("no assistant text event carried the reply")
	}
	if !sawTerminal {
		t.Error("no terminal event")
	}

	// read_only must not have produced a transcript of mutations; the point of
	// plan mode is that nothing lands.
	if _, err := os.Stat(filepath.Join(sess.(*Session).spec.WorktreeDir, "REIN.txt")); err == nil {
		t.Error("a read_only session wrote a file")
	}
}

// TestIntegrationResume proves the CapResume claim: a second session started
// with the first one's id continues the same conversation.
func TestIntegrationResume(t *testing.T) {
	a := requireClaude(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Preflight(ctx); err != nil {
		t.Skipf("preflight: %v", err)
	}
	dir := tempRepo(t)

	first, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-resume-1",
		WorktreeDir:    dir,
		Prompt:         "Remember this number: 4287. Reply with just the number.",
		PermissionMode: adapter.PermissionReadOnly,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, res1, err := collect(t, first, 5*time.Minute)
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	if res1.SessionID == "" {
		t.Fatal("the first session reported no id to resume")
	}

	second, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-resume-2",
		WorktreeDir:    dir,
		Prompt:         "What number did I ask you to remember? Reply with just the number.",
		PermissionMode: adapter.PermissionReadOnly,
		ResumeID:       res1.SessionID,
	})
	if err != nil {
		t.Fatalf("Start with a resume id: %v", err)
	}
	evs, res2, err := collect(t, second, 5*time.Minute)
	if err != nil {
		t.Fatalf("resumed session: %v", err)
	}
	if res2.Status != adapter.StatusSucceeded {
		t.Fatalf("resumed status = %q (summary %q)", res2.Status, res2.Summary)
	}

	var recalled bool
	for _, ev := range evs {
		if strings.Contains(ev.Text, "4287") {
			recalled = true
		}
	}
	if !recalled && !strings.Contains(res2.Summary, "4287") {
		t.Errorf("the resumed session did not remember the number; summary %q", res2.Summary)
	}
}

// TestIntegrationPermissionDenialIsReported is the auto-deny behaviour that
// makes approvals partial, verified against the real binary rather than a
// recording.
func TestIntegrationPermissionDenialIsReported(t *testing.T) {
	a := requireClaude(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Preflight(ctx); err != nil {
		t.Skipf("preflight: %v", err)
	}

	dir := tempRepo(t)
	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-denial",
		WorktreeDir:    dir,
		Prompt:         "Create a file named blocked.txt containing the word hello. Do it now.",
		PermissionMode: adapter.PermissionReadOnly,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs, _, _ := collect(t, sess, 5*time.Minute)

	// Whatever the agent tried, nothing may have landed, and no event may ask
	// the run loop for an approval this adapter cannot deliver.
	if _, err := os.Stat(filepath.Join(dir, "blocked.txt")); err == nil {
		t.Error("read_only let a write through")
	}
	for _, ev := range evs {
		if ev.Kind == adapter.EventPermissionRequest {
			t.Fatal("emitted a permission_request while declaring approvals=partial")
		}
	}
}

// initLine digs the vendor's `system/init` payload out of the event stream. The
// adapter reports it as a progress event carrying the whole line in Raw, which
// is what makes an assertion about the session's actual configuration possible
// without reaching around the adapter.
type initLine struct {
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	Tools   []string `json:"tools"`
	Plugins []struct {
		Name string `json:"name"`
	} `json:"plugins"`
}

func findInit(t *testing.T, evs []adapter.Event) initLine {
	t.Helper()
	for _, ev := range evs {
		if ev.Kind != adapter.EventProgress || len(ev.Raw) == 0 {
			continue
		}
		var probe struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if err := json.Unmarshal(ev.Raw, &probe); err != nil {
			continue
		}
		if probe.Type != "system" || probe.Subtype != "init" {
			continue
		}
		var out initLine
		if err := json.Unmarshal(ev.Raw, &out); err != nil {
			t.Fatalf("decoding the init line: %v", err)
		}
		return out
	}
	t.Fatal("no system/init event in the stream")
	return initLine{}
}

// TestIntegrationRunInheritsNoMCPServers is the regression test for
// ark:rein#21, and it has to run against the real binary because the whole bug
// was about what the binary loads when nobody tells it not to.
//
// Before the fix, a run whose packet named no MCP servers came up on this Mac
// with 287 MCP tools, 43 of them the Elk connector's — logged in as the
// developer who queued the work. A grok run used exactly that to submit its own
// deliverable to Elk under the developer's identity.
func TestIntegrationRunInheritsNoMCPServers(t *testing.T) {
	a := requireClaude(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Preflight(ctx); err != nil {
		t.Skipf("preflight: %v", err)
	}

	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-no-mcp",
		WorktreeDir:    tempRepo(t),
		Prompt:         "Reply with exactly the two words: REIN OK",
		PermissionMode: adapter.PermissionReadOnly,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs, _, waitErr := collect(t, sess, 5*time.Minute)
	if waitErr != nil {
		t.Fatalf("Wait: %v", waitErr)
	}

	init := findInit(t, evs)

	if len(init.MCPServers) != 0 {
		t.Errorf("the session loaded %d MCP servers with none in the packet: %+v",
			len(init.MCPServers), init.MCPServers)
	}

	// The tool list is the part that actually matters: a server the agent
	// cannot see is a server it cannot act through.
	var mcpTools, elkTools []string
	for _, name := range init.Tools {
		if strings.HasPrefix(name, "mcp__") {
			mcpTools = append(mcpTools, name)
		}
		if strings.Contains(strings.ToLower(name), "elk") {
			elkTools = append(elkTools, name)
		}
	}
	if len(mcpTools) != 0 {
		t.Errorf("the agent can see %d inherited MCP tools, e.g. %v", len(mcpTools), mcpTools[:min(3, len(mcpTools))])
	}
	if len(elkTools) != 0 {
		t.Errorf("the agent can reach Elk directly, under the developer's own login: %v", elkTools)
	}

	// Plugins come from the developer's ~/.claude, not from MCP, so they prove
	// the settings-source half of the isolation rather than the strict-MCP half.
	if len(init.Plugins) != 0 {
		t.Errorf("the session loaded the developer's plugins: %+v", init.Plugins)
	}
}

// TestIntegrationRunGetsExactlyThePacketsServers is the other side of it: the
// packet's own servers must still arrive. The server here cannot start, which
// is fine — what is asserted is the set the session was given, not that it
// connected.
func TestIntegrationRunGetsExactlyThePacketsServers(t *testing.T) {
	a := requireClaude(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Preflight(ctx); err != nil {
		t.Skipf("preflight: %v", err)
	}

	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-packet-mcp",
		WorktreeDir:    tempRepo(t),
		Prompt:         "Reply with exactly the two words: REIN OK",
		PermissionMode: adapter.PermissionReadOnly,
		MCPServers: map[string]any{
			"rein-probe": map[string]any{"command": "/nonexistent/rein-mcp-probe"},
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs, _, _ := collect(t, sess, 5*time.Minute)

	init := findInit(t, evs)
	if len(init.MCPServers) != 1 {
		t.Fatalf("want exactly the packet's one server, got %+v", init.MCPServers)
	}
	if init.MCPServers[0].Name != "rein-probe" {
		t.Errorf("server = %q, want the packet's rein-probe", init.MCPServers[0].Name)
	}
	for _, name := range init.Tools {
		if strings.Contains(strings.ToLower(name), "elk") {
			t.Errorf("naming one server still let the developer's Elk connector in: %s", name)
		}
	}
}
