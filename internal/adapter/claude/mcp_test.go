package claude

import (
	"encoding/json"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

func TestMCPServerConfigIsAClaudeMCPConfigEntry(t *testing.T) {
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
	// The secret is a ${NAME} Claude Code expands itself; no value is in here.
	want := `{"docs":{"args":["-y","docs-mcp"],"command":"npx","env":{"DOCS_KEY":"${DOCS_KEY}"},"type":"stdio"},` +
		`"posthog":{"headers":{"Authorization":"Bearer ${POSTHOG_API_KEY}"},"type":"http","url":"https://mcp.posthog.com/mcp"}}`
	if string(got) != want {
		t.Errorf("config =\n %s\nwant\n %s", got, want)
	}

	if _, err := a.MCPServerConfig(adapter.MCPServerSpec{Name: "bad"}); err == nil {
		t.Error("an invalid spec was translated")
	}
}

func TestTheRunConnectorIsAnHTTPServerWithTheTokenByName(t *testing.T) {
	a := New()
	var _ adapter.MCPToolDenier = a
	v, err := a.MCPServerConfig(adapter.MCPServerSpec{
		Name: adapter.RunConnectorName, URL: "https://api.elk.work/functions/v1/elk-mcp",
		BearerTokenEnvVar: adapter.RunConnectorTokenEnv})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(v)
	want := `{"headers":{"Authorization":"Bearer ${REIN_ELK_TOKEN}"},"type":"http","url":"https://api.elk.work/functions/v1/elk-mcp"}`
	if string(got) != want {
		t.Errorf("run connector =\n %s\nwant\n %s", got, want)
	}
	if name, ok := a.MCPToolName("elk", "submit_deliverable"); !ok || name != "mcp__elk__submit_deliverable" {
		t.Errorf("MCPToolName = %q, %v; want mcp__elk__submit_deliverable", name, ok)
	}
}
