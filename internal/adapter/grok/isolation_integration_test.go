//go:build integration

package grok

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// Keep the fixture outside OS temp directories: workspace deliberately permits
// writes there, which would make an outside-worktree denial test meaningless.
func TestIntegrationLinkedWorktreeSandbox(t *testing.T) {
	cwd, _ := os.Getwd()
	root, err := os.MkdirTemp(cwd, "rein-isolation-probe-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	main, work := filepath.Join(root, "main"), filepath.Join(root, "linked")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	git("init", main)
	git("-C", main, "-c", "user.name=Rein Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "initial")
	git("-C", main, "worktree", "add", "-b", "probe", work)
	// The vendor may read its own login, but this probe writes session state
	// only inside our fixture, never the person's session archive.
	real, err := realGrokHome()
	if err != nil {
		t.Fatal(err)
	}
	loginHome := filepath.Join(root, "login-home")
	if err := os.MkdirAll(filepath.Join(loginHome, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "auth.json"), filepath.Join(loginHome, "auth.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", loginHome)
	h, err := newPrivateHome("isolation-probe")
	if err != nil {
		t.Fatal(err)
	}
	defer h.remove()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	spec := adapter.RunSpec{WorktreeDir: work, Env: map[string]string{sandboxEnv: sandboxWorktree}}
	if err := h.configure(ctx, Binary, spec); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"workspace", sandboxWorktree} {
		prompt := "Execute this shell command exactly once; report the output and do nothing else: " +
			"printf probe > probe.txt; git add probe.txt; git -c user.name='Rein Test' -c user.email=test@example.invalid commit -m probe; printf 'COMMIT_EXIT=%s\\n' \"$?\"; printf forbidden > ../outside; printf 'OUTSIDE_EXIT=%s\\n' \"$?\""
		cmd := exec.CommandContext(ctx, Binary, "--sandbox", profile, "--always-approve", "--no-subagents", "--max-turns", "3", "-p", prompt)
		cmd.Dir = work
		cmd.Env = h.runEnv(os.Environ(), nil)
		if profile == "workspace" {
			// The baseline cannot write linked session storage either. Keep its
			// sessions private so the probe reaches the Git operation.
			if err := os.Remove(filepath.Join(h.dir, "sessions")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(h.dir, "sessions"), 0700); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.RemoveAll(filepath.Join(h.dir, "sessions")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(loginHome, "sessions"), filepath.Join(h.dir, "sessions")); err != nil {
				t.Fatal(err)
			}
		}
		out, err := cmd.CombinedOutput()
		t.Logf("grok --sandbox %s --always-approve --no-subagents --max-turns 3 -p %q\n%s", profile, prompt, out)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if !strings.Contains(string(out), "OUTSIDE_EXIT=1") {
			t.Fatalf("missing evidence of refused outside attempt: %s", out)
		}
		check := exec.Command("git", "-C", work, "log", "-1", "--format=%s")
		head, err := check.Output()
		if err != nil {
			t.Fatal(err)
		}
		if profile == "workspace" && string(head) != "initial\n" {
			t.Fatalf("baseline committed: %s", head)
		}
		if profile == sandboxWorktree && string(head) != "probe\n" {
			t.Fatalf("custom sandbox did not commit: %s", head)
		}
		if _, err := os.Stat(filepath.Join(root, "outside")); !os.IsNotExist(err) {
			t.Fatalf("outside write was permitted: %v", err)
		}
	}
	sess, err := New().Start(ctx, adapter.RunSpec{
		RunID: "isolation-acp", WorktreeDir: work, PermissionMode: adapter.PermissionFull,
		Prompt: "Without calling tools, reply with exactly pong.",
	})
	if err != nil {
		t.Fatal(err)
	}
	sawServers := false
	for ev := range sess.Events() {
		if ev.Kind == adapter.EventProgress && ev.Text == "_x.ai/mcp/servers_updated" {
			var n struct {
				MCPServers []struct {
					Name string `json:"name"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(ev.Raw, &n); err != nil {
				t.Fatal(err)
			}
			sawServers = true
			if len(n.MCPServers) != 0 {
				t.Errorf("unexpected MCP servers: %v", n.MCPServers)
			}
		}
	}
	result, err := sess.Wait()
	if err != nil || result.Status != adapter.StatusSucceeded {
		t.Fatalf("ACP session: %v, %v", result.Status, err)
	}
	if !sawServers {
		t.Fatal("missing MCP inventory notification")
	}
	t.Log("Full adapter Start succeeded; MCP inventory was empty with no RunSpec.MCPServers")

}
