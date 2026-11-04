//go:build integration

package gemini

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// The real Gemini CLI, a private home, and a key Google will refuse. Run with:
//
//	REIN_GEMINI_LIVE=1 go test -tags integration ./internal/adapter/gemini/ -run Live
//
// It sends one short prompt and a key that is not a key to Google.

func liveAdapter(t *testing.T) *Adapter {
	t.Helper()
	if os.Getenv("REIN_GEMINI_LIVE") != "1" {
		t.Skip("set REIN_GEMINI_LIVE=1 to run against the real Gemini CLI")
	}
	bin, err := exec.LookPath(Binary)
	if err != nil {
		t.Skipf("Gemini CLI is not installed: %v", err)
	}
	return &Adapter{binary: bin, Root: t.TempDir(), Items: items{KeyService + "/" + KeyName: testKey}}
}

// The whole path: the private home puts Gemini CLI on the key, the key reaches
// it, it goes to the Gemini API and not a Google sign-in, and Google's refusal
// comes back as the run's error.
func TestLiveARefusedKeyComesBackInGooglesWords(t *testing.T) {
	a := liveAdapter(t)
	if err := a.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	spec := baseSpec(t)
	spec.PermissionMode = adapter.PermissionReadOnly
	spec.Model = "gemini-2.5-flash"
	spec.Prompt = "Reply with the single word: hi"
	spec.Timeouts.Total = 3 * time.Minute
	s, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for range s.Events() {
	}
	res, err := s.Wait()
	t.Logf("status %s, exit %d, err %v", res.Status, res.ExitCode, err)
	if err == nil || !strings.Contains(err.Error(), "API key not valid") {
		t.Fatalf("want Google's own refusal of the key; got %v", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Error("the error carries the key")
	}
}

// ark:rein#119: in a folder Gemini CLI does not trust it connects no MCP
// server. The run's environment trusts the worktree, so the run's own server
// is enabled.
func TestLiveTheWorktreeIsTrustedSoItsServersConnect(t *testing.T) {
	a := liveAdapter(t)
	spec := baseSpec(t)
	srv, err := a.MCPServerConfig(adapter.MCPServerSpec{Name: "probe", URL: "http://127.0.0.1:9/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	spec.MCPServers = map[string]any{"probe": srv}
	h, err := a.newHome(spec)
	if err != nil {
		t.Fatal(err)
	}
	defer h.remove()

	list := func(env []string) string {
		cmd := exec.Command(a.binary, "mcp", "list")
		cmd.Dir = spec.WorktreeDir
		cmd.Env = env
		out, _ := cmd.CombinedOutput() // it prints to stderr
		return string(out)
	}
	trusted := list(h.childEnv(spec.InheritedEnv(), nil, testKey))
	t.Logf("trusted:\n%s", trusted)
	if !strings.Contains(trusted, "probe") || strings.Contains(trusted, "untrusted") || strings.Contains(trusted, "Disabled") {
		t.Errorf("the run's server is not enabled in its worktree:\n%s", trusted)
	}
	// And the control: the same home without the trust switch.
	var untrustedEnv []string
	for _, kv := range h.childEnv(spec.InheritedEnv(), nil, testKey) {
		if !strings.HasPrefix(kv, "GEMINI_CLI_TRUST_WORKSPACE=") {
			untrustedEnv = append(untrustedEnv, kv)
		}
	}
	if untrusted := list(untrustedEnv); !strings.Contains(untrusted, "untrusted") {
		t.Logf("control did not show the untrusted warning (Gemini CLI may have changed):\n%s", untrusted)
	}
}
