package grok

import (
	"errors"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

func TestMCPServerConfigTakesOnlyAPlainRemoteServer(t *testing.T) {
	a := New()
	var _ adapter.MCPConfigurer = a

	cfg, err := a.MCPServerConfig(adapter.MCPServerSpec{Name: "docs", URL: "https://docs.example.com/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	// And it survives the adapter's own conversion to the ACP array.
	servers, err := mcpServers(map[string]any{"docs": cfg})
	if err != nil {
		t.Fatalf("the translated entry is refused by mcpServers: %v", err)
	}
	entry := servers[0].(map[string]any)
	if entry["name"] != "docs" || entry["type"] != "http" || entry["url"] != "https://docs.example.com/mcp" {
		t.Errorf("ACP entry = %#v", entry)
	}

	for name, spec := range map[string]adapter.MCPServerSpec{
		"a stdio server": {Name: "local", Command: "npx"},
		"a bearer token": {Name: "posthog", URL: "https://mcp.posthog.com/mcp", BearerTokenEnvVar: "POSTHOG_API_KEY"},
	} {
		if _, err := a.MCPServerConfig(spec); !errors.Is(err, adapter.ErrNotSupported) {
			t.Errorf("%s: err = %v, want ErrNotSupported", name, err)
		}
	}
}
