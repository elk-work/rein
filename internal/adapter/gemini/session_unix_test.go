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

// fakeAgy writes a stand-in for agy that behaves as the real one was measured
// to: with modelProvider gemini in $HOME's settings it refuses to start
// without GEMINI_API_KEY, `models` prints the table, and a print run replays a
// fixture. It records its argv, its environment's names and stdin, so a test
// can check what reached it.
func fakeAgy(t *testing.T, fixture string) (bin, rec string) {
	t.Helper()
	dir := t.TempDir()
	rec = filepath.Join(dir, "rec")
	must(t, os.MkdirAll(rec, 0o700))
	fix, err := filepath.Abs(filepath.Join("testdata", fixture))
	must(t, err)
	script := `#!/bin/sh
rec="` + rec + `"
printf '%s\n' "$@" > "$rec/argv"
env | cut -d= -f1 | sort > "$rec/envnames"
printf '%s' "$HOME" > "$rec/home"
if [ "$GEMINI_API_KEY" = "` + testKey + `" ]; then echo yes > "$rec/key"; fi
if [ -n "$GOOGLE_API_KEY" ]; then echo leaked > "$rec/google"; fi
if grep -q '"modelProvider": *"gemini"' "$HOME/.gemini/antigravity-cli/settings.json" 2>/dev/null; then
  if [ -z "$GEMINI_API_KEY" ]; then
    echo 'modelProvider is set to "gemini" in settings.json, but the GEMINI_API_KEY environment variable is not set.'
    exit 0
  fi
else
  echo 'Error: Please sign in to view available models.'
  exit 0
fi
if [ "$1" = "models" ]; then
  echo 'Fetching available models...'
  printf 'gemini-3.1-pro-high\tGemini 3.1 Pro (High)\n'
  exit 0
fi
cat > "$rec/stdin"
cat "` + fix + `"
case "` + fixture + `" in badkey.jsonl) exit 3;; esac
exit 0
`
	bin = filepath.Join(dir, "agy")
	must(t, os.WriteFile(bin, []byte(script), 0o755))
	return bin, rec
}

func testAdapter(t *testing.T, bin string) *Adapter {
	real := t.TempDir()
	must(t, os.WriteFile(filepath.Join(real, ".gitconfig"), []byte("[user]\n"), 0o600))
	return &Adapter{binary: bin, Root: filepath.Join(t.TempDir(), "gemini"), RealHome: real,
		Items: items{KeyService + "/" + KeyName: testKey}}
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

func TestASessionRunsOnTheKeyUnderThePrivateHome(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", testKey) // in the daemon's environment; must not pass
	bin, rec := fakeAgy(t, "tool.jsonl")
	a := testAdapter(t, bin)
	spec := baseSpec(t)
	spec.SystemPrompt = "standing rules"
	spec.Model = "gemini-3.1-pro-high"
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
	if res.Usage.InputTokens != 20 || res.Usage.OutputTokens != 8 || res.Usage.Model != "gemini-3.1-pro-high" {
		t.Errorf("usage = %+v", res.Usage)
	}
	if last := evs[len(evs)-1]; last.Kind != adapter.EventDone {
		t.Errorf("last event = %v", last)
	}
	var sawTool bool
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			t.Errorf("event %d has seq %d", i, ev.Seq)
		}
		if ev.Kind == adapter.EventToolUse && ev.Tool.Name == "list_dir" {
			sawTool = true
		}
	}
	if !sawTool {
		t.Error("no tool_use event")
	}

	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(rec, name)); return string(b) }
	if read("key") != "yes\n" {
		t.Error("the child did not get the key in GEMINI_API_KEY")
	}
	if read("google") != "" {
		t.Error("GOOGLE_API_KEY reached the child")
	}
	if got := read("home"); got != filepath.Join(a.Root, "home") {
		t.Errorf("HOME = %q, want the private home", got)
	}
	if strings.Contains(read("argv"), testKey) || strings.Contains(read("argv"), "do the thing") {
		t.Errorf("argv carries the key or the prompt:\n%s", read("argv"))
	}
	if stdin := read("stdin"); !strings.Contains(stdin, `"event":"user"`) || !strings.Contains(stdin, "standing rules") ||
		!strings.Contains(stdin, "do the thing") {
		t.Errorf("stdin = %q", stdin)
	}
	// The run is recorded for the reaper.
	if b, _ := os.ReadFile(filepath.Join(a.Root, "runs", "run-1")); strings.TrimSpace(string(b)) != s.ID() {
		t.Errorf("run record = %q", b)
	}
}

func TestABadKeyEndsTheRunWithGooglesWords(t *testing.T) {
	bin, _ := fakeAgy(t, "badkey.jsonl")
	a := testAdapter(t, bin)
	s, err := a.Start(context.Background(), baseSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, s)
	res, err := s.Wait()
	if err == nil || res.Status != adapter.StatusFailed {
		t.Fatalf("status %s, err %v", res.Status, err)
	}
	if !strings.Contains(err.Error(), "API key not valid") || res.ExitCode != 3 {
		t.Errorf("err = %v, exit %d", err, res.ExitCode)
	}
	if last := evs[len(evs)-1]; last.Kind != adapter.EventError {
		t.Errorf("last event = %v", last)
	}
}

func TestPreflight(t *testing.T) {
	bin, rec := fakeAgy(t, "text.jsonl")

	t.Run("ready", func(t *testing.T) {
		a := testAdapter(t, bin)
		if err := a.Preflight(context.Background()); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if b, _ := os.ReadFile(filepath.Join(rec, "argv")); strings.TrimSpace(string(b)) != "models" {
			t.Errorf("the probe ran %q, want `agy models`", b)
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
		a := testAdapter(t, filepath.Join(t.TempDir(), "agy-missing"))
		err := a.Preflight(context.Background())
		if !errors.Is(err, exec.ErrNotFound) || !errors.Is(err, adapter.ErrPreflight) || !strings.Contains(err.Error(), InstallCommand) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("key mode not on", func(t *testing.T) {
		// A program that ignores the private home's settings is caught: it
		// says "sign in", which is not a model list.
		dir := t.TempDir()
		odd := filepath.Join(dir, "agy")
		must(t, os.WriteFile(odd, []byte("#!/bin/sh\necho 'Error: Please sign in to view available models.'\n"), 0o755))
		a := testAdapter(t, odd)
		if err := a.Preflight(context.Background()); err == nil || !errors.Is(err, adapter.ErrPreflight) {
			t.Fatalf("err = %v", err)
		}
	})
}
