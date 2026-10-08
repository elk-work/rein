package cli_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/fake"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/keyring"
)

const enrolToken = "connector-token"

// registerAs puts the fake adapter in the registry under a real agent kind, so
// the enrolment path can be exercised end to end — including the capability
// list Elk is told about, which comes from the adapter's own manifest.
func registerAs(t *testing.T, kind string) {
	t.Helper()
	a := fake.New()
	a.Kind = kind
	if err := adapter.Register(a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adapter.Unregister(kind) })
}

func enrolServer(t *testing.T) *elktest.Server {
	t.Helper()
	s := elktest.New(t, enrolToken)
	s.Text("connect_executor", `Created named queue "mac-claude" in workspace "Elk Scout" (executor `+
		"`exec-1`). Pull its work with `claim_run(queue: \"mac-claude\")`.")
	s.Text("heartbeat_executor", `Heartbeat recorded for queue "mac-claude". You are online for the `+
		"next 15 minutes. Nothing is waiting for you.")
	return s
}

func TestEnrolWithATokenWritesConfigThenKeychain(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	registerAs(t, "claude")
	s := enrolServer(t)

	out, errb, code := run(t, "enrol",
		"--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac",
		"--token", enrolToken, "--mcp-url", s.URL)
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	q, ok := cfg.Queue("mac-claude")
	if !ok {
		t.Fatalf("no queue in the config:\n%+v", cfg)
	}
	if q.AgentKind != "claude" {
		t.Errorf("agent kind = %q", q.AgentKind)
	}
	if cfg.MachineID == "" {
		t.Error("no machine_id was minted; host_id + agent_kind is the durable identity in Elk")
	}
	if cfg.Elk.MCPURL != s.URL {
		t.Errorf("mcp url = %q", cfg.Elk.MCPURL)
	}

	store := keyring.NewFileStore(filepath.Join(home, keyring.FileName))
	if got, err := store.Get("Elk Scout", "mac-claude"); err != nil || got != enrolToken {
		t.Fatalf("token = %q, err = %v", got, err)
	}
	// The token must never be printed back.
	if strings.Contains(out, enrolToken) {
		t.Fatalf("enrol printed the token:\n%s", out)
	}

	// The adapter's manifest is what Elk is told this machine can do, and the
	// machine id — not the hostname — is what identifies it.
	args := s.CallsTo("connect_executor")[0].Args
	caps, _ := args["declared_capabilities"].([]any)
	if len(caps) == 0 {
		t.Error("no declared_capabilities were sent")
	}
	if args["host_id"] != cfg.MachineID {
		t.Errorf("host_id = %v, want the minted machine id %q", args["host_id"], cfg.MachineID)
	}
	if args["agent_kind"] != "claude" {
		t.Errorf("agent_kind = %v", args["agent_kind"])
	}
	// The enrolment ends by proving the stored credential works and marking
	// the queue online — Elk does not offer work to a queue it thinks is off.
	if len(s.CallsTo("heartbeat_executor")) != 1 {
		t.Error("enrol did not heartbeat after storing the token")
	}
}

func TestEnrolExchangesAClaimCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	registerAs(t, "codex")

	const claim = "claim-code-1"
	const durable = "durable-token-9"
	s := elktest.New(t, claim) // the claim code IS the bearer: it is a bootstrap-tier token
	s.Handle("connect_executor", func(args map[string]any) elktest.Reply {
		if args["claim_code"] != claim {
			return elktest.Reply{Text: "claim_code missing", IsError: true}
		}
		return elktest.Reply{Text: "Connected invited agent queue \"mac-codex\" (executor `exec-2`). " +
			"Replace the one-time claim URL programmatically with this durable connector URL: " +
			"https://api.elk.work/functions/v1/elk-mcp/" + durable + ". Do not print, log, or repeat it. " +
			"Do not call `connect_executor` again."}
	})

	out, errb, code := run(t, "enrol",
		"--workspace", "Elk Scout", "--agent-kind", "codex",
		"--claim-code", claim, "--mcp-url", s.URL)
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}

	store := keyring.NewFileStore(filepath.Join(home, keyring.FileName))
	got, err := store.Get("Elk Scout", "mac-codex")
	if err != nil {
		t.Fatal(err)
	}
	if got != durable {
		t.Errorf("stored %q, want the durable token from the exchange", got)
	}
	if strings.Contains(out, durable) {
		t.Fatalf("enrol printed the durable credential:\n%s", out)
	}
	if s.CallsTo("connect_executor")[0].Args["self_configure"] != true {
		t.Error("self_configure was not asserted, so Elk would not return the credential")
	}
}

// A queue is private to the person who connected it unless someone asks
// otherwise, so `shared` reaches Elk if and only if --shared was typed. The
// omitted case matters as much as the set one: an Elk older than scout
// migration 0201 must see no argument it does not recognise.
func TestEnrolSendsSharedOnlyWithTheFlag(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantKey bool
		wantOut string
	}{
		{
			name:    "without the flag",
			wantOut: "private to you",
		},
		{
			name:    "with the flag",
			args:    []string{"--shared"},
			wantKey: true,
			wantOut: "any member of the workspace",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvHome, t.TempDir())
			registerAs(t, "claude")
			s := enrolServer(t)

			args := append([]string{"enrol",
				"--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac",
				"--token", enrolToken, "--mcp-url", s.URL}, tc.args...)
			out, errb, code := run(t, args...)
			if code != 0 {
				t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
			}

			call := s.CallsTo("connect_executor")[0].Args
			got, ok := call["shared"]
			if ok != tc.wantKey {
				t.Fatalf("shared present = %v, want %v (args = %#v)", ok, tc.wantKey, call)
			}
			if ok && got != true {
				t.Errorf("shared = %#v, want true", got)
			}
			// Elk's own reply does not say which it got, so the enrolment
			// summary has to — otherwise --shared is a flag with no visible
			// effect, and its absence is an assumption nobody can check.
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("enrol did not report who may send work here (want %q):\n%s", tc.wantOut, out)
			}
		})
	}
}

// The claim carries the inviting person's choice, so Elk ignores --shared on
// this path — and says so. Rein passes it through rather than dropping it,
// because a flag that vanishes in silence is how someone comes to believe a
// private queue is open to their team.
func TestEnrolWithAClaimCodeStillSendsSharedAndRelaysThatElkIgnoredIt(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	registerAs(t, "codex")

	const claim = "claim-code-shared"
	const durable = "durable-token-7"
	s := elktest.New(t, claim)
	s.Text("connect_executor", "Connected invited agent queue \"mac-codex\" (executor `exec-3`). "+
		"Replace the one-time claim URL programmatically with this durable connector URL: "+
		"https://api.elk.work/functions/v1/elk-mcp/"+durable+". Do not print, log, or repeat it. "+
		"Do not call `connect_executor` again. The `shared` argument was ignored because the "+
		"inviting person already chose Private or Shared.")
	s.Text("heartbeat_executor", `Heartbeat recorded for queue "mac-codex".`)

	out, errb, code := run(t, "enrol", "--shared",
		"--workspace", "Elk Scout", "--agent-kind", "codex",
		"--claim-code", claim, "--mcp-url", s.URL)
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if s.CallsTo("connect_executor")[0].Args["shared"] != true {
		t.Errorf("shared was dropped on the claim path; Elk can only explain an argument it receives")
	}
	if !strings.Contains(out, "ignored because the inviting person already chose") {
		t.Errorf("Elk's explanation did not reach the person who typed --shared:\n%s", out)
	}
	// And Rein must not contradict Elk by printing a local access line the
	// claim exchange never honoured.
	if strings.Contains(out, "access  ") {
		t.Errorf("enrol asserted an access mode it did not choose:\n%s", out)
	}
}

func TestEnrolRejectsAnOverLongQueueName(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	s := enrolServer(t)

	_, errb, code := run(t, "enrol",
		"--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac",
		"--queue", "mac-claude-13", "--token", enrolToken, "--mcp-url", s.URL)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb, "refused, not shortened") {
		t.Errorf("error does not explain the rejection: %q", errb)
	}
	if n := len(s.Calls()); n != 0 {
		t.Errorf("%d calls reached Elk; the name is refused locally first", n)
	}
}

func TestEnrolRejectsAnUnknownAgentKind(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	_, errb, code := run(t, "enrol", "--workspace", "w", "--agent-kind", "gemini", "--token", "t")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb, "claude, codex, grok, other") {
		t.Errorf("error does not name the kinds Elk accepts: %q", errb)
	}
}

func TestEnrolNeedsACredential(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	_, errb, code := run(t, "enrol", "--workspace", "w", "--agent-kind", "claude", "--host-id", "mac")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb, "--claim-code") {
		t.Errorf("error does not say how to supply one: %q", errb)
	}
}

func TestEnrolTakesTheCredentialFromTheEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	t.Setenv("REIN_ELK_TOKEN", enrolToken)
	s := enrolServer(t)

	_, errb, code := run(t, "enrol",
		"--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac", "--mcp-url", s.URL)
	if code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, errb)
	}
	store := keyring.NewFileStore(filepath.Join(home, keyring.FileName))
	if _, err := store.Get("Elk Scout", "mac-claude"); err != nil {
		t.Fatal(err)
	}
}

func TestEnrolRefusesToOverwriteWithoutForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	s := enrolServer(t)
	store := keyring.NewFileStore(filepath.Join(home, keyring.FileName))
	if err := store.Set("Elk Scout", "mac-claude", "already-here"); err != nil {
		t.Fatal(err)
	}

	_, errb, code := run(t, "enrol",
		"--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac",
		"--token", enrolToken, "--mcp-url", s.URL)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb, "--force") {
		t.Errorf("error does not name the way through: %q", errb)
	}

	_, errb, code = run(t, "enrol", "--force",
		"--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac",
		"--token", enrolToken, "--mcp-url", s.URL)
	if code != 0 {
		t.Fatalf("--force exit = %d\nstderr: %s", code, errb)
	}
	if got, _ := store.Get("Elk Scout", "mac-claude"); got != enrolToken {
		t.Errorf("token = %q, want the new one", got)
	}
}

func TestEnrolSurfacesATierRefusalWithTheRemedy(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	s := elktest.New(t, enrolToken)
	s.Reply("connect_executor", elktest.Reply{IsError: true, Text: "Refused connect_executor: it requires " +
		"the interactive connector-token tier, but this token holds run-scoped."})

	_, errb, code := run(t, "enrol",
		"--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac",
		"--token", enrolToken, "--mcp-url", s.URL)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb, "--claim-code") {
		t.Errorf("error does not point at the path that works: %q", errb)
	}
}

func TestEnrolSucceedsAgainstAnElkWithoutHeartbeat(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	s := elktest.New(t, enrolToken)
	s.Text("connect_executor", `Created named queue "mac-claude" in workspace "Elk Scout" (executor `+"`exec-1`).")
	s.Reply("heartbeat_executor", elktest.UnknownTool("heartbeat_executor"))
	s.Text("list_workspaces", "Elk Scout")

	out, errb, code := run(t, "enrol",
		"--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac",
		"--token", enrolToken, "--mcp-url", s.URL)
	if code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, errb)
	}
	if !strings.Contains(out, "no heartbeat_executor yet") {
		t.Errorf("enrol did not say the queue will not show as online:\n%s", out)
	}
	if !strings.Contains(out, "the stored token works") {
		t.Errorf("enrol did not fall back to a read-only liveness probe:\n%s", out)
	}
	store := keyring.NewFileStore(filepath.Join(home, keyring.FileName))
	if _, err := store.Get("Elk Scout", "mac-claude"); err != nil {
		t.Fatalf("the enrolment was unwound over a probe that could not run: %v", err)
	}
}

func TestRevokeDeletesTheKeychainEntryBeforeTheConfigRow(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	s := enrolServer(t)

	if _, _, code := run(t, "enrol",
		"--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac",
		"--token", enrolToken, "--mcp-url", s.URL); code != 0 {
		t.Fatal("enrol failed")
	}

	out, errb, code := run(t, "enrol", "--revoke", "--queue", "mac-claude")
	if code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, errb)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Queue("mac-claude"); ok {
		t.Error("the queue is still in the config")
	}
	store := keyring.NewFileStore(filepath.Join(home, keyring.FileName))
	keys, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		// The keychain cannot be enumerated on a real machine, so a token
		// left behind here would be unreachable forever.
		t.Errorf("the token outlived the config row that indexes it: %v", keys)
	}
	if !strings.Contains(out, "Settings → Connected agents") {
		t.Errorf("revoke did not say it is local only:\n%s", out)
	}
}

func TestRevokeUnknownQueue(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	_, errb, code := run(t, "enrol", "--revoke", "--queue", "nope")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb, "no queue") {
		t.Errorf("error = %q", errb)
	}
}

func TestStatusReportsAdapterReadiness(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	registerAs(t, "claude")

	cfg := config.Config{
		HostID: "mac", Workspace: "Elk Scout", WorkDir: filepath.Join(home, "work"),
		Queues: []config.Queue{
			{Name: "mac-claude", AgentKind: "claude"},
			{Name: "mac-grok", AgentKind: "grok"},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	out, errb, code := run(t, "status")
	if code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, errb)
	}
	if !strings.Contains(out, "ADAPTER") {
		t.Fatalf("status has no adapter column:\n%s", out)
	}
	if !strings.Contains(out, "ready") {
		t.Errorf("the registered adapter did not report ready:\n%s", out)
	}
	if !strings.Contains(out, "no adapter registered") {
		t.Errorf("an unregistered kind must say so rather than look broken:\n%s", out)
	}
}
