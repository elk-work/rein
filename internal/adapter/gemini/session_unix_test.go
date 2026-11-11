//go:build !windows

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

// fakeGemini writes a stand-in for Gemini CLI that behaves as the real one
// does with each sign-in choice: on the key it refuses to start without
// GEMINI_API_KEY; on a Google sign-in it runs (the sign-in is in the
// keychain); with no choice and no key it asks for one; in an untrusted folder
// it exits 55; otherwise it replays a fixture. It records argv, its home,
// settings, stdin, whether the key arrived and whether the person's sign-in
// file is reachable.
func fakeGemini(t *testing.T, fixture string) (bin, rec string) {
	t.Helper()
	dir := t.TempDir()
	rec = filepath.Join(dir, "rec")
	must(t, os.MkdirAll(rec, 0o700))
	fix, err := filepath.Abs(filepath.Join("testdata", fixture))
	must(t, err)
	script := `#!/bin/sh
rec="` + rec + `"
if [ "$1" = "--version" ]; then echo 0.64.0; exit 0; fi
printf '%s\n' "$@" > "$rec/argv"
printf '%s' "$GEMINI_CLI_HOME" > "$rec/home"
s="$GEMINI_CLI_HOME/.gemini/settings.json"
cp "$s" "$rec/settings.json" 2>/dev/null
cat "$GEMINI_CLI_HOME/.gemini/oauth_creds.json" > "$rec/creds" 2>/dev/null
if [ "$GEMINI_API_KEY" = "` + testKey + `" ]; then echo yes > "$rec/key"; fi
if [ -n "$GOOGLE_API_KEY" ]; then echo leaked > "$rec/google"; fi
if grep -q '"selectedType": "gemini-api-key"' "$s" 2>/dev/null; then
  if [ -z "$GEMINI_API_KEY" ] && [ ! -e "$GEMINI_CLI_HOME/.gemini/.env" ]; then
    echo 'When using Gemini API, you must specify the GEMINI_API_KEY environment variable.' >&2
    exit 41
  fi
elif ! grep -q '"selectedType"' "$s" 2>/dev/null; then
  echo 'Please set an Auth method in your settings.json.' >&2
  exit 41
fi
if [ "$GEMINI_CLI_TRUST_WORKSPACE" != "true" ]; then
  echo 'Gemini CLI is not running in a trusted directory.' >&2
  exit 55
fi
cat > "$rec/stdin"
cat "` + fix + `"
case "` + fixture + `" in badkey.jsonl) exit 1;; esac
exit 0
`
	bin = filepath.Join(dir, "gemini")
	must(t, os.WriteFile(bin, []byte(script), 0o755))
	return bin, rec
}

// personsHome is a person's own Gemini CLI home whose settings choose auth,
// signed in with Google too (oauth_creds.json), with a .env when env is set.
func personsHome(t *testing.T, auth string, env bool) string {
	t.Helper()
	real := t.TempDir()
	conf := filepath.Join(real, ".gemini")
	must(t, os.MkdirAll(conf, 0o700))
	must(t, os.WriteFile(filepath.Join(conf, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"`+auth+`"}}}`), 0o600))
	must(t, os.WriteFile(filepath.Join(conf, "oauth_creds.json"), []byte("their-sign-in"), 0o600))
	if env {
		must(t, os.WriteFile(filepath.Join(conf, ".env"), []byte("# not read by Rein\n"), 0o600))
	}
	return real
}

func testAdapter(t *testing.T, bin string) *Adapter {
	return &Adapter{binary: bin, Root: filepath.Join(t.TempDir(), "gemini"), RealHome: t.TempDir(),
		Items: items{KeyService + "/" + KeyName: testKey}, InterruptGrace: time.Second}
}

func drain(t *testing.T, s adapter.Session) []adapter.Event {
	t.Helper()
	var evs []adapter.Event
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-timeout:
			t.Fatal("the session did not end")
		}
	}
}

// A person signed in to Gemini CLI with Google, no key anywhere: the run goes
// on that sign-in under a private home. On the old code it was refused before
// starting ("Add your Google key in Elk to use Gemini.").
// A person whose Gemini CLI is on their own key (here Vertex AI, which needs
// no key from Rein) runs under a private home that carries that choice and
// never their Google sign-in. On the old code the sign-in file was linked.
func TestASessionRunsOnThePersonsKeySetUpUnderAPrivateHome(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", testKey) // in the daemon's environment; metered, must not pass
	bin, rec := fakeGemini(t, "tool.jsonl")
	a := testAdapter(t, bin)
	a.RealHome = personsHome(t, "vertex-ai", false)
	a.Items = items{}
	spec := baseSpec(t)
	spec.ProviderKey = ""
	spec.SystemPrompt = "standing rules"
	spec.Effort = "high"
	spec.PassEnv = []string{"GOOGLE_API_KEY"}

	s, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, s)
	res, err := s.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Status != adapter.StatusSucceeded || res.Summary != "Hello from fake." {
		t.Errorf("result = %+v", res)
	}
	if s.ID() != "22222222-2222-4222-8222-222222222222" || res.SessionID != s.ID() {
		t.Errorf("session id = %q / %q", s.ID(), res.SessionID)
	}
	if res.Usage.InputTokens != 20 || res.Usage.OutputTokens != 8 || res.Usage.Model != "gemini-test-pro" {
		t.Errorf("usage = %+v", res.Usage)
	}
	if last := evs[len(evs)-1]; last.Kind != adapter.EventDone {
		t.Errorf("last event = %v", last)
	}
	var sawTool, sawEffort bool
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			t.Errorf("event %d has seq %d", i, ev.Seq)
		}
		sawTool = sawTool || ev.Kind == adapter.EventToolUse && ev.Tool.Name == "write_file"
		sawEffort = sawEffort || strings.Contains(ev.Text, "effort high was not applied")
		if strings.Contains(ev.Text, "Google key") {
			t.Errorf("the run says it is on a key: %q", ev.Text)
		}
	}
	if !sawTool || !sawEffort {
		t.Errorf("tool_use seen %v, effort note seen %v", sawTool, sawEffort)
	}

	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(rec, name)); return string(b) }
	if read("key") != "" {
		t.Error("a key reached the child though none is stored")
	}
	if read("google") != "" {
		t.Error("GOOGLE_API_KEY reached the child")
	}
	if read("creds") != "" {
		t.Error("the person's Google sign-in reached the private home")
	}
	home := read("home")
	if !strings.HasPrefix(home, filepath.Join(a.Root, "runs", "run-1-")) {
		t.Errorf("GEMINI_CLI_HOME = %q, want a private home under %s", home, a.Root)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Error("the private home outlived the session")
	}
	if set := read("settings.json"); !strings.Contains(set, `"selectedType": "vertex-ai"`) || strings.Contains(set, "enforcedType") {
		t.Errorf("settings = %s", set)
	}
	if argv := read("argv"); strings.Contains(argv, "do the thing") || !strings.Contains(argv, "--skip-trust") {
		t.Errorf("argv:\n%s", argv)
	}
	if stdin := read("stdin"); !strings.HasPrefix(stdin, "standing rules") || !strings.HasSuffix(stdin, "do the thing") {
		t.Errorf("stdin = %q", stdin)
	}
}

// A person with no sign-in chosen and a key stored for Rein: the run goes on
// that key, given once in the environment and never on the command line.
func TestAStoredKeyRunsGeminiCLIOnTheKey(t *testing.T) {
	bin, rec := fakeGemini(t, "tool.jsonl")
	s, err := testAdapter(t, bin).Start(context.Background(), baseSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	if res, err := s.Wait(); err != nil || res.Status != adapter.StatusSucceeded {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(rec, name)); return string(b) }
	if read("key") != "yes\n" {
		t.Error("the child did not get the key in GEMINI_API_KEY")
	}
	if !strings.Contains(read("settings.json"), `"selectedType": "gemini-api-key"`) {
		t.Errorf("settings = %s", read("settings.json"))
	}
	if strings.Contains(read("argv"), testKey) {
		t.Error("the key is on the command line")
	}
}

// A person signed in to Gemini CLI with their personal Google account and no
// key: nothing starts, and the run says to set Gemini CLI up on a key.
// A .env beside the same sign-in puts the run on the key instead.
func TestAPersonalGoogleSignInNeverRuns(t *testing.T) {
	bin, rec := fakeGemini(t, "text.jsonl")
	a := testAdapter(t, bin)
	a.RealHome = personsHome(t, "oauth-personal", false)
	a.Items = items{}
	spec := baseSpec(t)
	spec.ProviderKey = ""
	_, err := a.Start(context.Background(), spec)
	if !errors.Is(err, adapter.ErrLoginRequired) || !strings.Contains(err.Error(), NotSetUpLine) {
		t.Fatalf("err = %v, want the not-set-up refusal", err)
	}
	if _, err := os.Stat(filepath.Join(rec, "argv")); err == nil {
		t.Error("Gemini CLI was started")
	}

	a.RealHome = personsHome(t, "oauth-personal", true)
	s, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	if res, err := s.Wait(); err != nil || res.Status != adapter.StatusSucceeded {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	b, _ := os.ReadFile(filepath.Join(rec, "settings.json"))
	if !strings.Contains(string(b), `"selectedType": "gemini-api-key"`) {
		t.Errorf("settings = %s", b)
	}
	if c, _ := os.ReadFile(filepath.Join(rec, "creds")); len(c) != 0 {
		t.Error("the person's Google sign-in reached the private home")
	}
}

// A repository whose own Gemini settings choose a Google sign-in is refused
// before anything starts: the workspace layer outranks the private home's.
func TestARepositoryThatChoosesASignInIsRefused(t *testing.T) {
	bin, rec := fakeGemini(t, "text.jsonl")
	a := testAdapter(t, bin)
	spec := baseSpec(t)
	must(t, os.MkdirAll(filepath.Join(spec.WorktreeDir, ".gemini"), 0o700))
	must(t, os.WriteFile(filepath.Join(spec.WorktreeDir, ".gemini", "settings.json"),
		[]byte(`{"security":{"auth":{"selectedType":"oauth-personal","enforcedType":"oauth-personal"}}}`), 0o600))
	if _, err := a.Start(context.Background(), spec); !errors.Is(err, adapter.ErrNotPlanAuth) {
		t.Fatalf("err = %v, want a sign-in refusal", err)
	}
	if _, err := os.Stat(filepath.Join(rec, "argv")); err == nil {
		t.Error("Gemini CLI was started")
	}
}

func TestABadKeyEndsTheRunWithGooglesWords(t *testing.T) {
	bin, _ := fakeGemini(t, "badkey.jsonl")
	s, err := testAdapter(t, bin).Start(context.Background(), baseSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, s)
	res, err := s.Wait()
	if err == nil || res.Status != adapter.StatusFailed || !strings.Contains(err.Error(), "API key not valid") {
		t.Fatalf("status %s, err %v", res.Status, err)
	}
	if last := evs[len(evs)-1]; last.Kind != adapter.EventError {
		t.Errorf("last event = %v", last)
	}
}

// A Gemini CLI that is not signed in starts a Google sign-in nobody is there
// to finish: it is stopped at once and the run fails as "set up Gemini CLI
// first", a login failure (agent_login), not a refusal of the person's
// sign-in.
func TestAGeminiCLIThatIsNotSignedInFailsAsNotSetUp(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gemini")
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\necho 'Opening authentication page in your browser.' >&2\nsleep 30\n"), 0o755))
	start := time.Now()
	s, err := testAdapter(t, bin).Start(context.Background(), baseSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	_, err = s.Wait()
	if !errors.Is(err, adapter.ErrLoginRequired) || errors.Is(err, adapter.ErrNotPlanAuth) ||
		!strings.Contains(err.Error(), NotSetUpLine) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Error("the run waited for the sign-in")
	}
}

func TestPreflight(t *testing.T) {
	bin, _ := fakeGemini(t, "text.jsonl")
	t.Run("Vertex AI, no key", func(t *testing.T) {
		a := testAdapter(t, bin)
		a.RealHome = personsHome(t, "vertex-ai", false)
		a.Items = items{}
		if err := a.Preflight(context.Background()); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
	})
	t.Run("a personal Google sign-in, no key", func(t *testing.T) {
		a := testAdapter(t, bin)
		a.RealHome = personsHome(t, "oauth-personal", false)
		a.Items = items{}
		err := a.Preflight(context.Background())
		if !errors.Is(err, adapter.ErrLoginRequired) || !errors.Is(err, adapter.ErrPreflight) ||
			!strings.HasPrefix(err.Error(), NotSetUpLine) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a stored key, no sign-in chosen", func(t *testing.T) {
		if err := testAdapter(t, bin).Preflight(context.Background()); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
	})
	t.Run("a key in its own .env, no sign-in chosen", func(t *testing.T) {
		home := t.TempDir()
		must(t, os.MkdirAll(filepath.Join(home, ".gemini"), 0o700))
		must(t, os.WriteFile(filepath.Join(home, ".gemini", ".env"), []byte("# not read by Rein\n"), 0o600))
		a := testAdapter(t, bin)
		a.RealHome = home
		a.Items = items{}
		if err := a.Preflight(context.Background()); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
	})
	t.Run("not set up", func(t *testing.T) {
		a := testAdapter(t, bin)
		a.Items = items{}
		err := a.Preflight(context.Background())
		if !errors.Is(err, adapter.ErrLoginRequired) || !errors.Is(err, adapter.ErrPreflight) ||
			!strings.Contains(err.Error(), NotSetUpLine) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not installed", func(t *testing.T) {
		err := testAdapter(t, filepath.Join(t.TempDir(), "gemini-missing")).Preflight(context.Background())
		if !errors.Is(err, exec.ErrNotFound) || !errors.Is(err, adapter.ErrPreflight) ||
			!strings.Contains(err.Error(), NotSetUpLine) {
			t.Fatalf("err = %v", err)
		}
	})
}
