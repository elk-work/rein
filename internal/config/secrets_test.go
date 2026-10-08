package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
)

// The per-queue secrets map (ark:rein#48): names and keychain items in the
// config, never values.

func loadQueueTOML(t *testing.T, queueBody string) (config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[[queues]]\nname = \"acme-claude\"\nagent_kind = \"claude\"\n" + queueBody + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.LoadFile(path)
}

func TestSecretsParse(t *testing.T) {
	cfg, err := loadQueueTOML(t, `land = "pr"

[queues.secrets]
POSTHOG_API_KEY       = "acme-posthog"
AWS_SECRET_ACCESS_KEY = "acme-aws/secret-key"
DEEP                  = "svc/team/key"`)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	q := cfg.Queues[0]
	if !q.Scoped() {
		t.Fatal("a queue with a secrets map is not scoped")
	}
	want := []config.SecretRef{
		{Env: "AWS_SECRET_ACCESS_KEY", Service: "acme-aws", Account: "secret-key"},
		{Env: "DEEP", Service: "svc", Account: "team/key"}, // split at the FIRST slash
		{Env: "POSTHOG_API_KEY", Service: "acme-posthog", Account: config.DefaultSecretAccount},
	}
	if got := q.SecretRefs(); !reflect.DeepEqual(got, want) {
		t.Errorf("SecretRefs =\n%#v\nwant\n%#v", got, want)
	}
	if config.DefaultSecretAccount != "rein" {
		t.Errorf("DefaultSecretAccount = %q; the docs tell people to create items with -a rein", config.DefaultSecretAccount)
	}
}

func TestAQueueWithoutSecretsIsNotScoped(t *testing.T) {
	cfg, err := loadQueueTOML(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if q := cfg.Queues[0]; q.Scoped() || len(q.SecretRefs()) != 0 {
		t.Errorf("a queue that never mentions secrets is scoped (%v) or has refs %v — every existing queue would change",
			q.Scoped(), q.SecretRefs())
	}
}

func TestScopedSecretsWithAnEmptyMap(t *testing.T) {
	cfg, err := loadQueueTOML(t, "scoped_secrets = true")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Queues[0].Scoped() {
		t.Error("scoped_secrets = true did not scope the queue")
	}
}

func TestSecretsValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"bad name", "[queues.secrets]\n\"1BAD\" = \"svc\"", "not an environment variable name"},
		{"dash in name", "[queues.secrets]\n\"BAD-NAME\" = \"svc\"", "not an environment variable name"},
		{"system variable", "[queues.secrets]\nPATH = \"svc\"", "system variable"},
		{"home", "[queues.secrets]\nHOME = \"svc\"", "system variable"},
		{"rein prefix", "[queues.secrets]\nREIN_KEYRING = \"svc\"", "REIN_ prefix"},
		{"anthropic key on claude", "[queues.secrets]\nANTHROPIC_API_KEY = \"svc\"", "metered API billing"},
		{"empty item", "[queues.secrets]\nTOKEN = \"\"", "names no keychain service"},
		{"slash, no account", "[queues.secrets]\nTOKEN = \"svc/\"", "no account after it"},
		{"no service", "[queues.secrets]\nTOKEN = \"/acct\"", "names no keychain service"},
		{"rein's own tokens", "[queues.secrets]\nTOKEN = \"rein/Elk Scout/mac-claude\"", "Rein's own credentials"},
		{"wrangler connector", "[queues.secrets]\nTOKEN = \"elk-connector-url\"", "Rein's own credentials"},
		{"vault on a scoped queue", "capabilities = [\"supabase-vault\"]\n[queues.secrets]\nTOKEN = \"svc\"", "cannot declare supabase-vault"},
		{"credential outside the map", "capabilities = [\"OTHER_KEY\"]\n[queues.secrets]\nTOKEN = \"svc\"", "add it there"},
		{"vault on an empty scoped queue", "scoped_secrets = true\ncapabilities = [\"supabase-vault\"]", "cannot declare supabase-vault"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadQueueTOML(t, tc.body)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "acme-claude") {
				t.Errorf("error does not say %q, or which queue: %v", tc.want, err)
			}
		})
	}
}

func TestSecretsAcceptsToolsAndMapNamesAsCapabilities(t *testing.T) {
	_, err := loadQueueTOML(t, "capabilities = [\"docker\", \"TOKEN\"]\n[queues.secrets]\nTOKEN = \"svc\"")
	if err != nil {
		t.Errorf("a tool, and a credential the map supplies, were refused: %v", err)
	}
}

func TestWranglerQueuesCanBeScoped(t *testing.T) {
	for _, key := range []string{"wrangler", "pm"} {
		for _, body := range []string{"scoped_secrets = true", "[queues.secrets]\nTOKEN = \"svc\""} {
			cfg, err := loadQueueTOML(t, key+" = true\n"+body)
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.Queues[0].Scoped() {
				t.Fatal("Wrangler queue did not opt into scoped environment")
			}
			cfg.Queues[0].AgentKind = "codex"
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestSecretsRoundTrip: `rein enrol` rewrites the whole file, so a map a
// person wrote must survive a save and a load — and the file must still hold
// only names.
func TestSecretsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	want := config.Config{HostID: "mac", Queues: []config.Queue{
		{Name: "mac-claude", AgentKind: "claude"},
		{Name: "acme-claude", AgentKind: "claude", Land: config.LandPR,
			Secrets: map[string]string{"POSTHOG_API_KEY": "acme-posthog", "AWS_ACCESS_KEY_ID": "acme-aws/key-id"}},
		{Name: "acme-codex", AgentKind: "codex", ScopedSecrets: true},
	}}
	if err := want.SaveFile(path); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	got, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got.Queues[0].Scoped() {
		t.Error("an unscoped queue came back scoped")
	}
	if !reflect.DeepEqual(got.Queues[1].Secrets, want.Queues[1].Secrets) {
		t.Errorf("Secrets = %v after a round trip, want %v", got.Queues[1].Secrets, want.Queues[1].Secrets)
	}
	if !got.Queues[2].Scoped() || len(got.Queues[2].Secrets) != 0 {
		t.Error("scoped_secrets did not survive a round trip")
	}
}

func TestParseSecretItem(t *testing.T) {
	for in, want := range map[string][2]string{
		"svc":           {"svc", "rein"},
		" svc ":         {"svc", "rein"},
		"svc/acct":      {"svc", "acct"},
		"My Item/a b/c": {"My Item", "a b/c"},
	} {
		s, a, err := config.ParseSecretItem(in)
		if err != nil || s != want[0] || a != want[1] {
			t.Errorf("ParseSecretItem(%q) = %q, %q, %v; want %q, %q", in, s, a, err, want[0], want[1])
		}
	}
	if got := (config.SecretRef{Env: "X", Service: "svc", Account: "acct"}).Item(); got != `service "svc", account "acct"` {
		t.Errorf("Item() = %q", got)
	}
}
