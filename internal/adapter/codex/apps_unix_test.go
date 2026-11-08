//go:build !windows

package codex

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

func TestSpawnDisablesBuiltInApps(t *testing.T) {
	fakeRealHome(t)
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := New()
	a.binary = binary
	p, err := a.spawnProcess(context.Background(), adapter.RunSpec{RunID: "apps", WorktreeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer p.home.remove()
	out, err := io.ReadAll(p.stdout)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.wait(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "-c\nfeatures.apps=false\n") {
		t.Fatalf("apps not disabled: %s", out)
	}
}
