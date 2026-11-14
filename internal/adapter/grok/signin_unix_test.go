//go:build !windows

package grok

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

func TestSignInPreflight(t *testing.T) {
	for _, failed := range []string{"", "gh", "ark", "missing", "cancelled"} {
		t.Run(failed, func(t *testing.T) {
			dir := t.TempDir()
			work := t.TempDir()
			work, _ = filepath.EvalSymlinks(work)
			for _, binary := range []string{"gh", "ark"} {
				args := "auth status"
				if binary == "ark" {
					args = "credential list"
				}
				script := "#!/bin/sh\n[ \"$*\" = '" + args + "' ] || exit 2\n" +
					"[ \"$HOME\" = expected-home ] || exit 3\n" +
					"[ \"$PWD\" = '" + work + "' ] || exit 4\n" +
					"echo checked > " + filepath.Join(work, binary) + "\n"
				if binary == failed {
					script += "echo private-output >&2\nexit 1\n"
				}
				if failed == "missing" && binary == "gh" {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, binary), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failed == "cancelled" {
				cancel()
			}
			err := preflightSignIns(ctx, adapter.RunSpec{WorktreeDir: work, Env: map[string]string{"HOME": "expected-home"}})
			if failed == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, adapter.ErrPreflight) {
				t.Fatalf("want ErrPreflight, got %v", err)
			}
			name := "GitHub"
			if failed == "ark" {
				name = "Ark"
			}
			if !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "private-output") {
				t.Fatal(err)
			}
			if failed != "ark" {
				if _, err := os.Stat(filepath.Join(work, "ark")); !os.IsNotExist(err) {
					t.Fatal("Ark ran after GitHub failed")
				}
			}
		})
	}
}

func TestSpawnStopsBeforeGrokWhenSignInMissing(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	binary := filepath.Join(dir, "grok")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho started > "+marker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir) // no gh: the agent must never start
	a := New()
	a.binary = binary
	_, err := a.spawnProcess(context.Background(), adapter.RunSpec{WorktreeDir: dir})
	if !errors.Is(err, adapter.ErrPreflight) {
		t.Fatalf("want ErrPreflight, got %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("Grok started without sign-in")
	}
}

func TestSignInPreflightDeadline(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := preflightSignIns(ctx, adapter.RunSpec{WorktreeDir: dir})
	if !errors.Is(err, adapter.ErrPreflight) || !strings.Contains(err.Error(), "GitHub") {
		t.Fatalf("want GitHub preflight error, got %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("sign-in check ignored deadline")
	}
}
