package adapter

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fakeProgram(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}
	path := filepath.Join(t.TempDir(), "program")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadProgramVersion(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("x", programVersionMax+1)
	for _, c := range []struct{ name, body, want string }{
		{"its own words", `echo "2.1.296 (Claude Code)"`, "2.1.296 (Claude Code)"},
		{"first line only", `printf 'codex-cli 0.9\nbuilt yesterday\n'`, "codex-cli 0.9"},
		{"a failure is no reading", `echo nope; exit 3`, ""},
		{"nothing printed", `true`, ""},
		{"too long to be a version", `echo ` + long, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ReadProgramVersion(ctx, fakeProgram(t, c.body)); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
	if got := ReadProgramVersion(ctx, ""); got != "" {
		t.Fatalf("no binary: got %q", got)
	}
	if got := ReadProgramVersion(ctx, "/nonexistent/program"); got != "" {
		t.Fatalf("missing binary: got %q", got)
	}
}
