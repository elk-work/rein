//go:build integration

package gemini

import (
	"context"
	"errors"
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

// linkedWorktree is a repository and a linked worktree of it, as Rein gives a
// run.
func linkedWorktree(t *testing.T) (repo, wt string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	repo, wt = filepath.Join(root, "repo"), filepath.Join(root, "wt")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", repo, "worktree", "add", "-q", "-b", "task", wt},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return repo, wt
}

func liveSpec(t *testing.T, wt string) adapter.RunSpec {
	return adapter.RunSpec{RunID: "live-1", WorktreeDir: wt, Prompt: "do the task",
		PermissionMode: adapter.PermissionFull, ProviderKey: "AIza_FAKE_not_a_real_key_for_rein_tests_000",
		Timeouts: adapter.Timeouts{Total: 3 * time.Minute}}
}

func liveRun(t *testing.T, a *Adapter, spec adapter.RunSpec) (adapter.Result, error) {
	t.Helper()
	s, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for ev := range s.Events() {
		if ev.Kind == adapter.EventToolUse {
			t.Logf("tool: %s %s", ev.Tool.Name, string(ev.Tool.Input))
		}
	}
	return s.Wait()
}

// A person whose Gemini CLI chooses a Google sign-in but whose key is in its
// own .env: the run reads the key through the private home's link and goes
// to the Gemini API, never the sign-in, even when that .env also says
// GOOGLE_GENAI_USE_GCA=true. Google's refusal of a key that is not a key is
// the proof the key, and not a sign-in, was used.
func TestLiveTheKeyComesFromItsEnvFileNeverASignIn(t *testing.T) {
	for _, c := range []struct{ name, env string }{
		{"key only", "GEMINI_API_KEY=" + testKey + "\n"},
		{"key and the sign-in switch", "GOOGLE_GENAI_USE_GCA=true\nGEMINI_API_KEY=" + testKey + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := liveAdapter(t)
			a.Items = items{}
			conf := filepath.Join(a.RealHome, ".gemini")
			must(t, os.MkdirAll(conf, 0o700))
			must(t, os.WriteFile(filepath.Join(conf, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0o600))
			must(t, os.WriteFile(filepath.Join(conf, ".env"), []byte(c.env), 0o600))
			spec := baseSpec(t)
			spec.ProviderKey = ""
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
				t.Fatalf("want Google's own refusal of the key from .env; got %v", err)
			}
		})
	}
}

// A person set to the Gemini API key with no key anywhere Rein lets through
// (no .env, nothing stored, and the daemon's own GEMINI_API_KEY never passed
// on) gets the not-set-up line from the real Gemini CLI's own refusal.
func TestLiveAMissingKeyIsNotSetUp(t *testing.T) {
	a := liveAdapter(t)
	a.Items = items{}
	conf := filepath.Join(a.RealHome, ".gemini")
	must(t, os.MkdirAll(conf, 0o700))
	must(t, os.WriteFile(filepath.Join(conf, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`), 0o600))
	spec := baseSpec(t)
	spec.ProviderKey = ""
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
	if !errors.Is(err, adapter.ErrLoginRequired) || !strings.Contains(err.Error(), NotSetUpLine) {
		t.Fatalf("want the not-set-up line; got %v", err)
	}
}
