package runner_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
)

// The vault broker caller token (ark:rein#175): an unscoped queue that keeps
// the vault hands each run its own token from the keychain as
// ELK_CONNECTOR_TOKEN, redacted like a mapped secret, so vault-run reads go
// through Elk's vault broker instead of falling back to the Management API.

const brokerValue = "elkbroker_TESTVALUE_0123456789abcdef"

// fakeBroker is a keychain holding queue items and per-queue broker tokens.
type fakeBroker struct {
	*fakeItems
	tokens map[string]string
}

func newFakeBroker(kv ...string) *fakeBroker {
	f := &fakeBroker{fakeItems: newFakeItems(), tokens: map[string]string{}}
	for i := 0; i+1 < len(kv); i += 2 {
		f.tokens[kv[i]] = kv[i+1]
	}
	return f
}

func (f *fakeBroker) BrokerToken(workspace, queue string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := workspace + "/" + queue
	f.reads = append(f.reads, keyring.BrokerService+"/"+key)
	v, ok := f.tokens[key]
	if !ok {
		return "", fmt.Errorf("%w: %s", keyring.ErrItemNotFound, keyring.BrokerItem(workspace, queue))
	}
	return v, nil
}

func (f *fakeBroker) SetBrokerToken(workspace, queue, token string) error {
	return fmt.Errorf("not in tests")
}

func (f *fakeBroker) DeleteBrokerToken(workspace, queue string) error {
	return fmt.Errorf("not in tests")
}

func TestAnUnscopedVaultQueueHandsEachRunItsBrokerToken(t *testing.T) {
	h := newHarness(t)
	h.agent.Script, h.agent.Result = leakingScript(brokerValue)
	logDir := t.TempDir()
	logs, err := runlog.Open(logDir)
	if err != nil {
		t.Fatal(err)
	}
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{
		Secrets:          newFakeBroker(space+"/"+queue, brokerValue),
		HostCapabilities: machineHolds("supabase-vault"),
		Logs:             logs,
	}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	specs := h.agent.Specs()
	if len(specs) != 1 {
		t.Fatalf("the run did not start\nlog:\n%s", h.log)
	}
	spec := specs[0]
	if spec.Env[runner.BrokerTokenEnv] != brokerValue {
		t.Errorf("%s not set for the run; Env has %v", runner.BrokerTokenEnv, keys(spec.Env))
	}
	// Still unscoped: the queue keeps the vault and the daemon's named
	// variables, and the prompt still points at vault-run.
	if !has(declared(h), "supabase-vault") {
		t.Error("handing a broker token dropped the vault from the queue")
	}
	if !strings.Contains(spec.SystemPrompt, "vault-run --only KEY -- <cmd>") {
		t.Error("the vault paragraph was replaced on an unscoped queue")
	}
	if !strings.Contains(spec.SystemPrompt, "`"+runner.BrokerTokenEnv+"`") || strings.Contains(spec.SystemPrompt, brokerValue) {
		t.Error("the prompt should name the token's variable and never its value")
	}
	all := everythingWritten(t, h, logDir)
	if strings.Contains(all, brokerValue) {
		t.Error("the broker token reached a log, a report or the deliverable")
	}
	if !strings.Contains(all, "[redacted:"+runner.BrokerTokenEnv+"]") {
		t.Error("the agent's echo of the token was not redacted under its name")
	}
	if !strings.Contains(h.log.String(), runner.BrokerTokenEnv+" set for this run") {
		t.Errorf("the daemon log does not say the token was handed over:\n%s", h.log)
	}
}

func TestNoBrokerTokenLeavesTheRunAsBefore(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{Secrets: newFakeBroker(), HostCapabilities: machineHolds("supabase-vault")}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	specs := h.agent.Specs()
	if len(specs) != 1 {
		t.Fatalf("a missing broker token stopped the run\nlog:\n%s", h.log)
	}
	if _, ok := specs[0].Env[runner.BrokerTokenEnv]; ok {
		t.Error("a token was set with none in the keychain")
	}
	if strings.Contains(specs[0].SystemPrompt, "Rein set `"+runner.BrokerTokenEnv+"`") {
		t.Error("the prompt claims a token the run does not have")
	}
	if !strings.Contains(h.log.String(), "no caller token for this queue") {
		t.Errorf("the log does not say the run has no broker token:\n%s", h.log)
	}
}

func TestABrokerTokenReinWillNotHandOverIsRefused(t *testing.T) {
	for name, value := range map[string]string{
		"its own Elk token": token,
		"too short":         "Zq7xKw",
		"not a token":       "https://example.invalid/functions/v1/elk-mcp/abcdefgh",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.elk.Text("claim_run", order("run-1"))
			if err := h.run(runner.Options{
				Secrets:          newFakeBroker(space+"/"+queue, value),
				HostCapabilities: machineHolds("supabase-vault"),
			}); err != nil {
				t.Fatalf("%v\nlog:\n%s", err, h.log)
			}
			specs := h.agent.Specs()
			if len(specs) != 1 {
				t.Fatalf("a refused broker token stopped the run\nlog:\n%s", h.log)
			}
			if _, ok := specs[0].Env[runner.BrokerTokenEnv]; ok {
				t.Error("a refused value was handed to the run")
			}
			if strings.Contains(h.log.String(), value) {
				t.Error("the refused value reached the log")
			}
			if !strings.Contains(h.log.String(), "so this run carries none") {
				t.Errorf("the log does not say why:\n%s", h.log)
			}
		})
	}
}

func TestAnotherQueuesElkTokenIsNeverABrokerToken(t *testing.T) {
	h := newHarness(t)
	const otherToken = "other_queue_token_0123456789abcdef"
	h.cfg.Queues = append(h.cfg.Queues, config.Queue{Name: "mac-codex", AgentKind: kind})
	if err := h.store.Set(space, "mac-codex", otherToken); err != nil {
		t.Fatal(err)
	}
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{
		Secrets:          newFakeBroker(space+"/"+queue, otherToken),
		HostCapabilities: machineHolds("supabase-vault"),
		Queues:           []string{queue},
	}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	specs := h.agent.Specs()
	if len(specs) == 0 {
		t.Fatalf("the run did not start\nlog:\n%s", h.log)
	}
	if _, ok := specs[0].Env[runner.BrokerTokenEnv]; ok {
		t.Error("another queue's Elk token was handed to the run")
	}
	if !strings.Contains(h.log.String(), "a Rein queue's own Elk token") {
		t.Errorf("the log does not say why:\n%s", h.log)
	}
}

func TestAQueueWithoutTheVaultNeverReadsABrokerToken(t *testing.T) {
	h := newHarness(t)
	items := newFakeBroker(space+"/"+queue, brokerValue)
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{Secrets: items, HostCapabilities: machineHolds()}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if got := items.readKeys(); len(got) != 0 {
		t.Errorf("read %v for a queue that does not keep the vault", got)
	}
	if specs := h.agent.Specs(); len(specs) != 1 || specs[0].Env[runner.BrokerTokenEnv] != "" {
		t.Error("a queue without the vault got a broker token")
	}
}

func TestAScopedQueueNeverGetsTheBrokerToken(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Secrets = map[string]string{"POSTHOG_API_KEY": "acme-posthog"}
	items := newFakeBroker(space+"/"+queue, brokerValue)
	items.items["acme-posthog/rein"] = posthogValue
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{Secrets: items, HostCapabilities: machineHolds("supabase-vault")}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	specs := h.agent.Specs()
	if len(specs) != 1 {
		t.Fatalf("the run did not start\nlog:\n%s", h.log)
	}
	if _, ok := specs[0].Env[runner.BrokerTokenEnv]; ok {
		t.Error("a scoped queue got the broker token its map does not name")
	}
	for _, k := range items.readKeys() {
		if strings.HasPrefix(k, keyring.BrokerService+"/") {
			t.Errorf("a scoped queue read %s", k)
		}
	}
}

func TestTheFileKeychainHoldsBrokerTokensApartFromQueueSecrets(t *testing.T) {
	f := keyring.NewFileItems(filepath.Join(t.TempDir(), keyring.SecretsFileName))
	if err := f.SetBrokerToken(space, queue, brokerValue); err != nil {
		t.Fatal(err)
	}
	got, err := f.BrokerToken(space, queue)
	if err != nil || got != brokerValue {
		t.Fatalf("read back %v", err)
	}
	// A secrets map can never name it, so no other queue's run reaches it.
	if _, err := f.ReadItem(keyring.BrokerService, space+"/"+queue); err == nil || strings.Contains(err.Error(), brokerValue) {
		t.Errorf("a queue secret read reached the broker token: %v", err)
	}
	if err := f.DeleteBrokerToken(space, queue); err != nil {
		t.Fatal(err)
	}
	if _, err := f.BrokerToken(space, queue); err == nil {
		t.Error("delete left the token")
	}
}
