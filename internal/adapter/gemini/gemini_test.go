package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/secretenv"
)

// testKey is long and distinctive so a search for it cannot match anything
// else. It is not a key.
const testKey = "AIza_TESTVALUE_not_a_real_google_key_0123456789"

type items map[string]string

func (i items) Name() string { return "test" }
func (i items) ReadItem(service, account string) (string, error) {
	if v, ok := i[service+"/"+account]; ok {
		return v, nil
	}
	return "", fmt.Errorf("%w: %s/%s", keyring.ErrItemNotFound, service, account)
}

func baseSpec(t *testing.T) adapter.RunSpec {
	return adapter.RunSpec{
		RunID:          "run-1",
		WorktreeDir:    t.TempDir(),
		Prompt:         "do the thing",
		PermissionMode: adapter.PermissionFull,
		ProviderKey:    testKey,
	}
}

func TestRegisteredAsGemini(t *testing.T) {
	a, ok := adapter.Lookup(Kind)
	if !ok {
		t.Fatal("the gemini adapter is not registered")
	}
	if err := a.Manifest().Validate(); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if a.Manifest().Binary != "agy" {
		t.Errorf("binary = %q, want agy", a.Manifest().Binary)
	}
}

func TestBuildArgs(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*adapter.RunSpec)
		want []string
	}{
		{"full", func(*adapter.RunSpec) {},
			[]string{"--print=", "--output-format=stream-json", "--input-format=stream-json", "--dangerously-skip-permissions"}},
		{"read only is plan mode", func(s *adapter.RunSpec) { s.PermissionMode = adapter.PermissionReadOnly },
			[]string{"--print=", "--output-format=stream-json", "--input-format=stream-json", "--mode=plan"}},
		{"model and effort", func(s *adapter.RunSpec) { s.Model, s.Effort = "gemini-3.1-pro-high", "high" },
			[]string{"--print=", "--output-format=stream-json", "--input-format=stream-json", "--dangerously-skip-permissions",
				"--model=gemini-3.1-pro-high", "--effort=high"}},
		{"resume", func(s *adapter.RunSpec) { s.ResumeID = "11111111-1111-4111-8111-111111111111" },
			[]string{"--print=", "--output-format=stream-json", "--input-format=stream-json", "--dangerously-skip-permissions",
				"--conversation=11111111-1111-4111-8111-111111111111"}},
		{"total timeout", func(s *adapter.RunSpec) { s.Timeouts.Total = 90 * time.Minute },
			[]string{"--print=", "--output-format=stream-json", "--input-format=stream-json", "--dangerously-skip-permissions",
				"--print-timeout=1h30m0s"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := baseSpec(t)
			spec.SystemPrompt = "standing rules"
			c.mod(&spec)
			got, err := buildArgs(spec)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("argv\n got %q\nwant %q", got, c.want)
			}
			for _, a := range got {
				if strings.Contains(a, testKey) || strings.Contains(a, "do the thing") || strings.Contains(a, "standing rules") {
					t.Errorf("argv carries the key or the prompt: %q", a)
				}
			}
		})
	}
	for _, m := range []adapter.PermissionMode{adapter.PermissionAsk, adapter.PermissionAcceptEdits} {
		spec := baseSpec(t)
		spec.PermissionMode = m
		if _, err := buildArgs(spec); !errors.Is(err, adapter.ErrNotSupported) {
			t.Errorf("%s: err = %v, want ErrNotSupported", m, err)
		}
	}
}

func TestPromptGoesOnStdinWithTheSystemPromptFirst(t *testing.T) {
	spec := baseSpec(t)
	spec.SystemPrompt = "standing rules"
	msg, err := stdinMessage(promptText(spec))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Event   string `json:"event"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(msg, &got); err != nil {
		t.Fatal(err)
	}
	if got.Event != "user" || got.Message.Role != "user" {
		t.Errorf("message = %s", msg)
	}
	if !strings.HasPrefix(got.Message.Content, "standing rules") || !strings.HasSuffix(got.Message.Content, "do the thing") {
		t.Errorf("content = %q", got.Message.Content)
	}
	if msg[len(msg)-1] != '\n' {
		t.Error("the message does not end its line")
	}
}

func TestStartRefuses(t *testing.T) {
	a := &Adapter{binary: "agy-does-not-exist", Root: t.TempDir(), RealHome: t.TempDir()}
	cases := []struct {
		name string
		mod  func(*adapter.RunSpec)
		is   error
		text string
	}{
		{"no key", func(s *adapter.RunSpec) { s.ProviderKey = "" }, adapter.ErrNoProviderKey, MissingKeyLine},
		{"a key too short to redact", func(s *adapter.RunSpec) { s.ProviderKey = "short" }, adapter.ErrNoProviderKey, MissingKeyLine},
		{"the key in Env", func(s *adapter.RunSpec) { s.Env = map[string]string{"GEMINI_API_KEY": testKey} }, adapter.ErrNotPlanAuth, "GEMINI_API_KEY"},
		{"GOOGLE_API_KEY in Env", func(s *adapter.RunSpec) { s.Env = map[string]string{"GOOGLE_API_KEY": testKey} }, adapter.ErrNotPlanAuth, "GOOGLE_API_KEY"},
		{"hosted", func(s *adapter.RunSpec) { s.Hosted = true }, adapter.ErrNotAPIAuth, "Claude Code or Codex"},
		{"ask", func(s *adapter.RunSpec) { s.PermissionMode = adapter.PermissionAsk }, adapter.ErrCapability, "approvals"},
		{"accept edits", func(s *adapter.RunSpec) { s.PermissionMode = adapter.PermissionAcceptEdits }, adapter.ErrCapability, "approvals"},
		{"mcp", func(s *adapter.RunSpec) { s.MCPServers = map[string]any{"x": map[string]any{}} }, adapter.ErrCapability, "mcp"},
		{"a tool list", func(s *adapter.RunSpec) { s.DeniedTools = []string{"run_command"} }, adapter.ErrNotSupported, "tool"},
		{"program missing", func(*adapter.RunSpec) {}, exec.ErrNotFound, InstallCommand},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := baseSpec(t)
			c.mod(&spec)
			_, err := a.Start(context.Background(), spec)
			if !errors.Is(err, c.is) {
				t.Fatalf("err = %v, want %v", err, c.is)
			}
			if !strings.Contains(err.Error(), c.text) {
				t.Errorf("err = %q, want it to say %q", err, c.text)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Error("the error carries the key")
			}
		})
	}
	// The missing-key line is exactly the one sentence, nothing around it.
	spec := baseSpec(t)
	spec.ProviderKey = ""
	if _, err := a.Start(context.Background(), spec); err.Error() != "Add your Google key in Elk to use Gemini." {
		t.Errorf("missing-key message = %q", err)
	}
}

func TestChildEnvCarriesTheKeyOnceAndNothingThatMovesIt(t *testing.T) {
	h := &home{dir: "/rein/gemini/home"}
	base := []string{
		"PATH=/usr/bin", "HOME=/Users/person",
		"GEMINI_API_KEY=inherited-should-not-survive",
		"AGY_ADC_AUTH=true", "AGY_ACCOUNT=person@gmail.com",
		"GOOGLE_GEMINI_BASE_URL=https://elsewhere.example", "GOOGLE_GENAI_USE_VERTEXAI=true",
	}
	over := map[string]string{"POSTHOG_API_KEY": "phc", "HOME": "/spec/home", "AGY_ADC_AUTH": "1"}
	env := h.childEnv(base, over, testKey)
	got := map[string][]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = append(got[k], v)
	}
	if v := got["GEMINI_API_KEY"]; len(v) != 1 || v[0] != testKey {
		t.Errorf("GEMINI_API_KEY = %v, want exactly the provider key once", v)
	}
	if v := got["HOME"]; len(v) != 1 || v[0] != "/rein/gemini/home" {
		t.Errorf("HOME = %v, want the private home", v)
	}
	for _, k := range []string{"AGY_ADC_AUTH", "AGY_ACCOUNT", "GOOGLE_GEMINI_BASE_URL", "GOOGLE_GENAI_USE_VERTEXAI"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s reached the child", k)
		}
	}
	if got["PATH"] == nil || got["POSTHOG_API_KEY"] == nil {
		t.Errorf("the ordinary environment was lost: %v", env)
	}
}

// The other adapters' children are built from RunSpec.InheritedEnv and Env:
// the first drops the Google key names whatever a queue says, and CheckPlanEnv
// refuses the second carrying one. ProviderKey is set only for gemini runs.
func TestGoogleKeysNeverReachAnotherAdapter(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", testKey)
	t.Setenv("GOOGLE_API_KEY", testKey)
	for _, kind := range []string{"claude", "codex", "grok"} {
		spec := adapter.RunSpec{AgentKind: kind, PassEnv: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}}
		for _, kv := range spec.InheritedEnv() {
			if strings.HasPrefix(kv, "GEMINI_API_KEY=") || strings.HasPrefix(kv, "GOOGLE_API_KEY=") {
				t.Errorf("%s: a run inherited %s", kind, strings.SplitN(kv, "=", 2)[0])
			}
		}
		spec.Env = map[string]string{"GEMINI_API_KEY": testKey}
		if err := spec.CheckPlanEnv(); !errors.Is(err, adapter.ErrNotPlanAuth) {
			t.Errorf("%s: a spec carrying GEMINI_API_KEY in Env was not refused: %v", kind, err)
		}
	}
	if !secretenv.Metered("GEMINI_API_KEY") || !secretenv.Metered("GOOGLE_API_KEY") {
		t.Error("the Google key names are not metered")
	}
}

func TestHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the gemini adapter does not run on Windows")
	}
	real := t.TempDir()
	for _, name := range []string{".gitconfig", ".ssh", "go", ".gemini", ".agents", ".claude", "Library"} {
		if strings.HasPrefix(name, ".git") {
			must(t, os.WriteFile(filepath.Join(real, name), []byte("[user]\n"), 0o600))
		} else {
			must(t, os.MkdirAll(filepath.Join(real, name), 0o700))
		}
	}
	// The person's own agy settings, which must not change.
	must(t, os.MkdirAll(filepath.Join(real, ".gemini", "antigravity-cli"), 0o700))
	personal := filepath.Join(real, ".gemini", "antigravity-cli", "settings.json")
	must(t, os.WriteFile(personal, []byte(`{"colorScheme":"dark"}`), 0o600))
	// Rein's own directory inside the home.
	root := filepath.Join(real, ".rein", "gemini")
	must(t, os.MkdirAll(root, 0o700))

	a := &Adapter{Root: root, RealHome: real}
	h, err := a.prepareHome()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".gitconfig", ".ssh", "go", "Library"} {
		target, err := os.Readlink(filepath.Join(h.dir, name))
		if err != nil || target != filepath.Join(real, name) {
			t.Errorf("%s is not linked to the real home: %q, %v", name, target, err)
		}
	}
	for _, name := range []string{".agents", ".claude", ".rein"} {
		if _, err := os.Lstat(filepath.Join(h.dir, name)); err == nil {
			t.Errorf("%s was linked into the private home", name)
		}
	}
	info, err := os.Lstat(filepath.Join(h.dir, ".gemini"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the private home's .gemini must be its own directory: %v", err)
	}
	var s map[string]any
	blob, err := os.ReadFile(filepath.Join(h.dir, settingsRel))
	must(t, err)
	must(t, json.Unmarshal(blob, &s))
	if s["modelProvider"] != "gemini" {
		t.Errorf("settings = %s, want modelProvider gemini", blob)
	}
	if got, _ := os.ReadFile(personal); string(got) != `{"colorScheme":"dark"}` {
		t.Errorf("the person's own agy settings changed: %s", got)
	}

	// agy's own keys survive a refresh; a new entry in the real home is
	// picked up; one that went away is unlinked.
	must(t, os.WriteFile(filepath.Join(h.dir, settingsRel), []byte(`{"modelProvider":"gemini","agentMode":"x"}`), 0o600))
	must(t, os.MkdirAll(filepath.Join(real, ".cargo"), 0o700))
	must(t, os.RemoveAll(filepath.Join(real, "go")))
	if _, err := a.prepareHome(); err != nil {
		t.Fatal(err)
	}
	blob, _ = os.ReadFile(filepath.Join(h.dir, settingsRel))
	if !strings.Contains(string(blob), "agentMode") {
		t.Errorf("a refresh dropped agy's own settings: %s", blob)
	}
	if _, err := os.Readlink(filepath.Join(h.dir, ".cargo")); err != nil {
		t.Error("a new entry in the real home was not linked")
	}
	if _, err := os.Lstat(filepath.Join(h.dir, "go")); err == nil {
		t.Error("a link to something gone was kept")
	}
}

func TestDecodeFixtures(t *testing.T) {
	cases := []struct {
		file    string
		status  string
		text    string
		tools   []string
		usage   int64
		denied  bool
		errText string
	}{
		{file: "text.jsonl", status: "SUCCESS", text: "Hello from fake.\n", usage: 14},
		{file: "tool.jsonl", status: "SUCCESS", text: "Hello from fake.\n", tools: []string{"list_dir"}, usage: 28},
		{file: "denied.jsonl", status: "SUCCESS", tools: []string{"run_command"}, usage: 14, denied: true},
		{file: "badkey.jsonl", status: "ERROR", errText: "API key not valid"},
		{file: "ratelimit.jsonl", status: "ERROR", errText: "Error 429"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			blob, err := os.ReadFile(filepath.Join("testdata", c.file))
			must(t, err)
			var text string
			var tools []string
			var usage int64
			var res *resultInfo
			var denied bool
			for _, line := range strings.Split(strings.TrimSpace(string(blob)), "\n") {
				d, err := decode([]byte(line), "gemini-test")
				if err != nil {
					t.Fatalf("%s: %v", line, err)
				}
				if d.conversationID == "" {
					t.Errorf("no conversation id from %s", line[:40])
				}
				for _, ev := range d.events {
					switch ev.Kind {
					case adapter.EventText:
						text += ev.Text
					case adapter.EventToolUse:
						tools = append(tools, ev.Tool.Name)
					case adapter.EventUsage:
						usage += ev.Usage.InputTokens + ev.Usage.OutputTokens
						if ev.Usage.Model != "gemini-test" {
							t.Errorf("usage model = %q", ev.Usage.Model)
						}
					case adapter.EventProgress:
						if strings.Contains(ev.Text, "refused actions") {
							denied = true
						}
					}
				}
				if d.result != nil {
					res = d.result
				}
			}
			if res == nil || res.Status != c.status {
				t.Fatalf("result = %+v, want status %s", res, c.status)
			}
			if text != c.text {
				t.Errorf("text = %q, want %q", text, c.text)
			}
			if strings.Join(tools, ",") != strings.Join(c.tools, ",") {
				t.Errorf("tools = %v, want %v", tools, c.tools)
			}
			if usage != c.usage {
				t.Errorf("step usage = %d, want %d", usage, c.usage)
			}
			if denied != c.denied {
				t.Errorf("denied = %v, want %v", denied, c.denied)
			}
			if c.errText != "" && !strings.Contains(res.Error, c.errText) {
				t.Errorf("error = %q", res.Error)
			}
		})
	}
}

func TestReapRunKeepsTheNewestAndTheLive(t *testing.T) {
	root := t.TempDir()
	a := &Adapter{Root: root}
	conv := filepath.Join(root, "home", ".gemini", "antigravity-cli", "conversations")
	must(t, os.MkdirAll(conv, 0o700))
	must(t, os.MkdirAll(filepath.Join(root, "runs"), 0o700))
	ids := []string{
		"aaaaaaaa-0000-4000-8000-000000000001",
		"aaaaaaaa-0000-4000-8000-000000000002",
		"aaaaaaaa-0000-4000-8000-000000000003",
		"aaaaaaaa-0000-4000-8000-000000000004",
	}
	now := time.Now()
	for i, id := range ids {
		run := fmt.Sprintf("run-%d", i+1)
		must(t, os.WriteFile(filepath.Join(conv, id+".db"), []byte("x"), 0o600))
		p := filepath.Join(root, "runs", run)
		must(t, os.WriteFile(p, []byte(id+"\n"), 0o600))
		// run-1 oldest … run-4 newest
		must(t, os.Chtimes(p, now.Add(time.Duration(i)*time.Minute), now.Add(time.Duration(i)*time.Minute)))
	}
	lines, err := a.ReapRun(adapter.RunSpec{RunID: "run-4"}, adapter.ReapPolicy{
		KeepRuns: 2, Live: map[string]time.Time{"run-1": now}, Now: now,
	})
	must(t, err)
	// Kept: run-1 (live) and run-4 (newest); run-1 counts against the two.
	for i, want := range []bool{true, false, false, true} {
		_, err := os.Stat(filepath.Join(conv, ids[i]+".db"))
		if got := err == nil; got != want {
			t.Errorf("conversation of run-%d kept = %v, want %v (%v)", i+1, got, want, lines)
		}
	}
}

func TestValidIDRefusesPaths(t *testing.T) {
	for _, bad := range []string{"", "../../etc", "a/b", "..", "x"} {
		if validID(bad) {
			t.Errorf("validID(%q) = true", bad)
		}
	}
	if !validID("11111111-1111-4111-8111-111111111111") {
		t.Error("a UUID was refused")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
