//go:build !windows

package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

func TestExpiredSessionInvalidatesStoredLogin(t *testing.T) {
	for _, script := range []string{
		"echo 'Failed to authenticate: OAuth session expired and could not be refreshed' >&2; exit 1",
		`echo '{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["Failed to authenticate: OAuth session expired and could not be refreshed"]}'`,
	} {
		t.Run(script, func(t *testing.T) {
			path := stubBinary(t, script)
			a := &Adapter{Binary: path, DisableWatchdog: true}
			spec := adapter.RunSpec{WorktreeDir: t.TempDir(), Prompt: "test", PermissionMode: adapter.PermissionFull}
			sess, err := a.Start(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			drain(t, sess)
			_, err = sess.Wait()
			if !errors.Is(err, adapter.ErrLoginRequired) || !strings.Contains(err.Error(), "claude auth login") {
				t.Fatalf("Wait: %v", err)
			}
			writeStatusStub := func(result string) {
				t.Helper()
				body := "#!/bin/sh\ncase \"$1\" in\n--version) echo 'Claude test';;\nauth) echo '{\"loggedIn\":true}';;\n*) echo '" + result + "';;\nesac\n"
				if err := os.WriteFile(path, []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			// A stale login flag must not erase a known vendor rejection.
			writeStatusStub(`{"type":"result","subtype":"error_during_execution","is_error":true}`)
			if err := a.Preflight(context.Background()); !errors.Is(err, adapter.ErrLoginRequired) {
				t.Fatalf("stale login: %v", err)
			}
			writeStatusStub(`{"type":"result","subtype":"success","is_error":false}`)
			if err := a.Preflight(context.Background()); err != nil {
				t.Fatalf("renewed login: %v", err)
			}
			// Healthy preflight no longer needs a model request.
			writeStatusStub(`invalid`)
			if err := a.Preflight(context.Background()); err != nil {
				t.Fatalf("healthy login: %v", err)
			}
		})
	}
}

func TestPreflightRejectsUnreadableLogin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	for _, status := range []string{`{"loggedIn":false}`, `Login expired`, `not json`} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+status+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		a := &Adapter{Binary: path}
		if err := a.Preflight(context.Background()); !errors.Is(err, adapter.ErrPreflight) {
			t.Fatalf("%s: %v", status, err)
		}
	}
}

func TestLoginVerificationIsIsolatedAndFailsClosed(t *testing.T) {
	path := stubBinary(t, `
case "$*" in
 *'--tools  --strict-mcp-config --mcp-config {"mcpServers":{}} --setting-sources  --settings {"disableAllHooks":true} --no-session-persistence --max-turns 1'*) ;;
 *) exit 2;;
esac
case "$PWD" in *rein-claude-auth-*) ;; *) exit 3;; esac
echo '{"type":"result","subtype":"success","is_error":false}'
exit 0`)
	a := &Adapter{}
	if err := a.verifyLogin(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.verifyLogin(ctx, path); !errors.Is(err, adapter.ErrPreflight) || errors.Is(err, adapter.ErrLoginRequired) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestInitialPreflightDoesNotTrustStoredLogin(t *testing.T) {
	path := stubBinary(t, `case "$1" in
 --version) echo 'Claude test'; exit 0;;
 auth) echo '{"loggedIn":true}'; exit 0;;
 *) echo 'Failed to authenticate: OAuth session expired and could not be refreshed' >&2; exit 1;;
 esac`)
	a := &Adapter{Binary: path}
	if err := a.Preflight(context.Background()); !errors.Is(err, adapter.ErrPreflight) || !errors.Is(err, adapter.ErrLoginRequired) {
		t.Fatalf("expired stored login: %v", err)
	}
}
