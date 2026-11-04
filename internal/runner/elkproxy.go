package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// The Elk run connector's proxy (ark:rein#112).
//
// A run reaches Elk as its queue, on the queue's own run-scoped token. That is
// the token Rein claims and reports with, so the session must never hold it:
// with the token in the agent's environment, any shell command — the agent's
// own curl, a test script, a package's install hook — could call
// submit_deliverable, claim_run or rotate_own_token directly, and a deny list
// on the agent's MCP client stops only a well-behaved model (the review of
// elk-work/rein#52). So Rein holds the token and the session gets a loopback
// address instead: this proxy, one per run, listening on 127.0.0.1 behind a
// path secret that dies with the run. It forwards every MCP request to Elk with
// the queue's token, refuses a tools/call naming one of Rein's own tools, and
// leaves those tools out of tools/list. The refusal is enforced here, outside
// the session, whatever the session does.

// maxProxyBody bounds a request or response the proxy will buffer. Elk's MCP
// answers are JSON documents well under this; a larger one is refused rather
// than streamed past the filter.
const maxProxyBody = 8 << 20

// elkProxy is one run's loopback Elk connector.
type elkProxy struct {
	upstream string
	token    string
	denied   map[string]bool
	secret   string
	srv      *http.Server
	ln       net.Listener
	client   *http.Client
}

// startElkProxy listens on a fresh loopback port. upstream is the Elk MCP
// endpoint, token the queue's own, denied the bare tool names to refuse.
func startElkProxy(upstream, token string, denied []string) (*elkProxy, error) {
	if upstream == "" || token == "" {
		return nil, errors.New("no Elk endpoint or no queue token")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &elkProxy{
		upstream: upstream,
		token:    token,
		denied:   map[string]bool{},
		secret:   base64.RawURLEncoding.EncodeToString(b),
		ln:       ln,
		client:   &http.Client{Timeout: 5 * time.Minute},
	}
	for _, d := range denied {
		p.denied[d] = true
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// URL is the address the session is given. It carries no credential of Elk's:
// its path secret opens this proxy only, and only while the run lasts.
func (p *elkProxy) URL() string {
	return "http://" + p.ln.Addr().String() + "/" + p.secret
}

// Close stops the proxy; the URL stops answering at once.
func (p *elkProxy) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if p.srv.Shutdown(ctx) != nil {
		// A call still in flight does not outlive the run.
		_ = p.srv.Close()
	}
}

// rpcMessage is what the proxy reads of a JSON-RPC message. It is decoded
// from exact keys (rpcFrom), never through a struct: Go matches struct fields
// case-insensitively, so {"name":"submit_deliverable","Name":"list_actions"}
// would read as list_actions here while Elk, which reads `name` exactly, runs
// submit_deliverable. For a key that appears twice both sides take the last.
type rpcMessage struct {
	ID     json.RawMessage
	Method string
	Tool   string
}

func rpcFrom(raw json.RawMessage) (rpcMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return rpcMessage{}, false
	}
	m := rpcMessage{ID: obj["id"]}
	if v, ok := obj["method"]; ok && json.Unmarshal(v, &m.Method) != nil {
		return rpcMessage{}, false
	}
	if v, ok := obj["params"]; ok && m.Method == "tools/call" {
		var params map[string]json.RawMessage
		if json.Unmarshal(v, &params) != nil {
			return rpcMessage{}, false
		}
		if n, ok := params["name"]; ok && json.Unmarshal(n, &m.Tool) != nil {
			return rpcMessage{}, false
		}
	}
	return m, true
}

func (p *elkProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/"+p.secret {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		// Elk's MCP is POST only; a GET for a server-sent stream is refused
		// the way Elk itself refuses it.
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProxyBody+1))
	if err != nil || len(body) > maxProxyBody {
		http.Error(w, `{"error":"request too large or unreadable"}`, http.StatusRequestEntityTooLarge)
		return
	}

	msgs, ok := parseRPC(body)
	if !ok {
		// Not JSON-RPC Rein can read, so it cannot be shown to be harmless.
		http.Error(w, `{"error":"unreadable JSON-RPC request"}`, http.StatusBadRequest)
		return
	}
	listsTools := false
	for _, m := range msgs {
		if m.Method == "tools/list" {
			listsTools = true
		}
		if m.Method == "tools/call" && p.denied[m.Tool] {
			// One refused call refuses the whole request: nothing in it is
			// forwarded, so a batch cannot smuggle a denied call past.
			writeRPCError(w, m.ID, m.Tool)
			return
		}
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.upstream, bytes.NewReader(body))
	if err != nil {
		http.Error(w, `{"error":"bad upstream"}`, http.StatusBadGateway)
		return
	}
	for _, h := range []string{"Content-Type", "Accept", "Mcp-Session-Id", "Mcp-Protocol-Version"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		http.Error(w, `{"error":"Elk did not answer"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyBody+1))
	if err != nil || len(out) > maxProxyBody {
		http.Error(w, `{"error":"Elk's answer was too large or unreadable"}`, http.StatusBadGateway)
		return
	}
	status := resp.StatusCode
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// The queue's token was refused. A 401 with Elk's challenge would send
		// the agent's MCP client into a sign-in flow against this proxy; say
		// what happened instead.
		status = http.StatusBadGateway
		out = []byte(`{"error":"Elk refused this queue's credential; Rein's own claims will fail the same way"}`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(out)
		return
	}
	if listsTools {
		out = p.filterToolsList(out, resp.Header.Get("Content-Type"))
	}
	for _, h := range []string{"Content-Type", "Mcp-Session-Id"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// parseRPC reads one JSON-RPC message or a batch. ok is false when the body
// is neither, or any message in it cannot be read.
func parseRPC(body []byte) ([]rpcMessage, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, false
	}
	var raws []json.RawMessage
	if trimmed[0] == '[' {
		if json.Unmarshal(trimmed, &raws) != nil {
			return nil, false
		}
	} else {
		raws = []json.RawMessage{trimmed}
	}
	out := make([]rpcMessage, 0, len(raws))
	for _, raw := range raws {
		m, ok := rpcFrom(raw)
		if !ok {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, tool string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	blob, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code": -32601,
			"message": "Rein reports this run, so " + tool + " is not available to it. " +
				"Finish the task and say what you did in your final message.",
		},
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(blob)
}

// filterToolsList drops Rein's own tools from a tools/list answer, in JSON or
// in a server-sent event's data lines. Anything it cannot parse is returned
// as it came: the call refusal above holds either way.
func (p *elkProxy) filterToolsList(out []byte, contentType string) []byte {
	if strings.HasPrefix(contentType, "text/event-stream") {
		lines := strings.Split(string(out), "\n")
		for i, line := range lines {
			if data, ok := strings.CutPrefix(line, "data:"); ok {
				lines[i] = "data: " + string(p.filterMessages([]byte(strings.TrimSpace(data))))
			}
		}
		return []byte(strings.Join(lines, "\n"))
	}
	return p.filterMessages(out)
}

func (p *elkProxy) filterMessages(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw
	}
	if trimmed[0] == '[' {
		var batch []map[string]any
		if json.Unmarshal(trimmed, &batch) != nil {
			return raw
		}
		for _, m := range batch {
			p.filterOne(m)
		}
		b, err := json.Marshal(batch)
		if err != nil {
			return raw
		}
		return b
	}
	var m map[string]any
	if json.Unmarshal(trimmed, &m) != nil {
		return raw
	}
	p.filterOne(m)
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

func (p *elkProxy) filterOne(m map[string]any) {
	result, ok := m["result"].(map[string]any)
	if !ok {
		return
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		return
	}
	kept := tools[:0]
	for _, t := range tools {
		if tm, ok := t.(map[string]any); ok {
			if name, _ := tm["name"].(string); p.denied[name] {
				continue
			}
		}
		kept = append(kept, t)
	}
	result["tools"] = kept
}
