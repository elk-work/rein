package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
)

// Plan login only, and the per-queue model and effort.

func TestModelAndEffortParseAndRoundTrip(t *testing.T) {
	cfg, err := loadQueueTOML(t, "model = \"claude-opus-5-5[1m]\"\neffort = \"xhigh\"\ninherit_env = [\"SENTRY_ORG\"]")
	if err != nil {
		t.Fatal(err)
	}
	q := cfg.Queues[0]
	if q.Model != "claude-opus-5-5[1m]" || q.Effort != "xhigh" || len(q.InheritEnv) != 1 {
		t.Fatalf("model %q effort %q inherit_env %v", q.Model, q.Effort, q.InheritEnv)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := cfg.SaveFile(path); err != nil {
		t.Fatal(err)
	}
	back, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b := back.Queues[0]; b.Model != q.Model || b.Effort != q.Effort || len(b.InheritEnv) != 1 {
		t.Errorf("round trip lost a setting: %+v", b)
	}
}

func TestAQueueWithoutModelOrEffortKeepsTheCLIDefaults(t *testing.T) {
	cfg, err := loadQueueTOML(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if q := cfg.Queues[0]; q.Model != "" || q.Effort != "" {
		t.Errorf("an unset queue got model %q effort %q", q.Model, q.Effort)
	}
}

func TestMeteredKeysAndBadSettingsAreRefused(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"bad model", "model = \"claude opus\"", "not a model id"},
		{"bad effort", "effort = \"High Effort\"", "lower-case word"},
		{"inherit a metered key", "inherit_env = [\"ANTHROPIC_API_KEY\"]", "metered API billing"},
		{"inherit a bad name", "inherit_env = [\"NOT-A-NAME\"]", "not an environment variable name"},
		{"capability is a metered key", "capabilities = [\"OPENAI_API_KEY\"]", "metered API billing"},
		{"auth token in a secrets map", "[queues.secrets]\nANTHROPIC_AUTH_TOKEN = \"svc\"", "metered API billing"},
		{"xai key in a secrets map", "[queues.secrets]\nXAI_API_KEY = \"svc\"", "metered API billing"},
		{"codex key in a secrets map", "[queues.secrets]\nCODEX_API_KEY = \"svc\"", "metered API billing"},
		{"inherit_env on a scoped queue", "inherit_env = [\"SENTRY_ORG\"]\n[queues.secrets]\nTOKEN = \"svc\"", "only their map"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadQueueTOML(t, tc.body)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

func TestMeteredKeysAreRefusedAtTheTopLevel(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"capabilities", "capabilities = [\"XAI_API_KEY\"]\n"},
		{"inherit_env", "inherit_env = [\"OPENAI_API_KEY\"]\n"},
		{"mcp server key", "[mcp_servers.billing]\ncommand = \"x\"\nenv_vars = [\"ANTHROPIC_API_KEY\"]\n"},
		{"mcp bearer", "[mcp_servers.billing]\nurl = \"https://example.invalid\"\nbearer_token_env_var = \"OPENAI_API_KEY\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := config.LoadFile(path); err == nil || !strings.Contains(err.Error(), "metered API billing") {
				t.Errorf("err = %v; want a metered-billing refusal", err)
			}
		})
	}
}
