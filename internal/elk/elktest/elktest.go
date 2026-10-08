// Package elktest is a fake Elk MCP endpoint for tests.
//
// It is an [httptest.Server] speaking the same wire contract the real
// connector does — JSON-RPC 2.0 over one POST URL, `Authorization: Bearer`,
// `{"content":[{"type":"text","text":…}],"isError":…}` results, `isError`
// omitted when false — so a test drives [github.com/elk-work/rein/internal/elk]
// through the real HTTP path rather than a stubbed interface. It lives in its
// own package rather than a _test.go file because the run loop needs it too.
//
// The default is a server that answers every tool with empty text. A test
// scripts the tools it cares about with [Server.Handle] or [Server.Reply], and
// asserts on [Server.Calls].
package elktest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Reply is what a scripted tool answers.
type Reply struct {
	// Text is the tool result's single text block.
	Text string

	// IsError sets MCP's isError. The real server omits the field entirely
	// when false, and so does this one — a client that reads `isError: false`
	// as meaningful should not pass here.
	IsError bool

	// RPCError, when set, makes the call answer with a JSON-RPC error instead
	// of a result. Use [UnknownTool] for the shape Elk uses for a tool it does
	// not have.
	RPCError *RPCError
}

// RPCError is a JSON-RPC error to answer with.
type RPCError struct {
	Code    int
	Message string
}

// UnknownTool is the answer Elk gives for a tool name it does not know:
// JSON-RPC -32602, message `unknown tool: <name>` (elk-mcp/mcp.ts:1734). It is
// what a Rein built against a newer contract sees from an older deployment.
func UnknownTool(name string) Reply {
	return Reply{RPCError: &RPCError{Code: -32602, Message: "unknown tool: " + name}}
}

// ToolGroupMissing is the other absence Elk has: a whole tool group a
// deployment has not wired, answered as -32601 with the group's own sentence.
func ToolGroupMissing(what string) Reply {
	return Reply{RPCError: &RPCError{Code: -32601, Message: what + " is not available"}}
}

// Handler answers one tool call.
type Handler func(args map[string]any) Reply

// Call records one tool invocation.
type Call struct {
	Tool string
	Args map[string]any
}

// Arg returns a string argument, or "".
func (c Call) Arg(name string) string {
	s, _ := c.Args[name].(string)
	return s
}

// Server is a fake Elk MCP endpoint.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	token    string
	handlers map[string]Handler
	calls    []Call
	notice   string
	sse      bool
	inits    int
}

// New starts a fake endpoint requiring the given bearer token. It is stopped
// automatically when the test ends.
func New(t *testing.T, token string) *Server {
	t.Helper()
	s := &Server{token: token, handlers: map[string]Handler{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Handle scripts a tool with a function.
func (s *Server) Handle(tool string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[tool] = h
}

// Reply scripts a tool with a fixed answer.
func (s *Server) Reply(tool string, r Reply) {
	s.Handle(tool, func(map[string]any) Reply { return r })
}

// Text scripts a tool with a fixed successful text answer.
func (s *Server) Text(tool, text string) { s.Reply(tool, Reply{Text: text}) }

// SetNotice arms a one-time workspace notice, prepended to the next successful
// tool reply exactly as the connector does it — the marker line, then a blank
// line, then the reply (elk-mcp/handler.ts:1402).
func (s *Server) SetNotice(body, author, date string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notice = fmt.Sprintf("📣 Workspace notice (delivered once): %s — posted by %s %s", body, author, date)
}

// UseSSE makes every response a one-event `text/event-stream` instead of
// `application/json`. Elk answers JSON today; this exercises the client's
// tolerance of the other half of streamable HTTP.
func (s *Server) UseSSE(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sse = on
}

// Calls returns every tool call the server has served, in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// CallsTo returns the calls to one tool.
func (s *Server) CallsTo(tool string) []Call {
	var out []Call
	for _, c := range s.Calls() {
		if c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

// Initializes counts the initialize handshakes the server has answered — the
// assertion behind "the client handshakes once, not once per call".
func (s *Server) Initializes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inits
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "method_not_allowed"})
		return
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got != s.token {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}

	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name            string         `json:"name"`
			Arguments       map[string]any `json:"arguments"`
			ProtocolVersion string         `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		writeJSON(w, 400, map[string]any{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32700, "message": "parse error"},
		})
		return
	}
	if len(msg.ID) == 0 {
		// A notification. The real server answers 202 with no body.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch msg.Method {
	case "initialize":
		s.mu.Lock()
		s.inits++
		s.mu.Unlock()
		version := msg.Params.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		s.result(w, msg.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}, "prompts": map[string]any{}},
			"serverInfo":      map[string]any{"name": "elk", "version": "0.1.0"},
		})
	case "ping":
		s.result(w, msg.ID, map[string]any{})
	case "tools/list":
		s.mu.Lock()
		tools := make([]map[string]any, 0, len(s.handlers))
		for name := range s.handlers {
			tools = append(tools, map[string]any{"name": name})
		}
		s.mu.Unlock()
		s.result(w, msg.ID, map[string]any{"tools": tools})
	case "tools/call":
		s.callTool(w, msg.ID, msg.Params.Name, msg.Params.Arguments)
	default:
		s.rpcError(w, msg.ID, -32601, "method not found: "+msg.Method)
	}
}

func (s *Server) callTool(w http.ResponseWriter, id json.RawMessage, name string, args map[string]any) {
	if args == nil {
		args = map[string]any{}
	}
	s.mu.Lock()
	s.calls = append(s.calls, Call{Tool: name, Args: args})
	h, ok := s.handlers[name]
	s.mu.Unlock()

	var rep Reply
	if ok {
		rep = h(args)
	}
	if rep.RPCError != nil {
		s.rpcError(w, id, rep.RPCError.Code, rep.RPCError.Message)
		return
	}

	text := rep.Text
	if !rep.IsError {
		s.mu.Lock()
		if s.notice != "" {
			text = s.notice + "\n\n" + text
			s.notice = "" // delivered once
		}
		s.mu.Unlock()
	}
	out := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	if rep.IsError {
		// Matches the real server: the key is present only when true.
		out["isError"] = true
	}
	s.result(w, id, out)
}

func (s *Server) result(w http.ResponseWriter, id json.RawMessage, result any) {
	s.write(w, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

func (s *Server) rpcError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	s.write(w, map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(id),
		"error": map[string]any{"code": code, "message": message},
	})
}

func (s *Server) write(w http.ResponseWriter, body any) {
	s.mu.Lock()
	sse := s.sse
	s.mu.Unlock()
	if !sse {
		writeJSON(w, 200, body)
		return
	}
	raw, err := json.Marshal(body)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fmt.Fprintf(w, ": keep-alive\n\nevent: message\ndata: %s\n\n", raw)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
