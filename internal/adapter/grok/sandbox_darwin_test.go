package grok

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

// Exercise the production launch wrapper without a model, login or real remote.
// The HTTP server runs outside the sandbox, just as a remote Git server does.
func TestMacSandboxCommitPushAndDeny(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main, work, home := filepath.Join(root, "main"), filepath.Join(root, "linked"), filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gitPath, args...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", main)
	git("-C", main, "config", "user.name", "Rein Test")
	git("-C", main, "config", "user.email", "test@example.invalid")
	git("-C", main, "commit", "--allow-empty", "-m", "initial")
	git("-C", main, "worktree", "add", "-b", "probe", work)
	remote := filepath.Join(root, "remote.git")
	git("init", "--bare", remote)
	git("-C", remote, "config", "http.receivepack", "true")
	backend := filepath.Join(git("--exec-path"), "git-http-backend")
	handler := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	git("-C", main, "remote", "add", "origin", server.URL+"/remote.git")
	outside := filepath.Join(root, "outside")
	marker := filepath.Join(work, "helper-ran")
	// A fake credential helper exercises Git's real HTTP credential round trip.
	// It also proves helper descendants inherit the filesystem boundary.
	helper := filepath.Join(home, "helper")
	script := "#!/bin/sh\nif touch \"$OUTSIDE\" 2>/dev/null; then exit 99; fi\ntouch \"$MARKER\"\nprintf 'username=test\\npassword=synthetic\\n'\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	h := &privateHome{dir: home}
	spec := adapter.RunSpec{WorktreeDir: work, Env: map[string]string{sandboxEnv: sandboxMacOS, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_TERMINAL_PROMPT": "0", "OUTSIDE": outside, "MARKER": marker}}
	run := func(binary string, args ...string) ([]byte, error) {
		t.Helper()
		cmd, err := h.sandboxCommand(context.Background(), binary, args, spec)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Dir = work
		cmd.Env = h.commandEnv(spec)
		return cmd.CombinedOutput()
	}
	for _, target := range []string{outside, filepath.Join(main, "forbidden"), filepath.Join(main, ".git", "config"), filepath.Join(home, "sandbox.sb")} {
		if out, err := run("/bin/sh", "-c", `printf forbidden >> "$1"`, "probe", target); err == nil {
			t.Fatalf("write allowed: %s: %s", target, out)
		}
	}
	// A symlink inside the checkout cannot make a sibling writable.
	if err := os.Symlink(root, filepath.Join(work, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := run("/usr/bin/touch", filepath.Join(work, "escape", "outside")); err == nil {
		t.Fatal("symlink escaped sandbox")
	}
	if out, err := run("/bin/sh", "-c", "printf work > change.txt"); err != nil {
		t.Fatalf("checkout write: %v: %s", err, out)
	}
	for _, args := range [][]string{
		{"add", "change.txt"},
		{"-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-m", "sandbox commit"},
		{"-c", "credential.helper=", "-c", "credential.helper=" + helper, "push", "-u", "origin", "HEAD"},
	} {
		if out, err := run(gitPath, args...); err != nil {
			t.Fatalf("sandbox git %v: %v: %s", args, err, out)
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("credential helper did not run", err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("outside write succeeded", err)
	}
	if got, want := git("-C", remote, "rev-parse", "refs/heads/probe"), git("-C", work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("push %s != %s", got, want)
	}
	if out, err := run("/bin/sh", "-c", `touch "$TMPDIR/scratch"`); err != nil {
		t.Fatalf("scratch: %v: %s", err, out)
	}
	t.Log("native sandbox: linked commit and authenticated fake HTTP push succeeded; sibling, Git config, profile and symlink writes refused; helper inherited denial")
}

func TestMacSandboxInvalidProfileFailsClosed(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "invalid.sb")
	if err := os.WriteFile(profile, []byte("not a seatbelt profile"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "must-not-run")
	if err := exec.Command("/usr/bin/sandbox-exec", "-f", profile, "/usr/bin/touch", marker).Run(); err == nil {
		t.Fatal("invalid profile succeeded")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("command ran without enforcement")
	}
}
