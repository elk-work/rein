package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPServersRoundTripFromTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[mcp_servers.posthog]
url = "https://mcp.posthog.com/mcp"
bearer_token_env_var = "POSTHOG_API_KEY"

[mcp_servers.docs]
command = "npx"
args = ["-y", "docs-mcp"]
env_vars = ["DOCS_KEY"]

[[queues]]
name = "mac-claude"
agent_kind = "claude"
mcp_servers = ["posthog", "docs"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.MCPServers["posthog"]; got.URL != "https://mcp.posthog.com/mcp" || got.BearerTokenEnvVar != "POSTHOG_API_KEY" {
		t.Errorf("posthog = %+v", got)
	}
	if got := c.MCPServers["docs"]; got.Command != "npx" || len(got.Args) != 2 || got.EnvVars[0] != "DOCS_KEY" {
		t.Errorf("docs = %+v", got)
	}
	if q := c.Queues[0]; strings.Join(q.MCPServers, ",") != "posthog,docs" {
		t.Errorf("queue allow-list = %v", q.MCPServers)
	}
}

func TestMCPServerDefinitionsAreValidated(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"neither url nor command",
			Config{MCPServers: map[string]MCPServer{"x": {}}}, "needs url"},
		{"both",
			Config{MCPServers: map[string]MCPServer{"x": {URL: "https://x", Command: "y"}}}, "pick one"},
		{"a bad name",
			Config{MCPServers: map[string]MCPServer{"no spaces": {URL: "https://x"}}}, "server name"},
		{"a queue allowing an undefined server",
			Config{Queues: []Queue{{Name: "mac-codex", AgentKind: "codex", MCPServers: []string{"figma"}}}},
			`names "figma", which has no [mcp_servers.figma]`},
	} {
		err := tc.cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want one mentioning %q", tc.name, err, tc.wantErr)
		}
	}
}
