package runner_test

import (
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
)

// ark:rein#112: every run Rein drives reaches Elk through an MCP server named
// elk that holds the queue's OWN run-scoped token — the one Rein claims and
// reports with — and never a person's connector (ark:rein#21). Rein's own
// tools on that server are denied to the session, so it cannot submit its own
// deliverable, claim another run or rotate the token out from under Rein.
// A Wrangler cycle keeps the owner's connector exactly as before (ark:rein#50).

// connectorHarness is a harness whose fake can deny MCP tools by name, as the
// Claude adapter can, so the run loop hands it the run connector.
func connectorHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.agent.DeniesMCPTools = true
	return h
}

// assertRunConnector checks one RunSpec holds the run connector and nothing
// of a person's.
func assertRunConnector(t *testing.T, h *harness, s adapter.RunSpec) {
	t.Helper()
	srv, ok := s.MCPServers[adapter.RunConnectorName].(map[string]any)
	if !ok {
		t.Fatalf("the run has no %q MCP server: %#v", adapter.RunConnectorName, s.MCPServers)
	}
	// The session gets a loopback proxy's address, never the token: the
	// proxy holds it (elkproxy.go).
	if u, _ := srv["url"].(string); !strings.HasPrefix(u, "http://127.0.0.1:") || strings.Contains(u, token) {
		t.Errorf("the run connector's address = %q, want this run's loopback proxy", u)
	}
	if s.WranglerConnectorAccount != "" {
		t.Errorf("an ordinary run was handed the owner's connector %q", s.WranglerConnectorAccount)
	}
	for k, v := range s.Env {
		if v == token {
			t.Errorf("the queue's token reached the session's environment as %s", k)
		}
	}
	for _, tool := range adapter.RunConnectorDeniedTools {
		if !has(s.DeniedTools, "mcp__elk__"+tool) {
			t.Errorf("the session may call Rein's own tool %s: DeniedTools = %v", tool, s.DeniedTools)
		}
	}
	for _, kept := range []string{"create_action", "link_action", "send_message", "capture_to_elk", "list_actions", "dispatch_action"} {
		if has(s.DeniedTools, "mcp__elk__"+kept) {
			t.Errorf("the run connector denies %s, which the run needs", kept)
		}
	}
	if !strings.Contains(s.SystemPrompt, "the MCP server `elk`") {
		t.Error("the run was not told it has an Elk connector")
	}
}

func TestEveryRunGetsAnElkConnectorOnTheQueuesOwnToken(t *testing.T) {
	h := connectorHarness(t)
	h.elk.Text("claim_run", order("run-1", "mcp:elk"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	specs := h.agent.Specs()
	if len(specs) != 1 {
		t.Fatalf("a run requiring mcp:elk did not start\nlog:\n%s", h.log)
	}
	assertRunConnector(t, h, specs[0])
	if caps := declared(h); !has(caps, "mcp:elk") {
		t.Errorf("the queue does not declare mcp:elk: %v", caps)
	}
	if h.submitted().Arg("status") != "ready" {
		t.Errorf("the run did not finish normally\nlog:\n%s", h.log)
	}
}

func TestAnAdapterThatCannotDenyToolsGetsNoElkConnector(t *testing.T) {
	// An adapter without MCP tool denial support (such as Grok) gets no
	// connector and makes no claim to have one.
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	s := h.agent.Specs()[0]
	if s.MCPServers != nil || len(s.DeniedTools) != 0 {
		t.Errorf("a run on an adapter that cannot deny tools got the connector: servers %#v, denied %v",
			s.MCPServers, s.DeniedTools)
	}
	if caps := declared(h); has(caps, "mcp:elk") {
		t.Errorf("the queue declares mcp:elk without holding it: %v", caps)
	}
	if !strings.Contains(h.log.String(), "cannot keep Rein's own Elk tools out of a session") {
		t.Errorf("the startup log does not say why the run connector was left out:\n%s", h.log)
	}
}

func TestOnAWranglerQueueABuildRunGetsTheRunConnectorAndACycleTheOwners(t *testing.T) {
	for _, spelling := range []string{"wrangler", "pm"} {
		t.Run(spelling, func(t *testing.T) {
			h := wranglerHarness(t, spelling)
			h.agent.DeniesMCPTools = true
			orders := []string{order("run-1", "mcp:elk"), order("run-2", "pm")}
			for _, o := range orders {
				h.elk.Text("claim_run", o)
				if err := h.run(runner.Options{}); err != nil {
					t.Fatalf("%v\nlog:\n%s", err, h.log)
				}
			}
			specs := h.agent.Specs()
			if len(specs) != 2 {
				t.Fatalf("started %d runs, want 2\nlog:\n%s", len(specs), h.log)
			}
			build, cycle := specs[0], specs[1]
			assertRunConnector(t, h, build)
			if strings.Contains(build.SystemPrompt, "This run is a Wrangler cycle") {
				t.Error("a build run got the Wrangler rule")
			}
			if list := preflightList(t, build.SystemPrompt); has(list, "pm") || !has(list, "mcp:elk") {
				t.Errorf("a build run's preflight list = %v, want mcp:elk and no pm", list)
			}

			// The cycle is exactly what it was before the run connector.
			if cycle.WranglerConnectorAccount != "workspace/owner" || cycle.MCPServers != nil {
				t.Errorf("the cycle did not get exactly the owner's connector: account %q, servers %#v",
					cycle.WranglerConnectorAccount, cycle.MCPServers)
			}
			if len(cycle.DeniedTools) != 0 {
				t.Errorf("the cycle's tools were narrowed: %v", cycle.DeniedTools)
			}
			if strings.Contains(cycle.SystemPrompt, "the MCP server `elk`, acting as this queue's agent") {
				t.Error("the cycle was told its connector is the queue's")
			}
			if list := preflightList(t, cycle.SystemPrompt); !has(list, "pm") || !has(list, "mcp:elk") {
				t.Errorf("the cycle's preflight list lost the Wrangler's capabilities: %v", list)
			}
			if caps := declared(h); !has(caps, "pm") || !has(caps, "mcp:elk") {
				t.Errorf("the Wrangler queue stopped declaring pm and mcp:elk: %v", caps)
			}
		})
	}
}

func TestAConfigServerNamedElkGivesWayToTheRunConnector(t *testing.T) {
	// A config.toml server may hold any credential, a person's included. On a
	// queue that gets the run connector, the name elk is that connector's.
	h := connectorHarness(t)
	t.Setenv("REIN_TEST_PERSONAL_ELK", "never-read")
	h.cfg.MCPServers = map[string]config.MCPServer{
		"elk": {URL: "https://example.invalid/elk-mcp", BearerTokenEnvVar: "REIN_TEST_PERSONAL_ELK"},
	}
	h.cfg.Queues[0].MCPServers = []string{"elk"}
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	s := h.agent.Specs()[0]
	assertRunConnector(t, h, s)
	if !strings.Contains(h.log.String(), "the name is Rein's own Elk connector") {
		t.Errorf("the startup log does not say the config server was left out:\n%s", h.log)
	}
}

func TestAScopedQueueRunGetsTheRunConnectorBesideItsSecrets(t *testing.T) {
	h := connectorHarness(t)
	h.cfg.Queues[0].Secrets = map[string]string{"POSTHOG_API_KEY": "acme-posthog"}
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{Secrets: newFakeItems("acme-posthog/rein", posthogValue)}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	s := h.agent.Specs()[0]
	assertRunConnector(t, h, s)
	if s.Env["POSTHOG_API_KEY"] != posthogValue {
		t.Errorf("the scoped secret was lost: %v", s.Env["POSTHOG_API_KEY"] != "")
	}
}

func TestTheQueuesTokenIsNeverWrittenDown(t *testing.T) {
	// The run holds the token Rein claims with, so an agent that echoes it
	// must not put it into the run log, the daemon log or anything sent to Elk.
	h := connectorHarness(t)
	h.agent.Script, h.agent.Result = leakingScript(token)
	logDir := t.TempDir()
	logs, err := runlog.Open(logDir)
	if err != nil {
		t.Fatal(err)
	}
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{Logs: logs}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if len(h.agent.Specs()) != 1 {
		t.Fatalf("the run did not start\nlog:\n%s", h.log)
	}
	if all := everythingWritten(t, h, logDir); strings.Contains(all, token) {
		i := strings.Index(all, token)
		t.Errorf("the queue's token was written down, after: …%s", all[max(0, i-80):i])
	}
}
