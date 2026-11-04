package runner_test

import (
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/fake"
	geminiadapter "github.com/elk-work/rein/internal/adapter/gemini"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
)

// The person's own Google key (Casey's ruling, 2026-11-04): read from the
// keychain for a gemini run, handed to that adapter alone in ProviderKey, and
// redacted everywhere Rein writes.

const geminiValue = "AIza_TESTVALUE_google_key_fedcba9876543210"

// asGemini points the harness's queue at a fake registered as gemini, in
// place of the real adapter, for the length of the test.
func asGemini(t *testing.T, h *harness) *fake.Adapter {
	t.Helper()
	adapter.Unregister(geminiadapter.Kind)
	g := fake.New()
	g.Kind = geminiadapter.Kind
	if err := adapter.Register(g); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adapter.Unregister(geminiadapter.Kind)
		adapter.MustRegister(geminiadapter.New())
	})
	h.cfg.Queues[0].AgentKind = geminiadapter.Kind
	return g
}

func TestTheKeyServiceIsTheAppsManagedSecrets(t *testing.T) {
	if geminiadapter.KeyService != runner.ManagedSecretService {
		t.Fatalf("the gemini adapter reads %q; `rein secret set` writes %q",
			geminiadapter.KeyService, runner.ManagedSecretService)
	}
}

func TestAGeminiRunGetsTheKeyAndNothingElseDoes(t *testing.T) {
	h := newHarness(t)
	g := asGemini(t, h)
	g.Script, g.Result = leakingScript(geminiValue)
	logDir := t.TempDir()
	logs, err := runlog.Open(logDir)
	if err != nil {
		t.Fatal(err)
	}
	items := newFakeItems("elk-secret/GEMINI_API_KEY", geminiValue)
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{Secrets: items, Logs: logs}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	specs := g.Specs()
	if len(specs) != 1 {
		t.Fatalf("started %d sessions, want 1\nlog:\n%s", len(specs), h.log)
	}
	spec := specs[0]
	if spec.ProviderKey != geminiValue {
		t.Error("the gemini run did not get the key in ProviderKey")
	}
	if _, ok := spec.Env["GEMINI_API_KEY"]; ok {
		t.Error("the key is in Env, the door every adapter shares")
	}
	for _, name := range spec.PassEnv {
		if name == "GEMINI_API_KEY" {
			t.Error("the key is passed through from the daemon")
		}
	}
	if strings.Contains(spec.SystemPrompt, geminiValue) || strings.Contains(spec.Prompt, geminiValue) {
		t.Error("the key is in the prompt")
	}
	all := everythingWritten(t, h, logDir)
	if strings.Contains(all, geminiValue) {
		i := strings.Index(all, geminiValue)
		t.Errorf("the key was written down, after: …%s", all[max(0, i-80):i])
	}
	if !strings.Contains(h.submitted().Arg("deliverable"), "[redacted:GEMINI_API_KEY]") {
		t.Errorf("the deliverable does not carry the marker:\n%s", h.submitted().Arg("deliverable"))
	}
}

func TestAnotherKindNeverReadsOrGetsTheKey(t *testing.T) {
	h := newHarness(t) // a claude queue
	t.Setenv("GEMINI_API_KEY", geminiValue)
	items := newFakeItems("elk-secret/GEMINI_API_KEY", geminiValue)
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{Secrets: items}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spec := h.agent.Specs()[0]
	if spec.ProviderKey != "" {
		t.Error("a claude run was handed the Google key")
	}
	for _, kv := range spec.InheritedEnv() {
		if strings.HasPrefix(kv, "GEMINI_API_KEY=") {
			t.Error("a claude run inherited GEMINI_API_KEY from the daemon")
		}
	}
	for _, k := range items.readKeys() {
		if strings.Contains(k, "GEMINI_API_KEY") {
			t.Error("a claude run read the Google key from the keychain")
		}
	}
}

func TestAGeminiRunWithNoKeyIsStuckWithThePlainLine(t *testing.T) {
	h := newHarness(t)
	g := asGemini(t, h)
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{Secrets: newFakeItems()}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(g.Specs()); n != 0 {
		t.Fatalf("started %d sessions with no key", n)
	}
	sub := h.submitted()
	if sub.Arg("status") != "stuck" || sub.Arg("title") != geminiadapter.MissingKeyLine {
		t.Errorf("status %q, title %q", sub.Arg("status"), sub.Arg("title"))
	}
	if sub.Arg("stop_code") != "agent_login" {
		t.Errorf("stop_code = %q, want agent_login", sub.Arg("stop_code"))
	}
}
