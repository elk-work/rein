package runner_test

import (
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runner"
)

// The per-run MCP allow-list (ark:rein#40): a queue names servers from
// config.toml, every run on it gets exactly those, and it declares each one
// as `mcp:<name>` so Elk can route work that needs one.

func declared(h *harness) []string {
	var out []string
	for _, c := range h.elk.CallsTo("heartbeat_executor") {
		if caps, ok := c.Args["declared_capabilities"].([]any); ok {
			out = out[:0]
			for _, v := range caps {
				out = append(out, v.(string))
			}
		}
	}
	return out
}

func has(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestAllowedMCPServersReachEveryRunAndAreDeclared(t *testing.T) {
	h := newHarness(t)
	t.Setenv("REIN_TEST_POSTHOG_KEY", "never-read")
	h.cfg.MCPServers = map[string]config.MCPServer{
		"posthog": {URL: "https://mcp.posthog.com/mcp", BearerTokenEnvVar: "REIN_TEST_POSTHOG_KEY"},
		"docs":    {Command: "npx", Args: []string{"-y", "docs-mcp"}},
		"figma":   {URL: "https://mcp.figma.com/mcp"}, // defined, but this queue does not allow it
	}
	h.cfg.Queues[0].MCPServers = []string{"posthog", "docs"}
	// A packet that needs one of them is served, not refused at preflight.
	h.elk.Text("claim_run", order("run-1", "mcp:posthog"))

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	specs := h.agent.Specs()
	if len(specs) != 1 {
		t.Fatalf("started %d sessions, want 1 — the packet needing mcp:posthog was refused\nlog:\n%s", len(specs), h.log)
	}
	got := specs[0].MCPServers
	if len(got) != 2 || got["posthog"] == nil || got["docs"] == nil {
		t.Fatalf("RunSpec.MCPServers = %#v, want exactly posthog and docs", got)
	}
	if _, leaked := got["figma"]; leaked {
		t.Error("a server the queue did not allow reached the run")
	}
	if u := got["posthog"].(map[string]any)["url"]; u != "https://mcp.posthog.com/mcp" {
		t.Errorf("posthog reached the adapter as %#v", got["posthog"])
	}

	caps := declared(h)
	for _, want := range []string{"mcp:posthog", "mcp:docs"} {
		if !has(caps, want) {
			t.Errorf("declared_capabilities %v is missing %s", caps, want)
		}
	}
	if has(caps, "mcp:figma") {
		t.Errorf("declared a server the queue does not allow: %v", caps)
	}
	if sub := h.submitted(); sub.Arg("status") != "ready" {
		t.Errorf("status = %q", sub.Arg("status"))
	}
	if !strings.Contains(h.log.String(), "MCP servers every run gets: docs, posthog") {
		t.Errorf("the startup log does not name the allow-list:\n%s", h.log)
	}
}

func TestAServerWhoseKeyIsMissingIsNeitherPassedNorDeclared(t *testing.T) {
	h := newHarness(t)
	h.cfg.MCPServers = map[string]config.MCPServer{
		"posthog": {URL: "https://mcp.posthog.com/mcp", BearerTokenEnvVar: "REIN_TEST_UNSET_KEY_40"},
	}
	h.cfg.Queues[0].MCPServers = []string{"posthog"}
	h.elk.Text("claim_run", order("run-1", "mcp:posthog"))

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if len(h.agent.Specs()) != 0 {
		t.Fatal("a run needing a server this machine cannot authenticate to was started anyway")
	}
	if has(declared(h), "mcp:posthog") {
		t.Error("declared mcp:posthog without the key it needs")
	}
	sub := h.submitted()
	if sub.Arg("status") != "stuck" || !strings.Contains(sub.Arg("deliverable"), "mcp:posthog") {
		t.Errorf("want a stuck run naming mcp:posthog, got %q:\n%s", sub.Arg("status"), sub.Arg("deliverable"))
	}
	if !strings.Contains(h.log.String(), "MCP server left out: posthog: this process has no REIN_TEST_UNSET_KEY_40") {
		t.Errorf("the log does not say why posthog was left out:\n%s", h.log)
	}
}

func TestAServerTheAdapterRefusesIsNeitherPassedNorDeclared(t *testing.T) {
	h := newHarness(t)
	h.agent.RefuseMCP = []string{"local"}
	h.cfg.MCPServers = map[string]config.MCPServer{
		"local":  {Command: "local-mcp"},
		"remote": {URL: "https://example.com/mcp"},
	}
	h.cfg.Queues[0].MCPServers = []string{"local", "remote"}
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	got := h.agent.Specs()[0].MCPServers
	if len(got) != 1 || got["remote"] == nil {
		t.Errorf("MCPServers = %#v, want only remote", got)
	}
	caps := declared(h)
	if has(caps, "mcp:local") || !has(caps, "mcp:remote") {
		t.Errorf("declared %v; want mcp:remote and not mcp:local", caps)
	}
	if !strings.Contains(h.log.String(), "MCP server left out: local: fake refuses") {
		t.Errorf("the log does not say why local was left out:\n%s", h.log)
	}
}

func TestAQueueWithNoAllowListGetsNoServers(t *testing.T) {
	h := newHarness(t)
	h.cfg.MCPServers = map[string]config.MCPServer{"posthog": {URL: "https://mcp.posthog.com/mcp"}}
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if got := h.agent.Specs()[0].MCPServers; got != nil {
		t.Errorf("a queue that allows nothing handed its run %#v — a definition alone must reach no run", got)
	}
	for _, c := range declared(h) {
		if strings.HasPrefix(c, "mcp:") {
			t.Errorf("declared %s with no allow-list", c)
		}
	}
}

// TestWranglerQueueDeclaresCapabilityAndNamedOwnerBinding runs once per
// spelling: `wrangler = true`, and the old `pm = true` that Elk Scout's live
// Wrangler queue still carries, which must behave identically. Either way the
// capability on the wire is `pm` — Elk's api_set_pm_executor checks that name
// — and never `wrangler`.
func TestWranglerQueueDeclaresCapabilityAndNamedOwnerBinding(t *testing.T) {
	for _, spelling := range []string{"wrangler", "pm"} {
		t.Run(spelling, func(t *testing.T) {
			h := newHarness(t)
			// Harness registers its fake adapter as claude; config validation is tested separately.
			if spelling == "wrangler" {
				h.cfg.Queues[0].Wrangler = true
				h.cfg.Queues[0].WranglerConnectorAccount = "workspace/owner"
			} else {
				h.cfg.Queues[0].PM = true
				h.cfg.Queues[0].PMConnectorAccount = "workspace/owner"
			}
			h.elk.Text("claim_run", order("run-1", "pm"))
			if err := h.run(runner.Options{}); err != nil {
				t.Fatal(err)
			}
			specs := h.agent.Specs()
			if len(specs) != 1 {
				t.Fatal("Wrangler work did not start")
			}
			if specs[0].WranglerConnectorAccount != "workspace/owner" || specs[0].MCPServers != nil {
				t.Fatal("Wrangler binding not isolated")
			}
			caps := declared(h)
			if !has(caps, "pm") {
				t.Fatal("the Wrangler's wire capability, pm, was not declared")
			}
			if has(caps, "wrangler") {
				t.Fatal("declared wrangler, which is not a capability Elk knows")
			}
			if !strings.Contains(specs[0].SystemPrompt, "docs/wrangler.md") {
				t.Fatal("Wrangler rule not used by runner")
			}
		})
	}
}

func TestWranglerQueueDefaultsItsOwnerBinding(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Wrangler = true
	h.elk.Text("claim_run", order("run-1", "pm"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatal(err)
	}
	want := h.cfg.Workspace + "/" + h.cfg.Queues[0].Name
	if got := h.agent.Specs()[0].WranglerConnectorAccount; got != want {
		t.Errorf("owner binding = %q, want the <workspace>/<queue> default %q", got, want)
	}
}

func TestOrdinaryQueueDoesNotDeclareWrangler(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	// The environment may list either name; only the opt-in declares one.
	host := runner.NewHostCapabilities(runner.EnvCapabilities,
		"elk-connector", "git", "worktree", "github-cli", "pm", "wrangler")
	if err := h.run(runner.Options{HostCapabilities: host}); err != nil {
		t.Fatal(err)
	}
	caps := declared(h)
	if has(caps, "pm") || has(caps, "wrangler") || h.agent.Specs()[0].WranglerConnectorAccount != "" {
		t.Fatalf("Wrangler enabled without the opt-in: %v", caps)
	}
}
