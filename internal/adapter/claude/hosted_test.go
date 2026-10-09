package claude

import (
	"context"
	"errors"
	"github.com/elk-work/rein/internal/adapter"
	"os"
	"path/filepath"
	"testing"
)

func TestHostedInitRequiresExactAPIKeySource(t *testing.T) {
	for _, source := range []string{"ANTHROPIC_API_KEY", "none", "", "managed key", "ANTHROPIC_AUTH_TOKEN"} {
		err := checkPlanInit(source, true)
		if source == "ANTHROPIC_API_KEY" {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, adapter.ErrNotAPIAuth) {
			t.Fatalf("source %q accepted: %v", source, err)
		}
	}
}

func TestHostedStillRefusesRepositoryAPIKeyHelper(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{"apiKeyHelper":"repo-helper"}`), 0600); err != nil {
		t.Fatal(err)
	}
	spec := adapter.RunSpec{Hosted: true, WorktreeDir: dir, Prompt: "test", PermissionMode: adapter.PermissionFull, Env: map[string]string{"ANTHROPIC_API_KEY": "synthetic-key"}}
	if _, err := New().Start(context.Background(), spec); !errors.Is(err, adapter.ErrNotAPIAuth) {
		t.Fatalf("helper not refused: %v", err)
	}
}
