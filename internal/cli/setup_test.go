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
	mu        sync.Mutex
	started   map[string]any
	secrets   []string
	answers   []map[string]any
	collect   map[string]any
	cancelled bool
	// oldElk answers like an Elk from before queue_name: no echo beside the code.
	oldElk bool
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
			answer := map[string]any{
				"code": "KQ7M4MDX", "expires_at": time.Now().Add(10 * time.Minute).Format(time.RFC3339), "interval": 3,
			}
			if name, ok := body["queue_name"]; ok && !p.oldElk {
				answer["queue_name"] = name
			}
			_ = json.NewEncoder(w).Encode(answer)
		case strings.HasSuffix(r.URL.Path, "/collect"):
			_ = json.NewEncoder(w).Encode(p.collect)
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			p.cancelled = true
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
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
		map[string]any{"status": "approved", "workspace": "Elk Scout", "approved_by": "Casey Clark", "queue_name": "mba-claude"})
	p.collect = map[string]any{"claim_code": claim, "workspace": "Elk Scout", "queue_name": "mba-claude"}

	out, errb, code := run(t, "setup", "--agent-kind", "claude", "--host-id", "mba", "--name", "MacBook Air",
		"--mcp-url", s.URL, "--pair-url", p.URL, "--poll", "10ms", "--no-service")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if !strings.Contains(out, "Casey Clark, in Elk Scout, allowed this computer") {
		t.Errorf("setup did not say who allowed it, and where:\n%s", out)
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

// A computer's own name is usually longer than the 12 characters Elk allows a
// worker's name ("DESKTOP-4F7K2LQ"), and Elk refuses it at connect, after
// Allow. The Windows lab test on v0.9.11 failed exactly there.
func TestSetupFitsALongComputerNameToElksCap(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	registerAs(t, "claude")

	const claim = "claim-long-name"
	s := elktest.New(t, claim)
	s.Handle("connect_executor", func(args map[string]any) elktest.Reply {
		return elktest.Reply{Text: "Connected invited agent queue \"desk-claude\" (executor `exec-9`). " +
			"Replace the one-time claim URL programmatically with this durable connector URL: " +
			"https://api.elk.work/functions/v1/elk-mcp/durable-long. Do not print, log, or repeat it."}
	})
	s.Text("heartbeat_executor", `Heartbeat recorded for queue "desk-claude".`)
	p := newPairServer(t,
		map[string]any{"status": "approved", "workspace": "Elk Scout", "approved_by": "Casey Clark", "queue_name": "desk-claude"})
	p.collect = map[string]any{"claim_code": claim, "workspace": "Elk Scout", "queue_name": "desk-claude"}

	out, errb, code := run(t, "setup", "--agent-kind", "claude", "--host-id", "desk", "--name", "DESKTOP-4F7K2LQ",
		"--mcp-url", s.URL, "--pair-url", p.URL, "--poll", "10ms", "--no-service")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if p.started["machine_name"] != "DESKTOP-4F7K2LQ" {
		t.Errorf("the Allow screen should get the full name; start sent %v", p.started["machine_name"])
	}
	if got := s.CallsTo("connect_executor")[0].Args["display_name"]; got != "DESKTO…7K2LQ" {
		t.Errorf("display_name = %v, want both ends of the computer's name within 12 runes", got)
	}
}

func TestSetupRefusesALongDisplayNameBeforeShowingACode(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	registerAs(t, "claude")
	p := newPairServer(t, map[string]any{"status": "pending"})

	out, errb, code := run(t, "setup", "--agent-kind", "claude", "--display-name", "win01-pairing-test",
		"--pair-url", p.URL, "--poll", "10ms", "--no-service")
	if code == 0 {
		t.Fatalf("setup accepted an 18-character display name\nstdout: %s", out)
	}
	if !strings.Contains(errb, "cap is 12") {
		t.Errorf("stderr does not name the cap: %s", errb)
	}
	if p.started != nil || strings.Contains(out, "Your code") {
		t.Errorf("a code was asked for before the name was checked:\n%s", out)
	}
}

// pairedElk is an Elk MCP server that connects the claim as the given queue.
func pairedElk(t *testing.T, claim, queue string) *elktest.Server {
	t.Helper()
	s := elktest.New(t, claim)
	s.Handle("connect_executor", func(args map[string]any) elktest.Reply {
		return elktest.Reply{Text: "Connected invited agent queue \"" + queue + "\" (executor `exec-9`). " +
			"Replace the one-time claim URL programmatically with this durable connector URL: " +
			"https://api.elk.work/functions/v1/elk-mcp/durable-" + queue + ". Do not print, log, or repeat it."}
	})
	s.Text("heartbeat_executor", `Heartbeat recorded for queue "`+queue+`".`)
	return s
}

// The report behind this: `rein setup --queue bora-claude` on host id "dav"
// joined as dav-claude, because the flag was never sent and Elk's answer
// overwrote it.
func TestSetupJoinsAsTheQueueNameItIsGiven(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	registerAs(t, "claude")

	const claim = "claim-asked-name"
	s := pairedElk(t, claim, "bora-claude")
	p := newPairServer(t,
		map[string]any{"status": "approved", "workspace": "Elk Scout", "approved_by": "Casey Clark", "queue_name": "bora-claude"})
	p.collect = map[string]any{"claim_code": claim, "workspace": "Elk Scout", "queue_name": "bora-claude"}

	out, errb, code := run(t, "setup", "--yes", "--agent-kind", "claude", "--host-id", "dav",
		"--queue", "bora-claude", "--display-name", "bora-claude",
		"--mcp-url", s.URL, "--pair-url", p.URL, "--poll", "10ms", "--no-service")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if p.started["queue_name"] != "bora-claude" {
		t.Errorf("start did not ask for the name: %v", p.started)
	}
	if _, hinted := p.started["queue_hint"]; hinted {
		t.Errorf("start sent a hint beside an asked name: %v", p.started)
	}
	said, shown := strings.Index(out, `will join as queue "bora-claude"`), strings.Index(out, "Your code")
	if said < 0 || shown < 0 || said > shown {
		t.Errorf("setup did not name the queue before showing the code:\n%s", out)
	}
	if got := s.CallsTo("connect_executor")[0].Args["queue"]; got != "bora-claude" {
		t.Errorf("connect_executor queue = %v", got)
	}
	store := keyring.NewFileStore(filepath.Join(home, keyring.FileName))
	if _, err := store.Get("Elk Scout", "bora-claude"); err != nil {
		t.Fatalf("no token stored under bora-claude: %v", err)
	}
	if _, err := store.Get("Elk Scout", "dav-claude"); err == nil {
		t.Fatal("a token was stored under the default name dav-claude")
	}
}

func TestSetupRefusesABadQueueNameBeforeAnyNetworkCall(t *testing.T) {
	for name, want := range map[string]string{
		"borabora-claude": "cap is 12",
		"bora_claude":     "at most 12 characters: letters, digits and hyphens",
		"-bora":           "starting and ending with a letter or digit",
		"":                "at most 12 characters",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(config.EnvHome, t.TempDir())
			registerAs(t, "claude")
			p := newPairServer(t, map[string]any{"status": "pending"})
			out, errb, code := run(t, "setup", "--agent-kind", "claude", "--host-id", "dav", "--queue", name,
				"--pair-url", p.URL, "--poll", "10ms", "--no-service")
			if code == 0 {
				t.Fatalf("setup accepted --queue %q\nstdout: %s", name, out)
			}
			if !strings.Contains(errb, want) {
				t.Errorf("stderr does not say %q: %s", want, errb)
			}
			if len(p.secrets) != 0 || strings.Contains(out, "Your code") {
				t.Errorf("Elk was called before the name was checked:\n%s", out)
			}
		})
	}
}

// An Elk from before queue_name ignores it and would pick its own name at
// Allow. Setup stops before a code is shown rather than join under that name.
func TestSetupStopsBeforeTheCodeWhenElkDoesNotTakeTheName(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	registerAs(t, "claude")
	s := elktest.New(t, "unused")
	p := newPairServer(t, map[string]any{"status": "approved", "queue_name": "dav-claude"})
	p.oldElk = true

	out, errb, code := run(t, "setup", "--agent-kind", "claude", "--host-id", "dav", "--queue", "bora-claude",
		"--mcp-url", s.URL, "--pair-url", p.URL, "--poll", "10ms", "--no-service")
	if code == 0 || !strings.Contains(errb, `did not take the queue name "bora-claude"`) {
		t.Fatalf("exit = %d, stderr = %q", code, errb)
	}
	if strings.Contains(out, "Your code") || !p.cancelled || len(s.Calls()) != 0 {
		t.Errorf("code shown, pairing left open, or Elk connected (cancelled=%v):\n%s", p.cancelled, out)
	}
}

func TestSetupNeverSwapsAnAskedNameForAnother(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	registerAs(t, "claude")
	s := elktest.New(t, "unused")
	p := newPairServer(t,
		map[string]any{"status": "approved", "workspace": "Elk Scout", "approved_by": "Casey Clark", "queue_name": "dav-claude2"})

	_, errb, code := run(t, "setup", "--yes", "--agent-kind", "claude", "--host-id", "dav", "--queue", "bora-claude",
		"--mcp-url", s.URL, "--pair-url", p.URL, "--poll", "10ms", "--no-service")
	if code == 0 || !strings.Contains(errb, `"dav-claude2"`) || !strings.Contains(errb, `"bora-claude"`) {
		t.Fatalf("exit = %d, stderr = %q", code, errb)
	}
	if !p.cancelled || len(s.Calls()) != 0 {
		t.Errorf("the pairing was not cancelled (cancelled=%v), or Elk was connected", p.cancelled)
	}
}

func TestSetupPrintsElksSentenceWhenTheNameIsTaken(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	registerAs(t, "claude")
	s := elktest.New(t, "unused")
	const sentence = `The queue name "bora-claude" is already used in this workspace. Choose another name.`
	p := newPairServer(t, map[string]any{
		"status": "refused", "reason": "queue_taken", "queue_name": "bora-claude", "error": sentence})

	_, errb, code := run(t, "setup", "--agent-kind", "claude", "--host-id", "dav", "--queue", "bora-claude",
		"--mcp-url", s.URL, "--pair-url", p.URL, "--poll", "10ms", "--no-service")
	if code == 0 || !strings.Contains(errb, sentence) {
		t.Fatalf("exit = %d, stderr = %q", code, errb)
	}
	if strings.Contains(errb, "lost this code") || len(s.Calls()) != 0 {
		t.Errorf("a refusal was reported as a lost code, or Elk was connected: %q", errb)
	}
}

// The second half of the report: running setup again added "dav-claude2".
func TestSetupASecondTimeKeepsTheOneQueue(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	registerAs(t, "claude")

	const claim = "claim-first"
	s := pairedElk(t, claim, "dav-claude")
	p := newPairServer(t,
		map[string]any{"status": "approved", "workspace": "Elk Scout", "approved_by": "Casey Clark", "queue_name": "dav-claude"})
	p.collect = map[string]any{"claim_code": claim, "workspace": "Elk Scout", "queue_name": "dav-claude"}
	args := []string{"setup", "--yes", "--agent-kind", "claude", "--host-id", "dav",
		"--mcp-url", s.URL, "--pair-url", p.URL, "--poll", "10ms", "--no-service"}
	if out, errb, code := run(t, args...); code != 0 {
		t.Fatalf("first setup: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	pairCalls, elkCalls := len(p.secrets), len(s.Calls())

	for _, again := range [][]string{args, append(append([]string{}, args...), "--queue", "dav-claude")} {
		out, errb, code := run(t, again...)
		if code != 0 {
			t.Fatalf("second setup: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
		}
		if !strings.Contains(out, `already joined to "Elk Scout" as queue "dav-claude"`) {
			t.Errorf("the second setup did not say which queue it has:\n%s", out)
		}
		if len(p.secrets) != pairCalls || len(s.Calls()) != elkCalls || strings.Contains(out, "Your code") {
			t.Errorf("the second setup paired again:\n%s", out)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Queues) != 1 || cfg.Queues[0].Name != "dav-claude" {
		t.Errorf("queues = %+v, want only dav-claude", cfg.Queues)
	}
}

func TestSetupAndEnrolHelpStateBothNameLimits(t *testing.T) {
	for _, command := range []string{"setup", "enrol"} {
		out, _, code := run(t, command, "--help")
		if code != 0 {
			t.Fatalf("%s --help: exit = %d", command, code)
		}
		for _, want := range []string{
			"queue name, at most 12 characters: letters, digits and hyphens",
			"name shown on Elk's Agents page, at most 12 characters",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s --help does not say %q", command, want)
			}
		}
	}
}
