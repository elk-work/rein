package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/cli"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/keyring"
)

func TestSecretSetReadsStdinAndNeverPrintsTheValue(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	t.Setenv(keyring.EnvBackend, "file")
	const value = "phc_not-a-real-key"
	var out, errb bytes.Buffer
	root := cli.NewRootCommand("v0.0.0-test")
	root.SetArgs([]string{"secret", "set", "posthog_api_key"})
	root.SetIn(strings.NewReader(value + "\n"))
	root.SetOut(&out)
	root.SetErr(&errb)
	if err := root.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, errb.String())
	}
	if strings.Contains(out.String()+errb.String(), value) {
		t.Fatal("the value was printed")
	}
	raw, err := os.ReadFile(filepath.Join(home, keyring.SecretsFileName))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	_ = json.Unmarshal(raw, &m)
	if m["elk-secret/POSTHOG_API_KEY"] != value {
		t.Fatalf("stored %v", m)
	}
	got, err := keyring.NewFileItems(filepath.Join(home, keyring.SecretsFileName)).ReadItem("elk-secret", "POSTHOG_API_KEY")
	if err != nil || got != value {
		t.Fatalf("read back %q, %v", got, err)
	}

	if _, errs, code := run(t, "secret", "rm", "POSTHOG_API_KEY"); code != 0 {
		t.Fatalf("rm: %s", errs)
	}
	if _, err := keyring.NewFileItems(filepath.Join(home, keyring.SecretsFileName)).ReadItem("elk-secret", "POSTHOG_API_KEY"); err == nil {
		t.Fatal("rm left the value")
	}
}

func TestSecretSetRefusesABadNameAndAnEmptyValue(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	if _, errb, code := run(t, "secret", "set", "not ok"); code != 1 || !strings.Contains(errb, "not a secret name") {
		t.Fatalf("code=%d stderr=%q", code, errb)
	}
	if _, errb, code := run(t, "secret", "set", "OK_NAME"); code != 1 || !strings.Contains(errb, "no value on stdin") {
		t.Fatalf("code=%d stderr=%q", code, errb)
	}
}
