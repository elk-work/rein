package elk_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/elk/elktest"
)

const testToken = "tok-abc123"

func newClient(t *testing.T, s *elktest.Server, opts ...elk.Option) *elk.Client {
	t.Helper()
	c, err := elk.New(s.URL, testToken, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCallToolRoundTrip(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Handle("list_workspaces", func(args map[string]any) elktest.Reply {
		return elktest.Reply{Text: "Elk Scout"}
	})
	c := newClient(t, s)

	res, err := c.CallTool(context.Background(), "list_workspaces", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Elk Scout" {
		t.Errorf("text = %q", res.Text)
	}
	if res.IsError {
		t.Error("isError set on a plain result")
	}
	if got := s.Initializes(); got != 1 {
		t.Errorf("handshakes = %d, want exactly 1 — the client must not re-handshake per call", got)
	}
	// A second call reuses the handshake.
	if _, err := c.CallTool(context.Background(), "list_workspaces", nil); err != nil {
		t.Fatal(err)
	}
	if got := s.Initializes(); got != 1 {
		t.Errorf("handshakes = %d after two calls, want 1", got)
	}
}

func TestAuthorizationHeaderIsTheBearerToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		var msg struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"result":{}}`))
	}))
	defer srv.Close()

	c, err := elk.New(srv.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := "Bearer " + testToken; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

func TestNewLiftsAPathTokenIntoTheHeader(t *testing.T) {
	// The connector URL people paste is the D15 compat form. Rein accepts it
	// and moves the token to the header, because a token in a URL ends up in
	// logs.
	c, err := elk.New("https://api.elk.work/functions/v1/elk-mcp/"+testToken, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://api.elk.work/functions/v1/elk-mcp"; c.URL() != want {
		t.Errorf("URL = %q, want %q", c.URL(), want)
	}
}

func TestUnauthorized(t *testing.T) {
	s := elktest.New(t, "the-right-token")
	c := newClient(t, s) // holds the wrong one

	_, err := c.CallTool(context.Background(), "list_workspaces", nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !elk.IsUnauthorized(err) {
		t.Errorf("IsUnauthorized(%v) = false", err)
	}
}

func TestIsErrorIsAResultNotAnError(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Reply("claim_run", elktest.Reply{Text: "No run `r-1` in your workspaces.", IsError: true})
	c := newClient(t, s)

	res, err := c.CallTool(context.Background(), "claim_run", nil)
	if err != nil {
		t.Fatalf("an isError result must not be a transport error: %v", err)
	}
	if !res.IsError {
		t.Error("isError not read back")
	}
	if res.Err() == nil {
		t.Error("Err() should surface the refusal for callers that want it fatal")
	}
}

func TestUnknownToolBothShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply elktest.Reply
	}{
		// -32602 is what Elk answers for a tool it has never heard of, NOT
		// -32601. Getting this wrong is what would make Rein fail hard
		// against an Elk that has not shipped heartbeat_executor.
		{"unknown tool", elktest.UnknownTool("heartbeat_executor")},
		// -32601 is a whole tool group a deployment has not wired.
		{"group missing", elktest.ToolGroupMissing("the Do queue")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := elktest.New(t, testToken)
			s.Reply("heartbeat_executor", tc.reply)
			c := newClient(t, s)

			_, err := c.CallTool(context.Background(), "heartbeat_executor", nil)
			var unknown *elk.UnknownToolError
			if !errors.As(err, &unknown) {
				t.Fatalf("err = %v, want *UnknownToolError", err)
			}
			if unknown.Tool != "heartbeat_executor" {
				t.Errorf("tool = %q", unknown.Tool)
			}
		})
	}
}

func TestGenuineMethodNotFoundIsNotAMissingTool(t *testing.T) {
	// "method not found: resources/list" is the server not implementing a
	// JSON-RPC method — a different thing from a tool it does not offer, and
	// it must not be swallowed as forward-compatibility.
	err := &elk.RPCError{Code: -32601, Message: "method not found: resources/list"}
	if elk.IsUnknownTool(err) {
		t.Error("a genuinely unimplemented method read as a missing tool")
	}
	if !elk.IsMethodNotFound(err) {
		t.Error("IsMethodNotFound = false")
	}
}

func TestWorkspaceNoticeIsSplitOffTheReply(t *testing.T) {
	s := elktest.New(t, testToken)
	const order = "Claimed Elk run `r-9` (contract v2).\n\n**Direction:** ship it"
	s.Text("claim_run", order)
	s.SetNotice("Upgrade Ark before the next run.", "Sam", "2026-08-21")

	var logged []string
	c := newClient(t, s, elk.WithLogger(func(f string, a ...any) { logged = append(logged, f) }))

	res, err := c.CallTool(context.Background(), "claim_run", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != order {
		t.Errorf("the notice was left glued to the work order:\n%q", res.Text)
	}
	if len(res.Notices) != 1 || !strings.Contains(res.Notices[0], "Upgrade Ark") {
		t.Errorf("notices = %#v", res.Notices)
	}
	if len(logged) != 1 {
		t.Errorf("the notice was not logged; it is delivered once and then gone")
	}
	// Delivered once: the next reply is clean.
	res2, err := c.CallTool(context.Background(), "claim_run", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Notices) != 0 {
		t.Errorf("notice delivered twice: %#v", res2.Notices)
	}
}

func TestReplyThatOnlyLooksLikeANoticeIsLeftAlone(t *testing.T) {
	s := elktest.New(t, testToken)
	// A body that is not the notice shape must survive untouched: a wrong
	// split loses reply text, a missed split only leaves noise in a log.
	s.Text("claim_run", "📣 Workspace notice was mentioned in the packet\n\nbody")
	c := newClient(t, s)

	res, err := c.CallTool(context.Background(), "claim_run", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, "📣 Workspace notice was mentioned") {
		t.Errorf("text was mangled: %q", res.Text)
	}
	if len(res.Notices) != 0 {
		t.Errorf("notices = %#v", res.Notices)
	}
}

func TestServerSentEventsResponse(t *testing.T) {
	// Elk answers application/json today. Streamable HTTP lets a server
	// choose, so the client reads the other half too.
	s := elktest.New(t, testToken)
	s.UseSSE(true)
	s.Text("list_workspaces", "Elk Scout")
	c := newClient(t, s)

	res, err := c.CallTool(context.Background(), "list_workspaces", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Elk Scout" {
		t.Errorf("text = %q", res.Text)
	}
}

func TestListTools(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Text("claim_run", "")
	s.Text("heartbeat_executor", "")
	c := newClient(t, s)

	ok, err := c.HasTool(context.Background(), "heartbeat_executor")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("HasTool = false for a tool the server lists")
	}
	ok, err = c.HasTool(context.Background(), "list_executors")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("HasTool = true for a tool the server does not list")
	}
}

func TestInitializeIsSkippedByAStatelessServer(t *testing.T) {
	// A server that does not implement initialize still takes tools/call:
	// Elk's own handler is a pure switch with no session state.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		w.Header().Set("Content-Type", "application/json")
		if msg.Method == "initialize" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) +
				`,"error":{"code":-32601,"message":"method not found: initialize"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) +
			`,"result":{"content":[{"type":"text","text":"fine"}]}}`))
	}))
	defer srv.Close()

	c, err := elk.New(srv.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.CallTool(context.Background(), "list_workspaces", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "fine" {
		t.Errorf("text = %q", res.Text)
	}
}

func TestNewRejectsEmptyInputs(t *testing.T) {
	if _, err := elk.New("", "t"); err == nil {
		t.Error("empty URL accepted")
	}
	if _, err := elk.New("https://example.test/elk-mcp", ""); err == nil {
		t.Error("empty token accepted")
	}
}
