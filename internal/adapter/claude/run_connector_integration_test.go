//go:build integration

package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// fakeElkMCP is a streamable-HTTP MCP server offering a few of Elk's tools,
// Rein's own among them, that answers only the bearer token it was given. It
// records whether a call carried that token.
type fakeElkMCP struct {
	token string
	mu    sync.Mutex
	authd int
	other int
}

func (f *fakeElkMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f.mu.Lock()
	ok := r.Header.Get("Authorization") == "Bearer "+f.token
	if ok {
		f.authd++
	} else {
		f.other++
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.NewDecoder(r.Body).Decode(&msg)
	if len(msg.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch msg.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "fake-elk", "version": "1"}}
	case "tools/list":
		var tools []any
		for _, name := range []string{"list_actions", "create_action", "submit_deliverable", "claim_run", "rotate_own_token"} {
			tools = append(tools, map[string]any{"name": name, "description": "fake " + name,
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
		}
		result = map[string]any{"tools": tools}
	default:
		result = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
}

// TestIntegrationRunConnectorOffersElkWithoutReinsOwnTools is ark:rein#112
// against the real binary: the run connector arrives as exactly the server elk,
// its bearer token reaches the server from the session's environment (never a
// file or an argument), and Rein's own tools on it are not offered to the
// agent. Offering them is the other half of ark:rein#21, where a run posted
// its own deliverable to Elk before Rein did.
func TestIntegrationRunConnectorOffersElkWithoutReinsOwnTools(t *testing.T) {
	a := requireClaude(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Preflight(ctx); err != nil {
		t.Skipf("preflight: %v", err)
	}
	const tok = "rein-integration-queue-token"
	srv := &fakeElkMCP{token: tok}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	server, err := a.MCPServerConfig(adapter.MCPServerSpec{
		Name: adapter.RunConnectorName, URL: ts.URL, BearerTokenEnvVar: adapter.RunConnectorTokenEnv})
	if err != nil {
		t.Fatal(err)
	}
	var denied []string
	for _, tool := range adapter.RunConnectorDeniedTools {
		name, _ := a.MCPToolName(adapter.RunConnectorName, tool)
		denied = append(denied, name)
	}
	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID:          "integration-run-connector",
		WorktreeDir:    tempRepo(t),
		Prompt:         "Reply with exactly the two words: REIN OK",
		PermissionMode: adapter.PermissionReadOnly,
		MCPServers:     map[string]any{adapter.RunConnectorName: server},
		DeniedTools:    denied,
		Env:            map[string]string{adapter.RunConnectorTokenEnv: tok},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs, _, _ := collect(t, sess, 5*time.Minute)
	init := findInit(t, evs)

	if len(init.MCPServers) != 1 || init.MCPServers[0].Name != adapter.RunConnectorName {
		t.Fatalf("want exactly the elk server, got %+v", init.MCPServers)
	}
	if init.MCPServers[0].Status != "connected" {
		t.Errorf("the elk server did not connect: %+v", init.MCPServers[0])
	}
	var elkTools []string
	for _, name := range init.Tools {
		if strings.HasPrefix(name, "mcp__elk__") {
			elkTools = append(elkTools, name)
		}
	}
	for _, want := range []string{"mcp__elk__list_actions", "mcp__elk__create_action"} {
		if !contains(elkTools, want) {
			t.Errorf("the agent cannot see %s: %v", want, elkTools)
		}
	}
	for _, bad := range []string{"mcp__elk__submit_deliverable", "mcp__elk__claim_run", "mcp__elk__rotate_own_token"} {
		if contains(elkTools, bad) {
			t.Errorf("the agent is offered Rein's own tool %s", bad)
		}
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.authd == 0 {
		t.Errorf("no call carried the queue's token (%d calls without it)", srv.other)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
