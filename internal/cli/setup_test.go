package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/keyring"
)

// pairServer fakes the worker-pair edge function: it hands out one code and
// answers each poll from a script, recording what Rein sent.
type pairServer struct {
	*httptest.Server
	mu      sync.Mutex
	started map[string]any
	secrets []string
	answers []map[string]any
}

func newPairServer(t *testing.T, answers ...map[string]any) *pairServer {
	t.Helper()
	p := &pairServer{answers: answers}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.secrets = append(p.secrets, body["secret"].(string))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			p.started = body
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": "KQ7M4MDX", "expires_at": time.Now().Add(10 * time.Minute).Format(time.RFC3339), "interval": 3,
			})
		case strings.HasSuffix(r.URL.Path, "/poll"):
			next := map[string]any{"status": "pending"}
			if len(p.answers) > 0 {
				next, p.answers = p.answers[0], p.answers[1:]
			}
			_ = json.NewEncoder(w).Encode(next)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

func TestSetupPairsWithACodeAndEnrolsWithTheClaimItCollects(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	registerAs(t, "claude")

	const claim = "claim-from-pairing"
	const durable = "durable-token-pair"
	s := elktest.New(t, claim)
	s.Handle("connect_executor", func(args map[string]any) elktest.Reply {
		if args["claim_code"] != claim {
			return elktest.Reply{Text: "claim_code missing", IsError: true}
		}
		return elktest.Reply{Text: "Connected invited agent queue \"mba-claude\" (executor `exec-9`). " +
			"Replace the one-time claim URL programmatically with this durable connector URL: " +
			"https://api.elk.work/functions/v1/elk-mcp/" + durable + ". Do not print, log, or repeat it."}
	})
	s.Text("heartbeat_executor", `Heartbeat recorded for queue "mba-claude".`)
	p := newPairServer(t,
		map[string]any{"status": "pending"},
		map[string]any{"status": "approved", "claim_code": claim, "workspace": "Elk Scout", "queue_name": "mba-claude"})

	out, errb, code := run(t, "setup", "--agent-kind", "claude", "--host-id", "mba", "--name", "MacBook Air",
		"--mcp-url", s.URL, "--pair-url", p.URL, "--poll", "10ms", "--no-service")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if !strings.Contains(out, "KQ7M-4MDX") {
		t.Errorf("the code was not shown in its readable form:\n%s", out)
	}
	for _, secret := range []string{claim, durable, p.secrets[0]} {
		if strings.Contains(out+errb, secret) {
			t.Fatalf("setup printed a secret %q:\n%s", secret, out)
		}
	}
	// One secret for the whole pairing, never a short or guessable one.
	for _, sec := range p.secrets {
		if sec != p.secrets[0] || len(sec) != 43 {
			t.Fatalf("secrets = %q", p.secrets)
		}
	}
	if p.started["machine_name"] != "MacBook Air" || p.started["agent_kind"] != "claude" || p.started["queue_hint"] != "mba-claude" {
		t.Errorf("start sent %v", p.started)
	}
	if got := s.CallsTo("connect_executor")[0].Args["display_name"]; got != "MacBook Air" {
		t.Errorf("display_name = %v, want the computer's name", got)
	}

	store := keyring.NewFileStore(filepath.Join(home, keyring.FileName))
	if got, err := store.Get("Elk Scout", "mba-claude"); err != nil || got != durable {
		t.Fatalf("stored %q, err %v; want the durable token from the exchange", got, err)
	}
}

func TestSetupStopsWhenSomeoneSaysNotMyComputer(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	s := elktest.New(t, "unused")
	p := newPairServer(t, map[string]any{"status": "declined"})

	_, errb, code := run(t, "setup", "--agent-kind", "claude", "--host-id", "mba",
		"--mcp-url", s.URL, "--pair-url", p.URL, "--poll", "10ms", "--no-service")
	if code != 1 || !strings.Contains(errb, "Not my computer") {
		t.Fatalf("exit = %d, stderr = %q", code, errb)
	}
	if len(s.Calls()) != 0 {
		t.Errorf("a declined pairing still talked to Elk's MCP endpoint")
	}
}

func TestSetupSaysWhenTheCodeExpires(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	s := elktest.New(t, "unused")
	p := newPairServer(t, map[string]any{"status": "expired"})
	_, errb, code := run(t, "setup", "--agent-kind", "codex", "--host-id", "mba",
		"--mcp-url", s.URL, "--pair-url", p.URL, "--poll", "10ms", "--no-service")
	if code != 1 || !strings.Contains(errb, "Run rein setup again") {
		t.Fatalf("exit = %d, stderr = %q", code, errb)
	}
}
