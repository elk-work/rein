//go:build integration

package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/codex"
)

// This uses a real Codex thread and MCP client, but no model turn, login or
// external service. All configuration and history live in the test directory.
func TestIntegrationCodexRunConnector(t *testing.T) {
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not installed")
	}
	var mu sync.Mutex
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-test-token" {
			t.Error("upstream did not receive the proxy credential")
			w.WriteHeader(401)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake-elk", "version": "1"}}
		case "tools/list":
			var list []any
			for _, name := range append([]string{"list_actions", "client_disabled"}, adapter.RunConnectorDeniedTools...) {
				list = append(list, map[string]any{"name": name, "description": "fake tool", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
			}
			result = map[string]any{"tools": list}
		case "tools/call":
			mu.Lock()
			calls = append(calls, req.Params.Name)
			mu.Unlock()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "fake actions"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer upstream.Close()
	a := codex.New()
	var l mcpAllowList
	l.addRunConnector(upstream.URL, a)
	if !l.connector {
		t.Fatalf("connector refused: %v", l.skipped)
	}
	qr := &queueRunner{r: &Runner{}, mcp: l, elkToken: "fake-test-token"}
	qr.r.opts.Config.Elk.MCPURL = upstream.URL
	spec := adapter.RunSpec{}
	proxy, err := qr.startRunConnector(&spec)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	cfg := spec.MCPServers["elk"].(map[string]any)
	// The replay test checks that the adapter supplies these unqualified names.
	cfg["disabled_tools"] = append([]string{"client_disabled"}, adapter.RunConnectorDeniedTools...)

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "codex"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "app-server", "-c", "features.apps=false")
	cmd.Dir = dir
	// Deliberately no inherited home, credentials, or personal config.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CODEX_HOME=" + filepath.Join(dir, "codex")}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cancel(); _ = cmd.Wait() }()
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 4096), 4<<20)
	id := 0
	rpc := func(method string, params any) map[string]json.RawMessage {
		t.Helper()
		id++
		if err := json.NewEncoder(stdin).Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		for scan.Scan() {
			var msg map[string]json.RawMessage
			if json.Unmarshal(scan.Bytes(), &msg) != nil {
				continue
			}
			var got int
			_ = json.Unmarshal(msg["id"], &got)
			if got == id && msg["method"] == nil {
				return msg
			}
		}
		t.Fatalf("Codex stopped answering %s: %v", method, scan.Err())
		return nil
	}
	requireOK := func(msg map[string]json.RawMessage) json.RawMessage {
		t.Helper()
		if e := msg["error"]; len(e) > 0 {
			t.Fatalf("Codex RPC error: %s", e)
		}
		return msg["result"]
	}
	requireOK(rpc("initialize", map[string]any{"clientInfo": map[string]any{"name": "rein-test", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}))
	_ = json.NewEncoder(stdin).Encode(map[string]any{"method": "initialized"})
	raw := requireOK(rpc("thread/start", map[string]any{"cwd": dir, "approvalPolicy": "never", "sandbox": "read-only", "config": map[string]any{"mcp_servers": spec.MCPServers}}))
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.Thread.ID == "" {
		t.Fatal("no thread ID")
	}
	raw = requireOK(rpc("mcpServerStatus/list", map[string]any{"threadId": started.Thread.ID, "serverName": "elk"}))
	var inventory struct {
		Data []struct {
			Name  string                     `json:"name"`
			Tools map[string]json.RawMessage `json:"tools"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory.Data) != 1 || inventory.Data[0].Name != "elk" {
		t.Fatal("expected only elk inventory")
	}
	tools := inventory.Data[0].Tools
	if _, ok := tools["list_actions"]; !ok {
		t.Fatalf("missing read tool; inventory: %s", raw)
	}
	for _, name := range append([]string{"client_disabled"}, adapter.RunConnectorDeniedTools...) {
		if _, ok := tools[name]; ok {
			t.Errorf("offered denied tool %s", name)
		}
	}
	params := map[string]any{"threadId": started.Thread.ID, "server": "elk", "tool": "list_actions", "arguments": map[string]any{}}
	raw = requireOK(rpc("mcpServer/tool/call", params))
	if !strings.Contains(string(raw), "fake actions") {
		t.Fatal("allowed call did not answer")
	}
	params["tool"] = "claim_run"
	denied := rpc("mcpServer/tool/call", params)
	if len(denied["error"]) == 0 && !strings.Contains(string(denied["result"]), `"isError":true`) {
		t.Fatal("Codex allowed denied call")
	}
	// Even bypassing the client's filtered tool map cannot reach upstream.
	_, body := post(t, proxy.URL(), `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"claim_run"}}`)
	if !strings.Contains(body, `"error"`) {
		t.Fatal("proxy allowed claim_run")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "list_actions" {
		t.Fatalf("unexpected upstream calls: %v", calls)
	}
}
