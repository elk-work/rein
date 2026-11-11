package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"path/filepath"
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
	if a.Manifest().Binary != "gemini" {
		t.Errorf("binary = %q, want gemini", a.Manifest().Binary)
	}
	if _, ok := a.(adapter.MCPConfigurer); !ok {
		t.Error("the adapter cannot take an allow-listed MCP server")
	}
}

func TestBuildArgs(t *testing.T) {
	base := []string{"--prompt", "", "--output-format", "stream-json"}
	cases := []struct {
		name string
		mod  func(*adapter.RunSpec)
		want []string
	}{
		{"full is yolo", func(*adapter.RunSpec) {},
			append(append([]string{}, base...), "--approval-mode", "yolo", "--skip-trust")},
		{"read only is plan", func(s *adapter.RunSpec) { s.PermissionMode = adapter.PermissionReadOnly },
			append(append([]string{}, base...), "--approval-mode", "plan", "--skip-trust")},
		{"model", func(s *adapter.RunSpec) { s.Model = "gemini-2.5-pro" },
			append(append([]string{}, base...), "--approval-mode", "yolo", "--skip-trust", "--model", "gemini-2.5-pro")},
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
			if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", c.want) {
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

func TestPromptPutsTheSystemPromptFirst(t *testing.T) {
	spec := baseSpec(t)
	spec.SystemPrompt = "standing rules"
	got := promptText(spec)
	if !strings.HasPrefix(got, "standing rules") || !strings.HasSuffix(got, "do the thing") {
		t.Errorf("prompt = %q", got)
	}
}

func TestStartRefuses(t *testing.T) {
	if !New().Manifest().SupportsHost() {
		t.Skip("the gemini adapter does not run on this platform, so Start refuses before any of these checks")
	}
	a := &Adapter{binary: "gemini-does-not-exist", Root: t.TempDir()}
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
		{"resume", func(s *adapter.RunSpec) { s.ResumeID = "abc" }, adapter.ErrCapability, "resume"},
		{"a tool list", func(s *adapter.RunSpec) { s.DeniedTools = []string{"run_shell_command"} }, adapter.ErrNotSupported, "tool"},
		{"program missing", func(*adapter.RunSpec) {}, exec.ErrNotFound, installLine()},
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

func TestARepositoryCannotChooseTheSignIn(t *testing.T) {
	cases := []struct {
		settings string
		refused  bool
	}{
		{`{"security":{"auth":{"selectedType":"oauth-personal","enforcedType":"oauth-personal"}}}`, true},
		{`{"security":{"auth":{"selectedType":"oauth-personal"}}}`, true},
		{`{"security":{"auth":{"selectedType":"vertex-ai"}}}`, true},
		{`{"security":{"auth":{"useExternal":true}}}`, true},
		{`not json`, true},
		{`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`, false},
		{"// a comment\n{\"general\":{\"vimMode\":true}}", false},
		{`{"mcpServers":{}}`, false},
	}
	for _, c := range cases {
		dir := t.TempDir()
		must(t, os.MkdirAll(filepath.Join(dir, ".gemini"), 0o700))
		must(t, os.WriteFile(filepath.Join(dir, ".gemini", "settings.json"), []byte(c.settings), 0o600))
		err := checkWorkspaceAuth(dir)
		if (err != nil) != c.refused {
			t.Errorf("%s: err = %v, want refused %v", c.settings, err, c.refused)
		}
		if err != nil && !errors.Is(err, adapter.ErrNotPlanAuth) {
			t.Errorf("%s: the refusal is not a sign-in refusal: %v", c.settings, err)
		}
	}
	if err := checkWorkspaceAuth(t.TempDir()); err != nil {
		t.Errorf("a repository with no Gemini settings was refused: %v", err)
	}
}

func TestChildEnvCarriesTheKeyOnceAndNothingThatMovesIt(t *testing.T) {
	h := &home{dir: "/rein/gemini/runs/run-1-x"}
	base := []string{
		"PATH=/usr/bin", "HOME=/Users/person",
		"GEMINI_API_KEY=inherited-should-not-survive",
		"GEMINI_DEFAULT_AUTH_TYPE=oauth-personal", "GEMINI_CLI_HOME=/Users/person",
		"GEMINI_CLI_SYSTEM_SETTINGS_PATH=/x", "GEMINI_RESTRICTED_MODE=true",
		"GOOGLE_GEMINI_BASE_URL=https://elsewhere.example", "GOOGLE_GENAI_USE_VERTEXAI=true",
		"GOOGLE_CLOUD_PROJECT=p", "GOOGLE_API_KEY=x", "GOOGLE_APPLICATION_CREDENTIALS=/creds.json",
	}
	over := map[string]string{"POSTHOG_API_KEY": "phc", "GEMINI_CLI_HOME": "/spec"}
	env := h.childEnv(base, over, testKey)
	got := map[string][]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = append(got[k], v)
	}
	want := map[string]string{
		"GEMINI_API_KEY":             testKey,
		"GEMINI_CLI_HOME":            h.dir,
		"GEMINI_CLI_TRUST_WORKSPACE": "true",
		"HOME":                       "/Users/person", // git, gh and SSH stay the person's
		"PATH":                       "/usr/bin",
		"POSTHOG_API_KEY":            "phc",
	}
	for k, v := range want {
		if len(got[k]) != 1 || got[k][0] != v {
			t.Errorf("%s = %v, want exactly %q", k, got[k], v)
		}
	}
	for _, k := range []string{"GEMINI_DEFAULT_AUTH_TYPE", "GEMINI_CLI_SYSTEM_SETTINGS_PATH", "GEMINI_RESTRICTED_MODE",
		"GOOGLE_GEMINI_BASE_URL", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_CLOUD_PROJECT", "GOOGLE_API_KEY",
		"GOOGLE_APPLICATION_CREDENTIALS"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s reached the child", k)
		}
	}
}

// The other adapters' children are built from RunSpec.InheritedEnv and Env:
// the first drops the Google key names whatever a queue says, and CheckPlanEnv
// refuses the second carrying one. ProviderKey is set only for gemini runs
// (the runner's tests hold that).
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

func TestTheHomeEnforcesTheKeyAndCarriesTheRunsServers(t *testing.T) {
	a := &Adapter{Root: t.TempDir()}
	spec := baseSpec(t)
	srv, err := a.MCPServerConfig(adapter.MCPServerSpec{Name: "posthog", URL: "https://mcp.posthog.com/mcp", BearerTokenEnvVar: "POSTHOG_API_KEY"})
	must(t, err)
	local, err := a.MCPServerConfig(adapter.MCPServerSpec{Name: "figma", Command: "figma-mcp", Args: []string{"--stdio"}, EnvVars: []string{"FIGMA_TOKEN"}})
	must(t, err)
	spec.MCPServers = map[string]any{"posthog": srv, "figma": local}
	h, err := a.newHome(spec)
	must(t, err)
	blob, err := os.ReadFile(filepath.Join(h.dir, ".gemini", "settings.json"))
	must(t, err)
	var s struct {
		Security struct {
			Auth map[string]string `json:"auth"`
		} `json:"security"`
		General    map[string]bool           `json:"general"`
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	must(t, json.Unmarshal(blob, &s))
	if s.Security.Auth["selectedType"] != "gemini-api-key" || s.Security.Auth["enforcedType"] != "gemini-api-key" {
		t.Errorf("auth = %v", s.Security.Auth)
	}
	if s.General["enableAutoUpdate"] {
		t.Error("the run may update Gemini CLI")
	}
	if s.MCPServers["posthog"]["httpUrl"] != "https://mcp.posthog.com/mcp" {
		t.Errorf("posthog = %v", s.MCPServers["posthog"])
	}
	if !strings.Contains(string(blob), `"Authorization": "Bearer ${POSTHOG_API_KEY}"`) ||
		!strings.Contains(string(blob), `"FIGMA_TOKEN": "${FIGMA_TOKEN}"`) {
		t.Errorf("a server's secret is not a name for Gemini CLI to expand:\n%s", blob)
	}
	if strings.Contains(string(blob), testKey) {
		t.Error("the key was written into the settings")
	}
	h.remove()
	if _, err := os.Stat(h.dir); !os.IsNotExist(err) {
		t.Error("the private home was not removed")
	}
}

func TestDecodeFixtures(t *testing.T) {
	cases := []struct {
		file    string
		ok      bool
		text    string
		tools   []string
		input   int64
		failed  bool
		errText string
	}{
		{file: "text.jsonl", ok: true, text: "Hello from fake.", input: 10},
		{file: "tool.jsonl", ok: true, text: "Hello from fake.", tools: []string{"write_file"}, input: 20},
		{file: "plan.jsonl", ok: true, text: "Hello from fake.", tools: []string{"write_file"}, input: 20, failed: true},
		{file: "badkey.jsonl", ok: false, errText: "API key not valid"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			blob, err := os.ReadFile(filepath.Join("testdata", c.file))
			must(t, err)
			var text, sid string
			var tools []string
			var input int64
			var res *turnResult
			var failed bool
			for _, l := range strings.Split(strings.TrimSpace(string(blob)), "\n") {
				d, err := decode([]byte(l))
				if err != nil {
					t.Fatalf("%s: %v", l, err)
				}
				if d.sessionID != "" {
					sid = d.sessionID
				}
				for _, ev := range d.events {
					switch ev.Kind {
					case adapter.EventText:
						text += ev.Text
					case adapter.EventToolUse:
						tools = append(tools, ev.Tool.Name)
					case adapter.EventUsage:
						input += ev.Usage.InputTokens
						if ev.Usage.Model == "" {
							t.Error("usage has no model")
						}
					case adapter.EventProgress:
						if strings.Contains(ev.Text, "failed") {
							failed = true
						}
					}
				}
				if d.result != nil {
					res = d.result
				}
			}
			if sid == "" {
				t.Error("no session id")
			}
			if res == nil || res.ok != c.ok {
				t.Fatalf("result = %+v, want ok %v", res, c.ok)
			}
			if text != c.text {
				t.Errorf("text = %q, want %q", text, c.text)
			}
			if strings.Join(tools, ",") != strings.Join(c.tools, ",") {
				t.Errorf("tools = %v, want %v", tools, c.tools)
			}
			if input != c.input {
				t.Errorf("input tokens = %d, want %d", input, c.input)
			}
			if failed != c.failed {
				t.Errorf("a failed tool reported = %v, want %v", failed, c.failed)
			}
			if c.errText != "" && !strings.Contains(res.error, c.errText) {
				t.Errorf("error = %q", res.error)
			}
		})
	}
}

func TestReapRunRemovesOnlyHomesNoLiveRunOwns(t *testing.T) {
	root := t.TempDir()
	a := &Adapter{Root: root}
	for _, d := range []string{"run-1-aaa", "run-10-bbb", "run-2-ccc"} {
		must(t, os.MkdirAll(filepath.Join(root, "runs", d, ".gemini"), 0o700))
	}
	lines, err := a.ReapRun(adapter.RunSpec{RunID: "run-2"}, adapter.ReapPolicy{
		KeepRuns: 5, Live: map[string]time.Time{"run-1": time.Now()},
	})
	must(t, err)
	for d, kept := range map[string]bool{"run-1-aaa": true, "run-10-bbb": false, "run-2-ccc": false} {
		_, err := os.Stat(filepath.Join(root, "runs", d))
		if (err == nil) != kept {
			t.Errorf("%s kept = %v, want %v (%v)", d, err == nil, kept, lines)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// installLine is the install command a missing Gemini CLI names here.
func installLine() string {
	if runtime.GOOS == "windows" {
		return InstallCommandWindows
	}
	return InstallCommand
}
