//go:build integration

// Against the real binary. `go test -tags integration ./internal/adapter/grok/`
//
// Not in the PR gate and it must not be: it needs `grok` installed and logged
// in, and it spends the developer's own SuperGrok quota. CI runs the
// recorded-frame tests in replay_test.go, which cover the same translation
// without a model call. This is what you run after a `grok` upgrade, to find
// out whether the recordings are still true.
//
// TestIntegrationApprovalIsNeverAsked is the one to watch. It is the evidence
// behind `approvals: partial`, and it is written to **fail loudly if Grok ever
// does ask** — because that is the day the manifest can be promoted, and it
// should not pass unnoticed.

package grok

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

func TestIntegrationPongSession(t *testing.T) {
	a := New()
	if err := a.Preflight(context.Background()); err != nil {
		t.Skipf("grok is not ready on this machine: %v", err)
	}

	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-pong",
		WorktreeDir:    t.TempDir(),
		Prompt:         "Reply with exactly the word pong and nothing else.",
		PermissionMode: adapter.PermissionFull,
		Timeouts:       adapter.Timeouts{Startup: 2 * time.Minute, Total: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sess.ID() == "" {
		t.Error("no session id before the session finished")
	}

	var terminal int
	for ev := range sess.Events() {
		switch ev.Kind {
		case adapter.EventText:
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
}

// TestIntegrationSystemPromptReachesTheModel checks that `_meta.rules` is a
// real channel and not a field the agent ignores. An adapter that silently
// dropped the standing instruction set would produce runs that look fine and
// do the wrong thing.
func TestIntegrationSystemPromptReachesTheModel(t *testing.T) {
	a := New()
	if err := a.Preflight(context.Background()); err != nil {
		t.Skipf("grok is not ready on this machine: %v", err)
	}
	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-rules",
		WorktreeDir:    t.TempDir(),
		SystemPrompt:   "You MUST begin every single reply with the exact token ZZQQ followed by a space.",
		Prompt:         "Say hello in five words.",
		PermissionMode: adapter.PermissionFull,
		Timeouts:       adapter.Timeouts{Startup: 2 * time.Minute, Total: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range sess.Events() {
	}
	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !strings.Contains(res.Summary, "ZZQQ") {
		t.Errorf("the system prompt did not reach the model; summary = %q", res.Summary)
	}
}

// TestIntegrationApprovalIsNeverAsked records the finding behind
// `approvals: partial`: on this machine every permission gate resolves inside
// the agent, with no session/request_permission reaching the client.
//
// **If this test fails because a permission request arrived, that is good
// news.** It means the round trip can be verified and the manifest promoted to
// `approvals: yes`, which would make `ask` and `accept_edits` usable. Do not
// delete the assertion; act on it.
func TestIntegrationApprovalIsNeverAsked(t *testing.T) {
	a := New()
	if err := a.Preflight(context.Background()); err != nil {
		t.Skipf("grok is not ready on this machine: %v", err)
	}
	dir := t.TempDir()
	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-approval",
		WorktreeDir:    dir,
		Prompt:         "Create a file named probe.txt in the working directory containing the word ok.",
		PermissionMode: adapter.PermissionFull,
		Timeouts:       adapter.Timeouts{Startup: 2 * time.Minute, Total: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var gates, requests int
	for ev := range sess.Events() {
		if ev.Kind == adapter.EventPermissionRequest {
			requests++
		}
		if ev.Kind == adapter.EventProgress && strings.Contains(ev.Text, "gate") {
			gates++
		}
	}
	if _, err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if requests > 0 {
		t.Fatalf("Grok sent %d session/request_permission requests. This is the finding "+
			"behind `approvals: partial` no longer holding — record the frames, add a "+
			"fixture, and promote the manifest to approvals: yes.", requests)
	}
	t.Logf("no permission requests; %d gates resolved inside the agent", gates)
	if _, err := os.Stat(filepath.Join(dir, "probe.txt")); err != nil {
		t.Logf("the agent did not write the file: %v", err)
	}
}

// TestIntegrationResume proves session/load round-trips a session id across
// two processes, and that the replayed history is suppressed rather than
// re-reported.
func TestIntegrationResume(t *testing.T) {
	a := New()
	if err := a.Preflight(context.Background()); err != nil {
		t.Skipf("grok is not ready on this machine: %v", err)
	}
	dir := t.TempDir()
	base := adapter.RunSpec{
		RunID:          "integration-resume",
		WorktreeDir:    dir,
		PermissionMode: adapter.PermissionFull,
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
	var texts []string
	for ev := range sess2.Events() {
		if ev.Kind == adapter.EventText {
			texts = append(texts, ev.Text)
		}
	}
	res2, err := sess2.Wait()
	if err != nil {
		t.Fatalf("resumed turn: %v", err)
	}
	if !strings.Contains(strings.ToUpper(res2.Summary), "MARMALADE") {
		t.Errorf("the resumed session did not remember; summary = %q", res2.Summary)
	}
	for _, text := range texts {
		if strings.Contains(strings.ToLower(text), "ok") && len(text) < 5 {
			t.Errorf("the first turn's reply was re-emitted as fresh text; "+
				"session/load's replayed history must be suppressed: %q", text)
		}
	}
}

// TestIntegrationSessionSeesOnlyTheSpecsMCPServers is the regression test for
// ark:rein#21 — the dogfood run (arun-18298891) whose agent found the
// developer's Elk connector in ~/.grok/config.toml and called
// submit_deliverable under his identity, mid-run, before Rein submitted.
//
// It asserts twice. The protocol assertion is the real one: Grok's own
// `_x.ai/mcp/servers_updated` must arrive empty of anything the packet did not
// ask for. The model assertion is a second opinion — the agent is asked what
// tools it has, and its answer must not name an Elk tool.
//
// **This test failing is a security regression, not a flake.**
func TestIntegrationSessionSeesOnlyTheSpecsMCPServers(t *testing.T) {
	a := New()
	if err := a.Preflight(context.Background()); err != nil {
		t.Skipf("grok is not ready on this machine: %v", err)
	}

	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:       "integration-mcp-isolation",
		WorktreeDir: t.TempDir(),
		Prompt: "Without calling any tool, list the names of every MCP tool available to you, " +
			"one per line. If you have none, reply with exactly: none.",
		PermissionMode: adapter.PermissionFull,
		Timeouts:       adapter.Timeouts{Startup: 2 * time.Minute, Total: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	configured := map[string]bool{}
	var said strings.Builder
	for ev := range sess.Events() {
		if ev.Kind == adapter.EventProgress && ev.Text == "_x.ai/mcp/servers_updated" {
			var n struct {
				MCPServers []struct {
					Name string `json:"name"`
				} `json:"mcpServers"`
			}
			if json.Unmarshal(ev.Raw, &n) == nil {
				for _, srv := range n.MCPServers {
					configured[srv.Name] = true
				}
			}
		}
		if ev.Kind == adapter.EventText {
			said.WriteString(ev.Text)
		}
	}
	if _, err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	t.Logf("config-derived MCP servers this session saw: %v", configured)
	if len(configured) != 0 {
		t.Errorf("the session inherited configured MCP servers %v; the private GROK_HOME is not holding", configured)
	}

	answer := strings.ToLower(said.String())
	for _, forbidden := range []string{"submit_deliverable", "capture_to_elk", "claim_run", "report_progress"} {
		if strings.Contains(answer, forbidden) {
			t.Errorf("the agent reports having the Elk tool %q; the developer's connector reached the session", forbidden)
		}
	}
	t.Logf("the agent's account of its tools: %s", said.String())
}

// TestIntegrationTurnEndHookFiresUnderACP is the evidence that the signal the
// Grok back-off rests on can reach Rein at all (ark:rein#40): a hook Rein
// writes into the private GROK_HOME is discovered and run for a turn driven
// over ACP, and its payload lands in the home's log.
//
// A real rate limit cannot be produced on demand, and Grok falls back to its
// default model rather than failing on an unknown one — so this subscribes
// the same hook to `Stop` as well, which fires on every completed turn, and
// proves the path with one "pong". StopFailure differs only in which turn end
// fires it (~/.grok/docs/user-guide/10-hooks.md). It spends one tiny turn.
func TestIntegrationTurnEndHookFiresUnderACP(t *testing.T) {
	if !hookSupported() {
		t.Skip("no StopFailure hook on this platform")
	}
	saved := hookEvents
	hookEvents = []string{"StopFailure", "Stop"}
	defer func() { hookEvents = saved }()

	a := New()
	if err := a.Preflight(context.Background()); err != nil {
		t.Skipf("grok is not ready on this machine: %v", err)
	}
	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-turn-end-hook",
		WorktreeDir:    t.TempDir(),
		Prompt:         "Reply with exactly the word pong and nothing else.",
		PermissionMode: adapter.PermissionFull,
		Timeouts:       adapter.Timeouts{Startup: 2 * time.Minute, Total: 3 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for ev := range sess.Events() {
		if ev.Text == "hook_execution" {
			t.Logf("%s %s", ev, ev.Raw)
		}
	}
	res, werr := sess.Wait()
	t.Logf("result %s, err %v", res.Status, werr)

	s := sess.(*session)
	s.mu.Lock()
	reports, rl := s.stopFailures, s.rateLimit
	s.mu.Unlock()
	t.Logf("turn-end reports: %+v; rate limit: %+v", reports, rl)
	if len(reports) == 0 {
		t.Fatal("no turn-end report reached the private home's log: Rein's hook did not run under ACP")
	}
	if rl != nil {
		t.Errorf("a completed turn was read as a rate limit: %+v", rl)
	}
}
