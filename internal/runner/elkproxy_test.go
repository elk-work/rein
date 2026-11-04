package runner

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

// fakeElk is an upstream Elk MCP that records what reached it.
type fakeElk struct {
	mu      sync.Mutex
	auth    []string
	methods []string
	tools   []string
	status  int
}

func (f *fakeElk) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &m)
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.methods = append(f.methods, m.Method)
	f.tools = append(f.tools, m.Params.Name)
	status := f.status
	f.mu.Unlock()
	if status != 0 {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var result any = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
	if m.Method == "tools/list" {
		result = map[string]any{"tools": []any{
			map[string]any{"name": "list_actions"},
			map[string]any{"name": "submit_deliverable"},
			map[string]any{"name": "create_action"},
			map[string]any{"name": "rotate_own_token"},
		}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
}

func proxyFor(t *testing.T, up *fakeElk) *elkProxy {
	t.Helper()
	ts := httptest.NewServer(up)
	t.Cleanup(ts.Close)
	p, err := startElkProxy(ts.URL, "queue-secret-token", adapter.RunConnectorDeniedTools)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("the proxy passed on a sign-in challenge")
	}
	return resp.StatusCode, string(b)
}

func TestTheProxyForwardsWithTheQueuesTokenAndHidesIt(t *testing.T) {
	up := &fakeElk{}
	p := proxyFor(t, up)
	if strings.Contains(p.URL(), "queue-secret-token") || !strings.HasPrefix(p.URL(), "http://127.0.0.1:") {
		t.Fatalf("proxy URL = %q", p.URL())
	}
	code, body := post(t, p.URL(), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_actions","arguments":{}}}`)
	if code != 200 || !strings.Contains(body, `"ok"`) {
		t.Fatalf("an allowed call: %d %s", code, body)
	}
	if len(up.auth) != 1 || up.auth[0] != "Bearer queue-secret-token" {
		t.Errorf("Elk saw Authorization %q, want the queue's token", up.auth)
	}
}

func TestTheProxyRefusesReinsOwnToolsAndForwardsNothing(t *testing.T) {
	up := &fakeElk{}
	p := proxyFor(t, up)
	for _, tool := range []string{"submit_deliverable", "claim_run", "rotate_own_token", "deploy_app"} {
		code, body := post(t, p.URL(), `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+tool+`"}}`)
		if code != 200 || !strings.Contains(body, `"error"`) || !strings.Contains(body, tool) || !strings.Contains(body, `"id":7`) {
			t.Errorf("%s: %d %s, want a JSON-RPC error for id 7", tool, code, body)
		}
	}
	// A batch carrying one denied call is refused whole.
	post(t, p.URL(), `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_actions"}},`+
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"submit_deliverable"}}]`)
	if len(up.methods) != 0 {
		t.Errorf("a refused request reached Elk: %v %v", up.methods, up.tools)
	}
	// Unreadable JSON cannot be shown harmless, so it is not forwarded either.
	if code, _ := post(t, p.URL(), `{"method":"tools/call","params":`); code != http.StatusBadRequest {
		t.Errorf("unreadable request: %d, want 400", code)
	}
	if len(up.methods) != 0 {
		t.Errorf("an unreadable request reached Elk")
	}
}

func TestTheProxyLeavesReinsOwnToolsOutOfTheList(t *testing.T) {
	p := proxyFor(t, &fakeElk{})
	_, body := post(t, p.URL(), `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	for _, want := range []string{"list_actions", "create_action"} {
		if !strings.Contains(body, want) {
			t.Errorf("tools/list lost %s: %s", want, body)
		}
	}
	for _, bad := range []string{"submit_deliverable", "rotate_own_token"} {
		if strings.Contains(body, bad) {
			t.Errorf("tools/list offers %s: %s", bad, body)
		}
	}
	// The same through a server-sent event.
	out := string(p.filterToolsList([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"tools\":[{\"name\":\"claim_run\"},{\"name\":\"list_actions\"}]}}\n\n"), "text/event-stream"))
	if strings.Contains(out, "claim_run") || !strings.Contains(out, "list_actions") {
		t.Errorf("SSE tools/list not filtered: %s", out)
	}
}

func TestTheProxyAnswersOnlyItsOwnPathAndPOST(t *testing.T) {
	up := &fakeElk{}
	p := proxyFor(t, up)
	base := strings.TrimSuffix(p.URL(), "/"+p.secret)
	if code, _ := post(t, base+"/guess", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); code != http.StatusNotFound {
		t.Errorf("a wrong path answered %d", code)
	}
	resp, err := http.Get(p.URL())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET answered %d", resp.StatusCode)
	}
	if len(up.methods) != 0 {
		t.Errorf("a refused request reached Elk")
	}
	p.Close()
	if _, err := http.Post(p.URL(), "application/json", strings.NewReader(`{}`)); err == nil {
		t.Error("the proxy still answers after the run")
	}
}

func TestARefusedQueueTokenIsNotASignInPrompt(t *testing.T) {
	p := proxyFor(t, &fakeElk{status: http.StatusUnauthorized})
	code, body := post(t, p.URL(), `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if code != http.StatusBadGateway || !strings.Contains(body, "refused this queue's credential") {
		t.Errorf("got %d %s", code, body)
	}
}

func TestTheProxyReadsKeysExactlyAsElkDoes(t *testing.T) {
	// Go struct decoding ignores case; Elk's handler does not. A request
	// that reads as an allowed tool to one and a denied tool to the other
	// must be refused.
	up := &fakeElk{}
	p := proxyFor(t, up)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"submit_deliverable","Name":"list_actions"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"Name":"list_actions","name":"submit_deliverable"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_actions","name":"submit_deliverable"}}`,
		`{"jsonrpc":"2.0","id":1,"Method":"tools/list","method":"tools/call","params":{"name":"claim_run"}}`,
	} {
		post(t, p.URL(), body)
	}
	if len(up.methods) != 0 {
		t.Errorf("a disguised call to Rein's own tool reached Elk: %v %v", up.methods, up.tools)
	}
}
