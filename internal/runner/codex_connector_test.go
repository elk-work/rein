package runner

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/codex"
	"github.com/elk-work/rein/internal/config"
)

func TestCodexRunConnectorIsDeclaredAndProxied(t *testing.T) {
	a := codex.New()
	cfg := config.Config{MCPServers: map[string]config.MCPServer{
		"elk": {URL: "https://example.invalid/personal"},
	}}
	l := resolveMCP(cfg, config.Queue{MCPServers: []string{"elk"}}, a)
	if len(l.servers) != 0 {
		t.Fatal("personal elk server was not reserved")
	}
	l.addRunConnector("https://example.invalid/upstream", a)
	if !l.connector || !slices.Contains(l.declared, "mcp:elk") {
		t.Fatalf("Codex connector not declared: %v", l.skipped)
	}
	for _, tool := range adapter.RunConnectorDeniedTools {
		if !slices.Contains(l.denied, "mcp__elk__"+tool) {
			t.Errorf("missing denial %s", tool)
		}
	}
	up := &fakeElk{}
	upstream := httptest.NewServer(up)
	defer upstream.Close()
	qr := &queueRunner{r: &Runner{}, mcp: l, elkToken: "test-only-token"}
	qr.r.opts.Config.Elk.MCPURL = upstream.URL
	spec := adapter.RunSpec{MCPServers: qr.mcpServersForRun(), DeniedTools: l.denied}
	proxy, err := qr.startRunConnector(&spec)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	cfgServer := spec.MCPServers["elk"].(map[string]any)
	if cfgServer["url"] != proxy.URL() || len(cfgServer) != 1 {
		t.Fatal("connector is not a plain per-run proxy URL")
	}
	_, body := post(t, proxy.URL(), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(body, "list_actions") || strings.Contains(body, "submit_deliverable") {
		t.Fatal("wrong filtered inventory")
	}
	_, body = post(t, proxy.URL(), `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"claim_run"}}`)
	if !strings.Contains(body, "error") {
		t.Fatal("denied call was accepted")
	}
}
