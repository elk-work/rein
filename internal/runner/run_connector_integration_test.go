//go:build integration

package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/claude"
)

// TestIntegrationRunConnectorThroughTheProxy is ark:rein#112 end to end on
// the real Claude Code binary:
//
//	go test -tags integration -run TestIntegrationRunConnectorThroughTheProxy ./internal/runner/
//
// The session is given only the proxy's loopback address. Elk (a local fake
// that answers only the queue's token) sees the queue's token on every call;
// the session's tool list has Elk's ordinary tools and none of Rein's own.
func TestIntegrationRunConnectorThroughTheProxy(t *testing.T) {
	a := claude.New()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("no claude on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Preflight(ctx); err != nil {
		t.Skipf("preflight: %v", err)
	}
	const tok = "rein-integration-queue-token"
	var mu sync.Mutex
	var authd, other int
	elk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Header.Get("Authorization") == "Bearer "+tok {
			authd++
		} else {
			other++
			mu.Unlock()
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &m)
		if len(m.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch m.Method {
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
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	}))
	defer elk.Close()

	var l mcpAllowList
	l.addRunConnector(elk.URL, a)
	if !l.connector {
		t.Fatalf("the claude adapter was refused the run connector: %v", l.skipped)
	}
	qr := &queueRunner{r: &Runner{}, elkToken: tok, mcp: l}
	qr.r.opts.Config.Elk.MCPURL = elk.URL
	spec := adapter.RunSpec{
		RunID:          "integration-run-connector",
		WorktreeDir:    t.TempDir(),
		Prompt:         "Reply with exactly the two words: REIN OK",
		PermissionMode: adapter.PermissionReadOnly,
		DeniedTools:    l.denied,
	}
	proxy, err := qr.startRunConnector(&spec)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	blob, _ := json.Marshal(spec)
	if strings.Contains(string(blob), tok) {
		t.Fatal("the queue's token is in the RunSpec handed to the adapter")
	}
	if err := os.WriteFile(spec.WorktreeDir+"/README", []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sess, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var initRaw json.RawMessage
	for ev := range sess.Events() {
		if ev.Kind == adapter.EventProgress && len(ev.Raw) > 0 && strings.Contains(string(ev.Raw), `"subtype":"init"`) {
			initRaw = ev.Raw
		}
	}
	_, _ = sess.Wait()
	var init struct {
		MCPServers []struct{ Name, Status string } `json:"mcp_servers"`
		Tools      []string                        `json:"tools"`
	}
	if err := json.Unmarshal(initRaw, &init); err != nil {
		t.Fatalf("no system/init: %v", err)
	}
	if len(init.MCPServers) != 1 || init.MCPServers[0].Name != "elk" || init.MCPServers[0].Status != "connected" {
		t.Fatalf("want exactly elk, connected; got %+v", init.MCPServers)
	}
	has := func(n string) bool {
		for _, v := range init.Tools {
			if v == n {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"mcp__elk__list_actions", "mcp__elk__create_action"} {
		if !has(want) {
			t.Errorf("the agent cannot see %s: %v", want, init.Tools)
		}
	}
	for _, bad := range []string{"mcp__elk__submit_deliverable", "mcp__elk__claim_run", "mcp__elk__rotate_own_token"} {
		if has(bad) {
			t.Errorf("the agent is offered Rein's own tool %s", bad)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if authd == 0 || other != 0 {
		t.Errorf("Elk saw %d calls with the queue's token and %d without", authd, other)
	}
}
