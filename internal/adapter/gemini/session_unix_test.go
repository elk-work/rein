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

// fakeGemini writes a stand-in for Gemini CLI that behaves as the real one was
// measured to: it refuses to start without GEMINI_API_KEY when its settings
// enforce the key, exits 55 in an untrusted folder, and otherwise replays a
// fixture. It records argv, its home, stdin and whether the key arrived.
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
cp "$GEMINI_CLI_HOME/.gemini/settings.json" "$rec/settings.json" 2>/dev/null
if [ "$GEMINI_API_KEY" = "` + testKey + `" ]; then echo yes > "$rec/key"; fi
if [ -n "$GOOGLE_API_KEY" ]; then echo leaked > "$rec/google"; fi
if ! grep -q '"enforcedType": "gemini-api-key"' "$GEMINI_CLI_HOME/.gemini/settings.json" 2>/dev/null; then
  echo 'Opening authentication page in your browser. Do you want to continue? [Y/n]:' >&2
  sleep 30
fi
if [ -z "$GEMINI_API_KEY" ]; then
  echo 'When using Gemini API, you must specify the GEMINI_API_KEY environment variable.' >&2
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

func testAdapter(t *testing.T, bin string) *Adapter {
	return &Adapter{binary: bin, Root: filepath.Join(t.TempDir(), "gemini"),
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

func TestASessionRunsOnTheKeyUnderAPrivateHome(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", testKey) // in the daemon's environment; must not pass
	bin, rec := fakeGemini(t, "tool.jsonl")
	a := testAdapter(t, bin)
	spec := baseSpec(t)
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
	}
	if !sawTool || !sawEffort {
		t.Errorf("tool_use seen %v, effort note seen %v", sawTool, sawEffort)
	}

	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(rec, name)); return string(b) }
	if read("key") != "yes\n" {
		t.Error("the child did not get the key in GEMINI_API_KEY")
	}
	if read("google") != "" {
		t.Error("GOOGLE_API_KEY reached the child")
	}
	home := read("home")
	if !strings.HasPrefix(home, filepath.Join(a.Root, "runs", "run-1-")) {
		t.Errorf("GEMINI_CLI_HOME = %q, want a private home under %s", home, a.Root)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Error("the private home outlived the session")
	}
	if !strings.Contains(read("settings.json"), `"enforcedType": "gemini-api-key"`) {
		t.Errorf("settings = %s", read("settings.json"))
	}
	if argv := read("argv"); strings.Contains(argv, testKey) || strings.Contains(argv, "do the thing") ||
		!strings.Contains(argv, "--skip-trust") {
		t.Errorf("argv:\n%s", argv)
	}
	if stdin := read("stdin"); !strings.HasPrefix(stdin, "standing rules") || !strings.HasSuffix(stdin, "do the thing") {
		t.Errorf("stdin = %q", stdin)
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

// A Gemini CLI that starts a Google sign-in anyway is stopped at once, and
// the run fails as not on the key.
func TestASignInAttemptIsStopped(t *testing.T) {
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
	if !errors.Is(err, adapter.ErrNotPlanAuth) || !strings.Contains(err.Error(), "Google sign-in") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Error("the sign-in was not stopped promptly")
	}
}

func TestPreflight(t *testing.T) {
	bin, _ := fakeGemini(t, "text.jsonl")
	t.Run("ready", func(t *testing.T) {
		if err := testAdapter(t, bin).Preflight(context.Background()); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
	})
	t.Run("no key", func(t *testing.T) {
		a := testAdapter(t, bin)
		a.Items = items{}
		err := a.Preflight(context.Background())
		if !errors.Is(err, adapter.ErrNoProviderKey) || !errors.Is(err, adapter.ErrPreflight) || err.Error() != MissingKeyLine {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not installed", func(t *testing.T) {
		err := testAdapter(t, filepath.Join(t.TempDir(), "gemini-missing")).Preflight(context.Background())
		if !errors.Is(err, exec.ErrNotFound) || !errors.Is(err, adapter.ErrPreflight) || !strings.Contains(err.Error(), InstallCommand) {
			t.Fatalf("err = %v", err)
		}
	})
}
