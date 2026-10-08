package runner_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
)

// Per-queue scoped secrets (ark:rein#48): a queue with a secrets map gets
// exactly those variables, from the keychain, and nothing from the machine's
// wider vault — and no value ever reaches a log, a prompt, a report or a
// deliverable.

// The values are long and distinctive so a substring search for one cannot
// match anything else by accident.
const (
	posthogValue = "phc_TESTVALUE_posthog_0123456789abcdef"
	awsValue     = "AKIA_TESTVALUE_aws_fedcba9876543210"
	vaultValue   = "VAULT_TESTVALUE_machine_wide_00000000"
)

// fakeItems is a keychain for tests: (service, account) → value, recording
// every read.
type fakeItems struct {
	mu    sync.Mutex
	items map[string]string
	reads []string
}

func newFakeItems(kv ...string) *fakeItems {
	f := &fakeItems{items: map[string]string{}}
	for i := 0; i+1 < len(kv); i += 2 {
		f.items[kv[i]] = kv[i+1]
	}
	return f
}

func (f *fakeItems) Name() string { return "fake" }

func (f *fakeItems) ReadItem(service, account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := service + "/" + account
	f.reads = append(f.reads, key)
	v, ok := f.items[key]
	if !ok {
		return "", fmt.Errorf("%w: service %q, account %q", keyring.ErrItemNotFound, service, account)
	}
	return v, nil
}

func (f *fakeItems) readKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reads...)
}

// leakingScript is an agent that echoes every value it was given — in its
// text, in a tool call's input, in a question to its person and in its final
// summary — which is the worst a run can do with what Rein hands it.
func leakingScript(values ...string) ([]adapter.Event, adapter.Result) {
	joined := strings.Join(values, " and ")
	input, _ := json.Marshal(map[string]string{"command": "echo " + joined})
	script := []adapter.Event{
		{Kind: adapter.EventText, Text: "my keys are " + joined},
		{Kind: adapter.EventToolUse, Tool: &adapter.ToolUse{ID: "t1", Name: "Bash", Input: input}},
		{Kind: adapter.EventQuestion, Text: "is " + joined + " the right key?"},
		{Kind: adapter.EventUsage, Usage: &adapter.Usage{InputTokens: 10, OutputTokens: 5, Model: "fake-1"}},
	}
	result := adapter.Result{
		Status:    adapter.StatusSucceeded,
		Summary:   "Done. For the record the key was " + joined + ".",
		SessionID: "fake-session",
		Usage:     adapter.Usage{InputTokens: 10, OutputTokens: 5, Model: "fake-1"},
	}
	return script, result
}

// everythingWritten is every byte this run put anywhere outside the agent:
// every Elk call's arguments, the daemon log, and the run log on disk.
func everythingWritten(t *testing.T, h *harness, logDir string) string {
	t.Helper()
	var b strings.Builder
	for _, c := range h.elk.Calls() {
		raw, _ := json.Marshal(c.Args)
		b.WriteString(c.Tool + " " + string(raw) + "\n")
	}
	b.WriteString(h.log.String())
	_ = filepath.WalkDir(logDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(path)
			b.Write(data)
		}
		return nil
	})
	return b.String()
}

func TestAScopedQueueRunGetsExactlyItsSecrets(t *testing.T) {
	h := newHarness(t)
	t.Setenv("REIN_TEST_DAEMON_ONLY", vaultValue)
	host := machineHolds("supabase-vault", "REIN_TEST_DAEMON_ONLY", "docker")
	h.cfg.Queues[0].Secrets = map[string]string{
		"POSTHOG_API_KEY":   "acme-posthog",
		"AWS_ACCESS_KEY_ID": "acme-aws/access-key",
	}
	items := newFakeItems(
		"acme-posthog/rein", posthogValue,
		"acme-aws/access-key", awsValue,
		"elk-vault/rein", vaultValue, // on the machine, not in the map
	)
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{Secrets: items, HostCapabilities: host}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	specs := h.agent.Specs()
	if len(specs) != 1 {
		t.Fatalf("started %d sessions, want 1\nlog:\n%s", len(specs), h.log)
	}
	spec := specs[0]
	want := map[string]string{"POSTHOG_API_KEY": posthogValue, "AWS_ACCESS_KEY_ID": awsValue}
	if len(spec.Env) != len(want) {
		t.Fatalf("RunSpec.Env has %d variables, want exactly %d: %v", len(spec.Env), len(want), keys(spec.Env))
	}
	for k, v := range want {
		if spec.Env[k] != v {
			t.Errorf("RunSpec.Env[%s] is not the keychain item's value", k)
		}
	}
	if len(spec.PassEnv) != 0 {
		t.Errorf("a scoped run passes daemon variables %v; its map is its only source of credentials", spec.PassEnv)
	}
	// The item outside the map was never read: a scoped run touches only its
	// own keychain entries.
	for _, k := range items.readKeys() {
		if k != "acme-posthog/rein" && k != "acme-aws/access-key" {
			t.Errorf("read keychain item %s, which the queue's map does not name", k)
		}
	}
	// And the agent process inherits nothing but system variables from the
	// daemon: the credential in this process's environment does not reach it.
	for _, kv := range spec.InheritedEnv() {
		if strings.HasPrefix(kv, "REIN_TEST_DAEMON_ONLY=") {
			t.Error("a scoped run inherited a credential from the daemon's environment")
		}
	}

	// The prompt names what the run holds — names only.
	if strings.Contains(spec.SystemPrompt, "vault-run -- <cmd>") {
		t.Error("a scoped run's prompt still tells it to use the machine's vault")
	}
	for _, name := range []string{"`AWS_ACCESS_KEY_ID`", "`POSTHOG_API_KEY`", "This queue's credentials are scoped"} {
		if !strings.Contains(spec.SystemPrompt, name) {
			t.Errorf("the system prompt does not say %s", name)
		}
	}
	for _, v := range []string{posthogValue, awsValue, vaultValue} {
		if strings.Contains(spec.SystemPrompt, v) || strings.Contains(spec.Prompt, v) {
			t.Error("a secret value is in the prompt")
		}
	}

	// What the queue declares is what its runs hold.
	caps := declared(h)
	for _, gone := range []string{"supabase-vault", "REIN_TEST_DAEMON_ONLY"} {
		if has(caps, gone) {
			t.Errorf("a scoped queue declared %s, which its runs do not have: %v", gone, caps)
		}
	}
	for _, kept := range []string{"POSTHOG_API_KEY", "AWS_ACCESS_KEY_ID", "docker"} {
		if !has(caps, kept) {
			t.Errorf("declared_capabilities %v is missing %s", caps, kept)
		}
	}
	if !strings.Contains(h.log.String(), "scoped secrets, read from the keychain at each run: AWS_ACCESS_KEY_ID, POSTHOG_API_KEY") {
		t.Errorf("the startup log does not name the queue's secrets:\n%s", h.log)
	}
	if sub := h.submitted(); sub.Arg("status") != "ready" {
		t.Errorf("status = %q", sub.Arg("status"))
	}
}

func TestAQueueWithoutAMapIsScopedByName(t *testing.T) {
	h := newHarness(t)
	t.Setenv("REIN_TEST_UNDECLARED", "x")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-not-real")
	host := machineHolds("supabase-vault", "SUPABASE_SERVICE_ROLE_KEY")
	items := newFakeItems("acme-posthog/rein", posthogValue)
	h.elk.Text("claim_run", order("run-1", "supabase-vault"))

	if err := h.run(runner.Options{Secrets: items, HostCapabilities: host}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spec := h.agent.Specs()[0]
	if spec.Env != nil {
		t.Errorf("a run with no secrets map got Env=%v; want none", keys(spec.Env))
	}
	// It keeps what it declared it holds, and nothing else the daemon has.
	if len(spec.PassEnv) != 1 || spec.PassEnv[0] != "SUPABASE_SERVICE_ROLE_KEY" {
		t.Errorf("PassEnv = %v; want exactly the declared SUPABASE_SERVICE_ROLE_KEY", spec.PassEnv)
	}
	for _, kv := range spec.InheritedEnv() {
		if strings.HasPrefix(kv, "REIN_TEST_UNDECLARED=") || strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Errorf("a run inherited %s from the daemon without the queue naming it", strings.SplitN(kv, "=", 2)[0])
		}
	}
	if len(items.readKeys()) != 0 {
		t.Errorf("an unscoped queue read the keychain: %v", items.readKeys())
	}
	if !strings.Contains(spec.SystemPrompt, "vault-run -- <cmd>") {
		t.Error("an unscoped queue lost the vault paragraph")
	}
	caps := declared(h)
	if !has(caps, "supabase-vault") || !has(caps, "SUPABASE_SERVICE_ROLE_KEY") {
		t.Errorf("an unscoped queue stopped declaring what it holds: %v", caps)
	}
	if strings.Contains(h.log.String(), "scoped secrets") {
		t.Errorf("an unscoped queue logged scoped secrets:\n%s", h.log)
	}
}

func TestTwoScopedQueuesNeverShareASecret(t *testing.T) {
	h := newHarness(t)
	const other, otherSpace = "mac-claude2", "Acme"
	if err := h.store.Set(otherSpace, other, token); err != nil {
		t.Fatal(err)
	}
	h.cfg.Queues = []config.Queue{
		{Name: queue, AgentKind: kind, Secrets: map[string]string{"POSTHOG_API_KEY": "elk-posthog"}},
		{Name: other, AgentKind: kind, Workspace: otherSpace, Secrets: map[string]string{"AWS_ACCESS_KEY_ID": "acme-aws"}},
	}
	items := newFakeItems("elk-posthog/rein", posthogValue, "acme-aws/rein", awsValue)
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{Secrets: items, MaxConcurrent: 2}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	specs := h.agent.Specs()
	if len(specs) != 2 {
		t.Fatalf("started %d sessions, want one per queue\nlog:\n%s", len(specs), h.log)
	}
	var sawPosthog, sawAWS bool
	for _, s := range specs {
		if len(s.Env) != 1 {
			t.Errorf("a run got %d variables, want exactly its own queue's one: %v", len(s.Env), keys(s.Env))
		}
		switch {
		case s.Env["POSTHOG_API_KEY"] == posthogValue:
			sawPosthog = true
		case s.Env["AWS_ACCESS_KEY_ID"] == awsValue:
			sawAWS = true
		}
		if s.Env["POSTHOG_API_KEY"] != "" && s.Env["AWS_ACCESS_KEY_ID"] != "" {
			t.Error("one run holds both queues' secrets")
		}
	}
	if !sawPosthog || !sawAWS {
		t.Errorf("each queue should have got its own secret (posthog=%v aws=%v)", sawPosthog, sawAWS)
	}
}

func TestAMissingItemFailsClosedNamingItAndNoValue(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Secrets = map[string]string{
		"POSTHOG_API_KEY":   "acme-posthog",
		"AWS_ACCESS_KEY_ID": "acme-aws/access-key",
	}
	items := newFakeItems("acme-posthog/rein", posthogValue) // the AWS item is missing
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{Secrets: items}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.agent.Specs()); n != 0 {
		t.Fatalf("started %d sessions with a secret missing; want none", n)
	}
	if created, _ := h.wts.counts(); created != 0 {
		t.Errorf("created %d worktrees before failing closed; want none", created)
	}
	sub := h.submitted()
	d := sub.Arg("deliverable")
	if sub.Arg("status") != "stuck" {
		t.Errorf("status = %q, want stuck", sub.Arg("status"))
	}
	for _, want := range []string{"AWS_ACCESS_KEY_ID", `service "acme-aws", account "access-key"`,
		"security add-generic-password", "No work was started"} {
		if !strings.Contains(d, want) {
			t.Errorf("the stuck deliverable does not say %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, posthogValue) || strings.Contains(h.log.String(), posthogValue) {
		t.Error("the value of the item that DID resolve leaked into the failure")
	}
}

func TestATooShortOrEmptyItemIsRefused(t *testing.T) {
	for name, value := range map[string]string{"short": "abc123", "empty": "  "} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Queues[0].Secrets = map[string]string{"TOKEN": "svc"}
			h.elk.Text("claim_run", order("run-1"))
			if err := h.run(runner.Options{Secrets: newFakeItems("svc/rein", value)}); err != nil {
				t.Fatal(err)
			}
			if len(h.agent.Specs()) != 0 {
				t.Fatal("a run started with an unusable secret")
			}
			if sub := h.submitted(); sub.Arg("status") != "stuck" || !strings.Contains(sub.Arg("deliverable"), "`TOKEN`") {
				t.Errorf("want stuck naming TOKEN, got %q:\n%s", sub.Arg("status"), sub.Arg("deliverable"))
			}
		})
	}
}

func TestAScopedQueueWithNoReaderFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Secrets = map[string]string{"TOKEN": "svc"}
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatal(err)
	}
	if len(h.agent.Specs()) != 0 || h.submitted().Arg("status") != "stuck" {
		t.Error("a scoped queue with no keychain reader started a run")
	}
}

func TestAnEmptyScopedQueueGetsNoCredentials(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].ScopedSecrets = true
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{Secrets: newFakeItems(), HostCapabilities: machineHolds("supabase-vault")}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spec := h.agent.Specs()[0]
	if len(spec.Env) != 0 || len(spec.PassEnv) != 0 {
		t.Errorf("Env=%v PassEnv=%v; want no variables at all", keys(spec.Env), spec.PassEnv)
	}
	if !strings.Contains(spec.SystemPrompt, "it holds none") || strings.Contains(spec.SystemPrompt, "vault-run -- <cmd>") {
		t.Error("an empty scoped queue's prompt does not say it holds no credentials")
	}
	if has(declared(h), "supabase-vault") {
		t.Error("an empty scoped queue declared the vault")
	}
}

func TestAScopedQueuesMCPServersSeeOnlyItsMap(t *testing.T) {
	h := newHarness(t)
	// In the daemon's environment, but not in the map: a scoped queue's
	// server keyed on it must be left out, because the run will not have it.
	t.Setenv("REIN_TEST_FIGMA_KEY", vaultValue)
	h.cfg.MCPServers = map[string]config.MCPServer{
		"posthog": {URL: "https://mcp.posthog.com/mcp", BearerTokenEnvVar: "POSTHOG_API_KEY"},
		"figma":   {URL: "https://mcp.figma.com/mcp", BearerTokenEnvVar: "REIN_TEST_FIGMA_KEY"},
	}
	h.cfg.Queues[0].MCPServers = []string{"posthog", "figma"}
	h.cfg.Queues[0].Secrets = map[string]string{"POSTHOG_API_KEY": "acme-posthog"}
	h.elk.Text("claim_run", order("run-1", "mcp:posthog"))

	if err := h.run(runner.Options{Secrets: newFakeItems("acme-posthog/rein", posthogValue)}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spec := h.agent.Specs()[0]
	if spec.MCPServers["posthog"] == nil || spec.MCPServers["figma"] != nil {
		t.Errorf("MCPServers = %#v; want posthog (its key is in the map) and not figma", spec.MCPServers)
	}
	// The server reaches its key the only way any vendor binary does: from
	// the agent process's environment, which is where the map's values went.
	if spec.Env["POSTHOG_API_KEY"] != posthogValue {
		t.Error("the allowed server's key is not in the run's environment")
	}
	caps := declared(h)
	if !has(caps, "mcp:posthog") || has(caps, "mcp:figma") {
		t.Errorf("declared %v; want mcp:posthog and not mcp:figma", caps)
	}
	if !strings.Contains(h.log.String(), "MCP server left out: figma: REIN_TEST_FIGMA_KEY is not in this queue's secrets map") {
		t.Errorf("the log does not say why figma was left out:\n%s", h.log)
	}
}

func TestASecretValueIsNeverLoggedReportedOrDelivered(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Secrets = map[string]string{
		"POSTHOG_API_KEY":   "acme-posthog",
		"AWS_ACCESS_KEY_ID": "acme-aws",
	}
	h.agent.Script, h.agent.Result = leakingScript(posthogValue, awsValue)
	logDir := t.TempDir()
	logs, err := runlog.Open(logDir)
	if err != nil {
		t.Fatal(err)
	}
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{
		Secrets: newFakeItems("acme-posthog/rein", posthogValue, "acme-aws/rein", awsValue),
		Logs:    logs,
	}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if len(h.agent.Specs()) != 1 {
		t.Fatalf("the run did not start\nlog:\n%s", h.log)
	}

	all := everythingWritten(t, h, logDir)
	for _, v := range []string{posthogValue, awsValue} {
		if strings.Contains(all, v) {
			i := strings.Index(all, v)
			snippet := all[max(0, i-80):i]
			for _, w := range []string{posthogValue, awsValue} {
				snippet = strings.ReplaceAll(snippet, w, "<VALUE>")
			}
			t.Errorf("a secret value was written down, after: …%s", snippet)
		}
	}
	// And what replaced it says which credential it was, so a reader can tell
	// the agent leaked a key without anyone seeing the key.
	sub := h.submitted()
	for _, marker := range []string{"[redacted:POSTHOG_API_KEY]", "[redacted:AWS_ACCESS_KEY_ID]"} {
		if !strings.Contains(sub.Arg("deliverable"), marker) {
			t.Errorf("the deliverable does not carry %s:\n%s", marker, sub.Arg("deliverable"))
		}
	}
	var question bool
	for _, c := range h.elk.CallsTo("report_progress") {
		if c.Arg("kind") == "question" && strings.Contains(c.Arg("body"), "[redacted:POSTHOG_API_KEY]") {
			question = true
		}
	}
	if !question {
		t.Error("the agent's question did not reach Elk redacted")
	}
	recs, err := logs.Records("run-1")
	if err != nil {
		t.Fatal(err)
	}
	var tool bool
	for _, r := range recs {
		if r.Tool != nil && strings.Contains(string(r.Tool.Input), "[redacted:AWS_ACCESS_KEY_ID]") {
			tool = true
		}
	}
	if !tool {
		t.Error("the run log's tool call was not redacted in place")
	}
}

// machineHolds is the host capability list a machine-level config.toml
// declaring these names produces, injected so no probe runs.
func machineHolds(names ...string) *runner.HostCapabilities {
	return runner.NewHostCapabilities("config.toml",
		append([]string{"elk-connector", "git", "worktree", "github-cli"}, names...)...)
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestAScopedWranglerCycleGetsExactlyItsSecrets(t *testing.T) {
	h := wranglerHarness(t, "wrangler")
	t.Setenv("REIN_TEST_DAEMON_ONLY", vaultValue)
	host := machineHolds("supabase-vault", "REIN_TEST_DAEMON_ONLY", "docker")
	h.cfg.Queues[0].Secrets = map[string]string{
		"POSTHOG_API_KEY":   "acme-posthog",
		"AWS_ACCESS_KEY_ID": "acme-aws/access-key",
	}
	items := newFakeItems(
		"acme-posthog/rein", posthogValue,
		"acme-aws/access-key", awsValue,
		"elk-vault/rein", vaultValue, // on the machine, not in the map
	)
	h.elk.Text("claim_run", order("run-1", "pm"))

	if err := h.run(runner.Options{Secrets: items, HostCapabilities: host}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	specs := h.agent.Specs()
	if len(specs) != 1 {
		t.Fatalf("started %d sessions, want 1\nlog:\n%s", len(specs), h.log)
	}
	spec := specs[0]
	want := map[string]string{"POSTHOG_API_KEY": posthogValue, "AWS_ACCESS_KEY_ID": awsValue}
	if len(spec.Env) != len(want) {
		t.Fatalf("RunSpec.Env has %d variables, want exactly %d: %v", len(spec.Env), len(want), keys(spec.Env))
	}
	for k, v := range want {
		if spec.Env[k] != v {
			t.Errorf("RunSpec.Env[%s] is not the keychain item's value", k)
		}
	}
	if len(spec.PassEnv) != 0 {
		t.Errorf("a scoped run passes daemon variables %v; its map is its only source of credentials", spec.PassEnv)
	}
	// The item outside the map was never read: a scoped run touches only its
	// own keychain entries.
	for _, k := range items.readKeys() {
		if k != "acme-posthog/rein" && k != "acme-aws/access-key" {
			t.Errorf("read keychain item %s, which the queue's map does not name", k)
		}
	}
	// And the agent process inherits nothing but system variables from the
	// daemon: the credential in this process's environment does not reach it.
	for _, kv := range spec.InheritedEnv() {
		if strings.HasPrefix(kv, "REIN_TEST_DAEMON_ONLY=") {
			t.Error("a scoped run inherited a credential from the daemon's environment")
		}
	}

	// The prompt names what the run holds — names only.
	if strings.Contains(spec.SystemPrompt, "vault-run -- <cmd>") {
		t.Error("a scoped run's prompt still tells it to use the machine's vault")
	}
	for _, name := range []string{"`AWS_ACCESS_KEY_ID`", "`POSTHOG_API_KEY`", "This queue's credentials are scoped"} {
		if !strings.Contains(spec.SystemPrompt, name) {
			t.Errorf("the system prompt does not say %s", name)
		}
	}
	for _, v := range []string{posthogValue, awsValue, vaultValue} {
		if strings.Contains(spec.SystemPrompt, v) || strings.Contains(spec.Prompt, v) {
			t.Error("a secret value is in the prompt")
		}
	}

	// What the queue declares is what its runs hold.
	caps := declared(h)
	for _, gone := range []string{"supabase-vault", "REIN_TEST_DAEMON_ONLY"} {
		if has(caps, gone) {
			t.Errorf("a scoped queue declared %s, which its runs do not have: %v", gone, caps)
		}
	}
	for _, kept := range []string{"POSTHOG_API_KEY", "AWS_ACCESS_KEY_ID", "docker"} {
		if !has(caps, kept) {
			t.Errorf("declared_capabilities %v is missing %s", caps, kept)
		}
	}
	if !strings.Contains(h.log.String(), "scoped secrets, read from the keychain at each run: AWS_ACCESS_KEY_ID, POSTHOG_API_KEY") {
		t.Errorf("the startup log does not name the queue's secrets:\n%s", h.log)
	}
	if sub := h.submitted(); sub.Arg("status") != "ready" {
		t.Errorf("status = %q", sub.Arg("status"))
	}
}
