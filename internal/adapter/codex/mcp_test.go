package codex

import (
	"context"
	"encoding/json"
	"errors"
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

func TestRunConnectorDenialsReachThreadStart(t *testing.T) {
	a, ref := replayAdapter(t, "pong.jsonl")
	var _ adapter.MCPToolDenier = a
	cfg, _ := a.MCPServerConfig(adapter.MCPServerSpec{Name: "elk", URL: "http://127.0.0.1:1234/test"})
	original := cfg.(map[string]any)
	original["disabled_tools"] = []string{"already_disabled"}
	spec := baseSpec(adapter.PermissionFull)
	spec.MCPServers = map[string]any{"elk": cfg}
	for _, tool := range adapter.RunConnectorDeniedTools {
		name, ok := a.MCPToolName("elk", tool)
		if !ok || name != "mcp__elk__"+tool {
			t.Fatalf("tool name: %q, %v", name, ok)
		}
		spec.DeniedTools = append(spec.DeniedTools, name)
	}
	sess, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, sess)
	call, ok := ref.clientCall(t, methodThreadStart)
	if !ok {
		t.Fatal("no thread/start")
	}
	var params struct {
		Config struct {
			Servers map[string]struct {
				URL      string   `json:"url"`
				Disabled []string `json:"disabled_tools"`
			} `json:"mcp_servers"`
		} `json:"config"`
	}
	if err := json.Unmarshal(call.Params, &params); err != nil {
		t.Fatal(err)
	}
	elk := params.Config.Servers["elk"]
	if elk.URL != original["url"] || len(elk.Disabled) != len(adapter.RunConnectorDeniedTools)+1 {
		t.Fatalf("connector config = %+v", elk)
	}
	for i, tool := range adapter.RunConnectorDeniedTools {
		if elk.Disabled[i+1] != tool {
			t.Errorf("missing denial %s", tool)
		}
	}
	if got := original["disabled_tools"].([]string); len(got) != 1 || got[0] != "already_disabled" {
		t.Fatal("mutated shared server configuration")
	}
}

func TestUnsupportedToolRestrictionsStillRefused(t *testing.T) {
	for _, name := range []string{"shell", "mcp__missing__claim_run", "mcp__elk__", "mcp__elk__bad.name", "mcp__elk__" + strings.Repeat("x", 65)} {
		t.Run(name, func(t *testing.T) {
			spec := baseSpec(adapter.PermissionFull)
			spec.MCPServers = map[string]any{"elk": map[string]any{"url": "http://127.0.0.1:1"}}
			spec.DeniedTools = []string{name}
			if _, err := New().Start(context.Background(), spec); !errors.Is(err, adapter.ErrNotSupported) {
				t.Fatalf("Start = %v, want ErrNotSupported", err)
			}
		})
	}
}

func TestMCPToolNamesRefuseAmbiguousOrRewrittenNames(t *testing.T) {
	for _, pair := range [][2]string{
		{"", "tool"}, {"server__part", "tool"}, {"server_", "tool"},
		{"elk", ""}, {"elk", "tool.with.dot"}, {"elk", strings.Repeat("x", 55)},
	} {
		if _, ok := New().MCPToolName(pair[0], pair[1]); ok {
			t.Errorf("accepted unrepresentable name %q %q", pair[0], pair[1])
		}
	}
}
