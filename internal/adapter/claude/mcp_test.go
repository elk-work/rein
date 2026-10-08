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
