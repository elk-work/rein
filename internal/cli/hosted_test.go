package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRunHostedGateBeforeKeychain(t *testing.T) {
	for _, tc := range []struct{ enabled, flag bool }{{false, true}, {true, false}} {
		home := t.TempDir()
		t.Setenv("REIN_HOME", home)
		t.Setenv("REIN_KEYRING", "invalid-backend-must-not-open")
		enabled := "false"
		if tc.enabled {
			enabled = "true"
		}
		text := "[hosted]\nenabled = " + enabled + "\nallow_repos = [\"acme/repo\"]\n[[queues]]\nname = \"cloud-claude\"\nagent_kind = \"claude\"\n"
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		root := NewRootCommand("test")
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		args := []string{"run", "--once"}
		if tc.flag {
			args = append(args, "--hosted")
		}
		root.SetArgs(args)
		if err := root.Execute(); err == nil || err.Error() != "hosted mode requires both --hosted and [hosted] enabled = true" {
			t.Fatalf("gate error=%v", err)
		}
	}
}
