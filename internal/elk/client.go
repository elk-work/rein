// Package elk is Rein's client for the Elk MCP endpoint — the only way the
// runner talks to Elk.
//
// Elk's connector (`supabase/functions/elk-mcp`) is a streamable-HTTP MCP
// server: one URL, JSON-RPC 2.0 in a POST body, `Authorization: Bearer
// <token>` for auth (D15 — header-first; the `/elk-mcp/<token>` URL-path form
// is a compat fallback and Rein never uses it). A response comes back as
// `application/json` or, if the server prefers, as a one-event
// `text/event-stream`; [Client] reads either.
//
// Three things about this server shape the client and are worth stating
// before the code:
//
//   - **A tool failure is a result, not a transport error.** MCP puts it in
//     `{"content":[…],"isError":true}` inside a 200. So [Client.CallTool]
//     returns a [*ToolResult] with [ToolResult.IsError] set and no Go error;
//     the typed wrappers in tools.go decide what an isError means for them.
//
//   - **A cancelled run is neither.** `report_progress` against a run that is
//     no longer running matches zero rows and answers with plain text and
//     `isError` unset (elk docs/rein.md §2). That is the cancellation signal,
//     and missing it means a runner keeps burning tokens on work a human
//     dropped. [Client.ReportProgress] detects it and returns [ErrCancelled].
//
//   - **Any reply may carry a one-time workspace notice.** The connector
//     prepends `📣 Workspace notice (delivered once): …` lines plus a blank
//     line to the first successful workspace-resolving reply for a token
//     (elk-mcp/handler.ts:1398-1402, bulletins.ts:189-195). It is delivered
//     exactly once, so a client that leaves it glued to the front of a work
//     order feeds it to an agent as if it were part of the packet. [Client]
//     splits it off into [ToolResult.Notices] and hands it to the logger.
//
// The client is safe for concurrent use.
package elk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion is the MCP revision Rein offers at initialize. The server
// answers with the one it chose, which [Client.ProtocolVersion] reports and
// every later request echoes in `MCP-Protocol-Version`.
const ProtocolVersion = "2025-06-18"

// DefaultTimeout bounds a single MCP call. Elk's tools are database reads and
// writes, not model calls, so this is generous rather than tuned.
const DefaultTimeout = 60 * time.Second

// ClientName is what Rein identifies itself as in the initialize handshake.
const ClientName = "rein"

// Client is one connection to an Elk MCP endpoint, holding one token.
//
// One client per (workspace, queue), because that is how tokens are scoped:
// a machine serving mac-claude and mac-codex holds two.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	version string
	logf    func(format string, args ...any)
	noInit  bool

	mu         sync.Mutex
	ready      bool
	sessionID  string
	protocol   string
	serverName string
	nextID     int64
}

// Option configures a [Client].
type Option func(*Client)

// WithHTTPClient replaces the HTTP client. The default has [DefaultTimeout]
// and no redirect surprises.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithVersion sets the version Rein reports in the initialize handshake.
func WithVersion(v string) Option {
	return func(c *Client) {
		if v != "" {
			c.version = v
		}
	}
}

// WithLogger sets where the client writes what it wants a human to see —
// today, the one-time workspace notices it strips off a reply. Notices are
// delivered once and then gone, so dropping them silently loses information
// nobody can ask for again.
func WithLogger(logf func(format string, args ...any)) Option {
	return func(c *Client) {
		if logf != nil {
			c.logf = logf
		}
	}
}

// WithoutInitialize skips the initialize handshake. The Elk connector answers
// `tools/call` without one, and a client that only ever calls tools can save
// the round trip; the handshake is still the default because it is what the
// protocol says and it is how the client learns the server's chosen protocol
// revision.
func WithoutInitialize() Option {
	return func(c *Client) { c.noInit = true }
}

// New returns a client for an Elk MCP endpoint.
//
// url is the endpoint — https://api.elk.work/functions/v1/elk-mcp — and token
// is the connector or run-scoped token it authenticates with. A URL that
// already carries a path token (the D15 compat form,
// .../elk-mcp/<token>) is accepted: the token is lifted out of the path into
// the header, because the header is the sanctioned form and a token in a URL
// ends up in logs.
func New(url, token string, opts ...Option) (*Client, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, errors.New("elk: no MCP URL")
	}
	url = strings.TrimRight(url, "/")
	if token == "" {
		if i := strings.LastIndex(url, "/elk-mcp/"); i >= 0 {
			token = url[i+len("/elk-mcp/"):]
			url = url[:i] + "/elk-mcp"
		}
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("elk: no token")
	}
	c := &Client{
		baseURL: url,
		token:   strings.TrimSpace(token),
		http:    &http.Client{Timeout: DefaultTimeout},
		version: "dev",
		logf:    func(string, ...any) {},
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// URL returns the endpoint this client posts to, with no token in it.
func (c *Client) URL() string { return c.baseURL }

// ProtocolVersion returns the MCP revision the server chose, or "" before the
// handshake has happened.
func (c *Client) ProtocolVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.protocol
}

// ServerName returns the server's self-reported name, or "" before the
// handshake.
func (c *Client) ServerName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverName
}

// ---------------------------------------------------------------- JSON-RPC

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"` // a server-initiated notification
}

// RPCError is a JSON-RPC error object. It reaches a caller when the server
// refuses the *call* — an unknown method, a malformed request, a rejected
// token — as opposed to a tool that ran and reported a problem, which arrives
// as [ToolResult.IsError].
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("elk: rpc error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("elk: rpc error %d: %s", e.Code, e.Message)
}

// The JSON-RPC codes Rein branches on. Elk uses them in a way worth writing
// down, because the obvious guess is wrong:
//
//   - **-32601** is a *method* the server does not implement — and also a
//     whole tool GROUP a deployment has not wired ("the Do queue is not
//     available"), which is what a Rein talking to an old Elk would see if
//     the queue tools were absent entirely (elk-mcp/mcp.ts:1790, 1135-1146).
//   - **-32602** is an unknown *tool* — `unknown tool: heartbeat_executor`
//     (elk-mcp/mcp.ts:1734). This, not -32601, is what a server that has not
//     shipped a tool yet answers.
//
// So "this tool does not exist here" has two codes, and treating only -32601
// as the forward-compatibility signal would make Rein fail hard on exactly
// the case it is supposed to tolerate.
const (
	// CodeMethodNotFound is JSON-RPC's -32601.
	CodeMethodNotFound = -32601
	// CodeInvalidParams is JSON-RPC's -32602, which Elk uses for an unknown
	// tool name.
	CodeInvalidParams = -32602
)

// IsMethodNotFound reports whether err is the server saying it does not
// implement a JSON-RPC method.
func IsMethodNotFound(err error) bool {
	var e *RPCError
	return errors.As(err, &e) && e.Code == CodeMethodNotFound
}

// UnknownToolError reports a tool this endpoint does not offer — because the
// build predates it, or because the deployment has not wired its tool group.
// It is the forward-compatibility signal: [Client.HeartbeatExecutor] is
// allowed to see one and carry on.
type UnknownToolError struct {
	Tool string
	Text string
}

func (e *UnknownToolError) Error() string {
	if e.Text != "" {
		return fmt.Sprintf("elk: this Elk has no %q tool: %s", e.Tool, e.Text)
	}
	return fmt.Sprintf("elk: this Elk has no %q tool", e.Tool)
}

// IsUnknownTool reports whether err means "this endpoint does not offer that
// tool". True for a [*UnknownToolError], and for the two JSON-RPC codes Elk
// answers with — -32602 `unknown tool: …` and -32601 `<group> is not
// available`.
func IsUnknownTool(err error) bool {
	var u *UnknownToolError
	if errors.As(err, &u) {
		return true
	}
	var e *RPCError
	if !errors.As(err, &e) {
		return false
	}
	switch e.Code {
	case CodeMethodNotFound:
		// A whole tool group the deployment has not wired. A genuinely
		// unimplemented JSON-RPC method says so in the message, and that is
		// not a missing tool.
		return !strings.HasPrefix(e.Message, "method not found:")
	case CodeInvalidParams:
		return strings.Contains(strings.ToLower(e.Message), "unknown tool")
	}
	return false
}

// HTTPError reports a non-2xx answer from the endpoint.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *HTTPError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 400 {
		body = body[:400] + "…"
	}
	if body == "" {
		return fmt.Sprintf("elk: %s", e.Status)
	}
	return fmt.Sprintf("elk: %s: %s", e.Status, body)
}

// Unauthorized reports whether the endpoint rejected the token. `rein enrol`
// and `rein status` say "your token was refused — re-enrol" for this, and
// something vaguer for everything else.
func (e *HTTPError) Unauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// IsUnauthorized reports whether err is the endpoint refusing the token.
func IsUnauthorized(err error) bool {
	var e *HTTPError
	return errors.As(err, &e) && e.Unauthorized()
}

func (c *Client) newID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

// post sends one JSON-RPC message. A request (id != nil) returns the raw
// result; a notification (id == nil) returns nil and tolerates an empty body,
// which is what a 202 Accepted carries.
func (c *Client) post(ctx context.Context, id *int64, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("elk: encode %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("elk: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Both, because a streamable-HTTP server chooses: Elk answers JSON today
	// and is free to answer a single SSE event tomorrow.
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", ClientName+"/"+c.version)

	c.mu.Lock()
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	if c.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", c.protocol)
	}
	c.mu.Unlock()

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elk: %s: %w", method, err)
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("elk: %s: read response: %w", method, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(raw)}
	}
	if id == nil {
		// A notification. 202 with an empty body is the expected answer; a
		// server that says something anyway is not an error.
		return nil, nil
	}

	msg, err := decodeRPC(resp.Header.Get("Content-Type"), raw, *id)
	if err != nil {
		return nil, fmt.Errorf("elk: %s: %w", method, err)
	}
	if msg.Error != nil {
		return nil, msg.Error
	}
	return msg.Result, nil
}

// decodeRPC reads a JSON-RPC response out of either an application/json body
// or a text/event-stream one, matching on id so a server that interleaves a
// notification into the stream does not confuse the answer for the question.
func decodeRPC(contentType string, raw []byte, id int64) (*rpcResponse, error) {
	mediaType := ""
	if contentType != "" {
		mt, _, err := mime.ParseMediaType(contentType)
		if err == nil {
			mediaType = mt
		}
	}
	var frames [][]byte
	if mediaType == "text/event-stream" {
		frames = sseData(raw)
		if len(frames) == 0 {
			return nil, errors.New("event stream carried no data")
		}
	} else {
		frames = [][]byte{bytes.TrimSpace(raw)}
	}

	var last error
	for _, f := range frames {
		if len(f) == 0 {
			continue
		}
		var msg rpcResponse
		if err := json.Unmarshal(f, &msg); err != nil {
			last = err
			continue
		}
		if msg.Method != "" && len(msg.ID) == 0 {
			continue // a server-initiated notification; not our answer
		}
		if len(msg.ID) == 0 || matchesID(msg.ID, id) {
			return &msg, nil
		}
	}
	if last != nil {
		return nil, fmt.Errorf("decode response: %w", last)
	}
	return nil, fmt.Errorf("no response for request id %d", id)
}

// matchesID compares a JSON-RPC id against the one we sent. Ids are numbers
// here, but a server echoing "3" instead of 3 should not strand a call.
func matchesID(raw json.RawMessage, id int64) bool {
	if len(raw) == 0 {
		return false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n == id
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == fmt.Sprint(id)
	}
	return false
}

// sseData pulls the `data:` payloads out of an SSE body, one per event.
// Multi-line data fields are joined with newlines, per the EventSource rules.
func sseData(raw []byte) [][]byte {
	var out [][]byte
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			out = append(out, []byte(strings.Join(cur, "\n")))
			cur = nil
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// a comment / keep-alive
		case strings.HasPrefix(line, "data:"):
			cur = append(cur, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	return out
}

// ---------------------------------------------------------------- handshake

// Initialize performs the MCP handshake if it has not happened. It is
// idempotent and every call path runs it first, so a caller never has to.
func (c *Client) Initialize(ctx context.Context) error {
	c.mu.Lock()
	if c.ready || c.noInit {
		c.ready = true
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	id := c.newID()
	res, err := c.post(ctx, &id, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": ClientName, "version": c.version},
	})
	if err != nil {
		// A server that does not implement initialize is stateless and takes
		// tools/call directly. Elk is one such today; treating the absence as
		// fatal would make Rein depend on a handshake it does not need.
		if IsMethodNotFound(err) {
			c.mu.Lock()
			c.ready = true
			c.mu.Unlock()
			return nil
		}
		return err
	}

	var out struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	_ = json.Unmarshal(res, &out) // a server that answers something else is still initialized

	c.mu.Lock()
	c.ready = true
	if out.ProtocolVersion != "" {
		c.protocol = out.ProtocolVersion
	}
	c.serverName = out.ServerInfo.Name
	c.mu.Unlock()

	// Best effort: the notification completes the handshake for a stateful
	// server and is ignored by a stateless one. Failing it would strand a
	// client that is otherwise working.
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		c.logf("elk: notifications/initialized: %v (continuing)", err)
	}
	return nil
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	_, err := c.post(ctx, nil, method, params)
	return err
}

// Ping round-trips the endpoint. `rein status` uses it to say whether a
// token still works without claiming anything.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.Initialize(ctx); err != nil {
		return err
	}
	id := c.newID()
	_, err := c.post(ctx, &id, "ping", map[string]any{})
	if err != nil && IsMethodNotFound(err) {
		// No ping on this server; a successful tools/list is the same proof.
		_, lerr := c.ListTools(ctx)
		return lerr
	}
	return err
}

// ---------------------------------------------------------------- tools

// Tool is one entry from `tools/list`.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ListTools returns the tools this endpoint offers. Rein uses it to tell a
// server that has not shipped `heartbeat_executor` yet from one that has,
// without probing by calling it.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}
	var out []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		id := c.newID()
		res, err := c.post(ctx, &id, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return nil, fmt.Errorf("elk: tools/list: decode: %w", err)
		}
		out = append(out, page.Tools...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// HasTool reports whether the endpoint offers a named tool.
func (c *Client) HasTool(ctx context.Context, name string) (bool, error) {
	tools, err := c.ListTools(ctx)
	if err != nil {
		return false, err
	}
	for _, t := range tools {
		if t.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// Content is one block of a tool result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolResult is what a tool answered.
//
// [ToolResult.IsError] is the tool reporting a problem, not the transport
// failing — MCP carries it inside a successful response, and Elk uses it for
// refusals a human should read. [ToolResult.Text] is every text block joined
// with blank lines, with any one-time workspace notice already lifted into
// [ToolResult.Notices].
type ToolResult struct {
	// Text is the tool's answer, notice-free.
	Text string

	// IsError is the server's own `isError` flag.
	IsError bool

	// Notices are the one-time workspace notices this reply carried, without
	// their prefix. They are delivered exactly once and never again.
	Notices []string

	// Content is every content block, verbatim, with the notice still on the
	// first one — for a transcript that should record what arrived.
	Content []Content

	// Structured is the tool's `structuredContent`, when it sent any.
	Structured json.RawMessage

	// Raw is the whole result object, for debugging.
	Raw json.RawMessage
}

// Err returns an error when the tool reported one, so a caller that wants
// isError to be fatal can write `if err := res.Err(); err != nil`.
func (r *ToolResult) Err() error {
	if !r.IsError {
		return nil
	}
	return &ToolError{Text: r.Text}
}

// ToolError is a tool that ran and reported a problem — MCP's isError.
type ToolError struct {
	Tool string
	Text string
}

func (e *ToolError) Error() string {
	if e.Tool == "" {
		return "elk: " + firstLine(e.Text)
	}
	return fmt.Sprintf("elk: %s: %s", e.Tool, firstLine(e.Text))
}

// CallTool calls one MCP tool and returns its result.
//
// It returns an error for a transport failure, a JSON-RPC error, or an unknown
// tool ([*UnknownToolError]). A tool that ran and reported a problem is NOT an
// error: it comes back with [ToolResult.IsError] set, because Elk uses that
// for refusals whose text is the answer.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*ToolResult, error) {
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	id := c.newID()
	res, err := c.post(ctx, &id, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		if IsUnknownTool(err) {
			var rpc *RPCError
			text := err.Error()
			if errors.As(err, &rpc) {
				text = rpc.Message
			}
			return nil, &UnknownToolError{Tool: name, Text: text}
		}
		return nil, err
	}

	var payload struct {
		Content           []Content       `json:"content"`
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	}
	if err := json.Unmarshal(res, &payload); err != nil {
		return nil, fmt.Errorf("elk: %s: decode result: %w", name, err)
	}

	parts := make([]string, 0, len(payload.Content))
	for _, blk := range payload.Content {
		if blk.Text != "" {
			parts = append(parts, blk.Text)
		}
	}
	text, notices := splitNotices(strings.Join(parts, "\n\n"))

	out := &ToolResult{
		Text:       text,
		IsError:    payload.IsError,
		Notices:    notices,
		Content:    payload.Content,
		Structured: payload.StructuredContent,
		Raw:        res,
	}
	for _, n := range notices {
		c.logf("elk: workspace notice (delivered once): %s", n)
	}
	return out, nil
}

// NoticePrefix is the literal the Elk connector puts in front of a one-time
// workspace notice (supabase/functions/elk-mcp/bulletins.ts:193). Notices are
// joined with "\n" and separated from the real reply by a blank line
// (handler.ts:1402).
const NoticePrefix = "📣 Workspace notice (delivered once): "

// splitNotices lifts any leading one-time workspace notices off a tool reply
// and returns the reply without them.
//
// This has to happen before anything else reads the text. The notice is
// prepended to the FIRST successful workspace-resolving reply for a token,
// which in a runner is very often the `claim_run` that fetches a work order —
// and a work order with an unrelated announcement stapled to its front is a
// work order the agent will try to act on.
//
// The shape is exactly what the connector builds: one line per notice, joined
// with "\n", then "\n\n", then the reply. A notice body is free text, so a
// body containing its own newline would produce a continuation line without
// the marker; those are folded back onto the notice above rather than
// mistaken for the reply. A body containing a *blank* line would still cut
// early — at which point the function returns the text unchanged rather than
// handing back a fragment, because a wrong split loses reply text and a
// missed split only leaves an announcement in the log.
func splitNotices(text string) (string, []string) {
	if !strings.HasPrefix(text, NoticePrefix) {
		return text, nil
	}
	block, rest, found := strings.Cut(text, "\n\n")
	if !found {
		// Notices and nothing else — the reply was empty.
		block, rest = text, ""
	}
	var notices []string
	for _, line := range strings.Split(block, "\n") {
		if after, ok := strings.CutPrefix(line, NoticePrefix); ok {
			notices = append(notices, strings.TrimSpace(after))
			continue
		}
		if len(notices) == 0 {
			return text, nil // not the shape we expected; leave it alone
		}
		notices[len(notices)-1] += "\n" + line
	}
	return rest, notices
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
