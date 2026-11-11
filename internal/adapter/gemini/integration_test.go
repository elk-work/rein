//go:build integration

package gemini

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// The real Gemini CLI, a private home, and a key Google will refuse. Run with:
//
//	REIN_GEMINI_LIVE=1 go test -tags integration ./internal/adapter/gemini/ -run Live
//
// It sends one short prompt and a key that is not a key to Google. With
// REIN_GEMINI_LIVE_KEY_FILE naming a file that holds a real paid key, it also
// sends one small task to Google on that key and checks the file and commit
// it makes (the lab proof of ark:rein#151, on Windows too).

func liveAdapter(t *testing.T) *Adapter {
	t.Helper()
	if os.Getenv("REIN_GEMINI_LIVE") != "1" {
		t.Skip("set REIN_GEMINI_LIVE=1 to run against the real Gemini CLI")
	}
	a := &Adapter{binary: Binary}
	bin, err := a.resolveBinary()
	if err != nil {
		t.Skipf("Gemini CLI is not installed: %v", err)
	}
	// A home with no sign-in chosen, so the stored key is the setup.
	return &Adapter{binary: bin, Root: t.TempDir(), RealHome: t.TempDir(), Items: items{KeyService + "/" + KeyName: testKey}}
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
	h, err := a.newHome(spec, testKey)
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

// A real paid key: one small task, sent to Google, finished in the folder.
func TestLiveGeminiCLIRealKeyFinishesATask(t *testing.T) {
	path := os.Getenv("REIN_GEMINI_LIVE_KEY_FILE")
	if path == "" {
		t.Skip("set REIN_GEMINI_LIVE_KEY_FILE to a file holding a real paid key")
	}
	a := liveAdapter(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the key file: %v", err)
	}
	_, wt := linkedWorktree(t)
	spec := liveSpec(t, wt)
	spec.ProviderKey = strings.TrimSpace(string(raw))
	spec.Model = os.Getenv("REIN_GEMINI_LIVE_MODEL")
	spec.Prompt = realKeyTask
	res, err := liveRun(t, a, spec)
	if err != nil || res.Status != adapter.StatusSucceeded {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	b, err := os.ReadFile(filepath.Join(wt, "hello.txt"))
	if err != nil || !strings.Contains(string(b), "hi") {
		t.Fatalf("hello.txt: %q %v", b, err)
	}
	out, _ := exec.Command("git", "-C", wt, "log", "--oneline", "-1").Output()
	if !strings.Contains(string(out), "hello") {
		t.Errorf("the commit did not land: %q", out)
	}
	t.Logf("last commit: %s; usage %+v; summary %q", strings.TrimSpace(string(out)), res.Usage, res.Summary)
}

// realKeyTask is the one small task both programs' real-key tests send. The
// git identity is given, since a lab machine may have none.
const realKeyTask = "Create a file named hello.txt in the current folder containing exactly the word hi, " +
	"then commit it by running: git add hello.txt && git -c user.name=lab -c user.email=lab@example.invalid " +
	"commit -m hello . Do nothing else."
