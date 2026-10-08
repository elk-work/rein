package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

func TestMCPServerConfigIsACodexMCPServersTable(t *testing.T) {
	a := New()
	var _ adapter.MCPConfigurer = a

	remote, err := a.MCPServerConfig(adapter.MCPServerSpec{
		Name: "posthog", URL: "https://mcp.posthog.com/mcp", BearerTokenEnvVar: "POSTHOG_API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	local, err := a.MCPServerConfig(adapter.MCPServerSpec{
		Name: "docs", Command: "npx", Args: []string{"-y", "docs-mcp"}, EnvVars: []string{"DOCS_KEY"}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(map[string]any{"posthog": remote, "docs": local})
	want := `{"docs":{"args":["-y","docs-mcp"],"command":"npx","env_vars":["DOCS_KEY"]},` +
		`"posthog":{"bearer_token_env_var":"POSTHOG_API_KEY","url":"https://mcp.posthog.com/mcp"}}`
	if string(got) != want {
		t.Errorf("config =\n %s\nwant\n %s", got, want)
	}
}

// The translated table is what thread/start carries, under config.mcp_servers.
func TestAllowListedServersReachThreadStart(t *testing.T) {
	a, ref := replayAdapter(t, "pong.jsonl")
	cfg, err := a.MCPServerConfig(adapter.MCPServerSpec{
		Name: "posthog", URL: "https://mcp.posthog.com/mcp", BearerTokenEnvVar: "POSTHOG_API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	spec := baseSpec(adapter.PermissionFull)
	spec.MCPServers = map[string]any{"posthog": cfg}
	sess, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, sess)
	call, ok := ref.clientCall(t, methodThreadStart)
	if !ok {
		t.Fatal("no thread/start")
	}
	if !strings.Contains(string(call.Params),
		`"mcp_servers":{"posthog":{"bearer_token_env_var":"POSTHOG_API_KEY","url":"https://mcp.posthog.com/mcp"}}`) {
		t.Errorf("thread/start params = %s", call.Params)
	}
}
