package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
		{"the key in Env", func(s *adapter.RunSpec) { s.Env = map[string]string{"GEMINI_API_KEY": testKey} }, adapter.ErrNotPlanAuth, "GEMINI_API_KEY"},
		{"GOOGLE_API_KEY in Env", func(s *adapter.RunSpec) { s.Env = map[string]string{"GOOGLE_API_KEY": testKey} }, adapter.ErrNotPlanAuth, "GOOGLE_API_KEY"},
		{"hosted", func(s *adapter.RunSpec) { s.Hosted = true }, adapter.ErrNotAPIAuth, "Claude Code or Codex"},
		{"ask", func(s *adapter.RunSpec) { s.PermissionMode = adapter.PermissionAsk }, adapter.ErrCapability, "approvals"},
		{"accept edits", func(s *adapter.RunSpec) { s.PermissionMode = adapter.PermissionAcceptEdits }, adapter.ErrCapability, "approvals"},
		{"resume", func(s *adapter.RunSpec) { s.ResumeID = "abc" }, adapter.ErrCapability, "resume"},
		{"a tool list", func(s *adapter.RunSpec) { s.DeniedTools = []string{"run_shell_command"} }, adapter.ErrNotSupported, "tool"},
		{"program missing", func(*adapter.RunSpec) {}, exec.ErrNotFound, NotSetUpLine},
		{"program missing, no key", func(s *adapter.RunSpec) { s.ProviderKey = "" }, exec.ErrNotFound, NotSetUpLine},
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
}

// The person's own variables reach Gemini CLI — a Vertex AI project, a
// gateway — as they reach Codex and Claude Code; only the names Rein sets
// itself are Rein's, and a key appears once and only when one is stored. On
// the old code every GEMINI_ and GOOGLE_ variable was stripped.
func TestChildEnvKeepsThePersonsVariables(t *testing.T) {
	h := &home{dir: "/rein/gemini/runs/run-1-x"}
	base := []string{
		"PATH=/usr/bin", "HOME=/Users/person",
		"GEMINI_API_KEY=inherited-is-replaced", "GEMINI_CLI_HOME=/Users/person",
		"GOOGLE_CLOUD_PROJECT=p", "GOOGLE_GENAI_USE_VERTEXAI=true",
	}
	over := map[string]string{"POSTHOG_API_KEY": "phc", "gemini_cli_home": "/spec"}
	env := h.childEnv(base, over, testKey)
	got := map[string][]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[strings.ToUpper(k)] = append(got[strings.ToUpper(k)], v)
	}
	want := map[string]string{
		"GEMINI_API_KEY":             testKey,
		"GEMINI_CLI_HOME":            h.dir,
		"GEMINI_CLI_TRUST_WORKSPACE": "true",
		"HOME":                       "/Users/person", // git, gh and SSH stay the person's
		"PATH":                       "/usr/bin",
		"POSTHOG_API_KEY":            "phc",
		"GOOGLE_CLOUD_PROJECT":       "p",
		"GOOGLE_GENAI_USE_VERTEXAI":  "true",
	}
	for k, v := range want {
		if len(got[k]) != 1 || got[k][0] != v {
			t.Errorf("%s = %v, want exactly %q", k, got[k], v)
		}
	}
	if _, n := envValue(h.childEnv([]string{"PATH=/usr/bin"}, nil, ""), KeyName); n != 0 {
		t.Error("a key appeared though none is stored")
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

// The private home carries the person's key setup and nothing else of theirs:
// not their MCP servers (the Elk connector acts as them, ark:rein#21), and
// never the files that hold a Google sign-in. Only .env is linked.
func TestTheHomeCarriesThePersonsKeySetUpAndTheRunsServers(t *testing.T) {
	real := t.TempDir()
	conf := filepath.Join(real, ".gemini")
	must(t, os.MkdirAll(conf, 0o700))
	must(t, os.WriteFile(filepath.Join(conf, "settings.json"), []byte(`// theirs
{"security":{"auth":{"selectedType":"vertex-ai"}},"mcpServers":{"elk":{"httpUrl":"https://elk.example/mcp"}}}`), 0o600))
	must(t, os.WriteFile(filepath.Join(conf, ".env"), []byte(".env"), 0o600))
	for _, f := range signInFiles {
		must(t, os.WriteFile(filepath.Join(conf, f), []byte(f), 0o600))
	}
	must(t, os.WriteFile(filepath.Join(conf, "GEMINI.md"), []byte("their instructions"), 0o600))
	a := &Adapter{Root: t.TempDir(), RealHome: real}
	spec := baseSpec(t)
	srv, err := a.MCPServerConfig(adapter.MCPServerSpec{Name: "posthog", URL: "https://mcp.posthog.com/mcp", BearerTokenEnvVar: "POSTHOG_API_KEY"})
	must(t, err)
	local, err := a.MCPServerConfig(adapter.MCPServerSpec{Name: "figma", Command: "figma-mcp", Args: []string{"--stdio"}, EnvVars: []string{"FIGMA_TOKEN"}})
	must(t, err)
	spec.MCPServers = map[string]any{"posthog": srv, "figma": local}
	h, err := a.newHome(spec, "")
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
	if s.Security.Auth["selectedType"] != "vertex-ai" || s.Security.Auth["enforcedType"] != "" {
		t.Errorf("auth = %v, want the person's own key choice and nothing added", s.Security.Auth)
	}
	if s.General["enableAutoUpdate"] {
		t.Error("the run may update Gemini CLI")
	}
	if _, ok := s.MCPServers["elk"]; ok {
		t.Error("the person's own MCP server reached the run")
	}
	if s.MCPServers["posthog"]["httpUrl"] != "https://mcp.posthog.com/mcp" {
		t.Errorf("posthog = %v", s.MCPServers["posthog"])
	}
	if !strings.Contains(string(blob), `"Authorization": "Bearer ${POSTHOG_API_KEY}"`) ||
		!strings.Contains(string(blob), `"FIGMA_TOKEN": "${FIGMA_TOKEN}"`) {
		t.Errorf("a server's secret is not a name for Gemini CLI to expand:\n%s", blob)
	}
	if b, err := os.ReadFile(filepath.Join(h.dir, ".gemini", ".env")); err != nil || string(b) != ".env" {
		t.Errorf(".env is not reachable from the private home: %q %v", b, err)
	}
	for _, f := range append([]string{"GEMINI.md"}, signInFiles...) {
		if _, err := os.Lstat(filepath.Join(h.dir, ".gemini", f)); err == nil {
			t.Errorf("the person's %s reached the run", f)
		}
	}
	h.remove()
	if _, err := os.Stat(h.dir); !os.IsNotExist(err) {
		t.Error("the private home was not removed")
	}
	if _, err := os.Stat(filepath.Join(conf, ".env")); err != nil {
		t.Error("removing the private home removed the person's own file")
	}
}

// signInFiles hold a Google sign-in in Gemini CLI 0.64's home.
var signInFiles = []string{"oauth_creds.json", "google_accounts.json", "gemini-credentials.json"}

// A run goes only on a key: the person's own key choice is kept, a Google
// sign-in choice is replaced by the key sign-in when a key is stored or their
// .env is there, and with no key at all the run is refused as not set up
// (agent_login). On the old code a Google sign-in was carried and its files
// linked, so a run could act on the person's personal Google account.
func TestARunGoesOnlyOnAKey(t *testing.T) {
	for _, c := range []struct {
		name     string
		settings string // "" writes none
		env      bool   // a .env is there
		key      string // stored for Rein
		want     string // the run's selectedType; "" means refused
	}{
		{"Google sign-in, no key", `{"security":{"auth":{"selectedType":"oauth-personal"}}}`, false, "", ""},
		{"older Google sign-in, no key", `{"selectedAuthType":"oauth-personal"}`, false, "", ""},
		{"Google sign-in, a stored key", `{"security":{"auth":{"selectedType":"oauth-personal"}}}`, false, testKey, apiKeyAuth},
		{"Google sign-in, a .env", `{"security":{"auth":{"selectedType":"oauth-personal"}}}`, true, "", apiKeyAuth},
		{"Google Cloud sign-in, no key", `{"security":{"auth":{"selectedType":"cloud-shell"}}}`, false, "", ""},
		{"Gemini API key", `{"security":{"auth":{"selectedType":"gemini-api-key"}}}`, true, "", apiKeyAuth},
		{"Vertex AI", `{"selectedAuthType":"vertex-ai"}`, false, "", "vertex-ai"},
		{"nothing chosen, a stored key", "", false, testKey, apiKeyAuth},
		{"nothing chosen, a .env", "", true, "", apiKeyAuth},
		{"nothing at all", "", false, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			real := t.TempDir()
			conf := filepath.Join(real, ".gemini")
			must(t, os.MkdirAll(conf, 0o700))
			if c.settings != "" {
				must(t, os.WriteFile(filepath.Join(conf, "settings.json"), []byte(c.settings), 0o600))
			}
			if c.env {
				must(t, os.WriteFile(filepath.Join(conf, ".env"), []byte("x"), 0o600))
			}
			root := t.TempDir()
			a := &Adapter{Root: root, RealHome: real}
			h, err := a.newHome(baseSpec(t), c.key)
			if c.want == "" {
				if !errors.Is(err, adapter.ErrLoginRequired) || !strings.HasPrefix(fmt.Sprint(err), NotSetUpLine) {
					t.Fatalf("err = %v, want the not-set-up refusal", err)
				}
				if left, _ := os.ReadDir(filepath.Join(root, "runs")); len(left) != 0 {
					t.Errorf("a refused run left its home behind: %v", left)
				}
				return
			}
			must(t, err)
			defer h.remove()
			blob, err := os.ReadFile(filepath.Join(h.dir, ".gemini", "settings.json"))
			must(t, err)
			var got struct {
				Security struct {
					Auth map[string]string `json:"auth"`
				} `json:"security"`
			}
			must(t, json.Unmarshal(blob, &got))
			if got.Security.Auth["selectedType"] != c.want {
				t.Errorf("auth %v, want %q", got.Security.Auth, c.want)
			}
			if strings.Contains(string(blob), testKey) {
				t.Error("the key was written into the settings")
			}
		})
	}
}

// A repository's checked-in settings may choose only a key; anything else,
// or a file Rein cannot read, is refused.
func TestCheckWorkspaceAuth(t *testing.T) {
	cases := []struct {
		settings string
		refused  bool
	}{
		{`{"security":{"auth":{"selectedType":"oauth-personal","enforcedType":"oauth-personal"}}}`, true},
		{`{"security":{"auth":{"selectedType":"oauth-personal"}}}`, true},
		{`{"security":{"auth":{"enforcedType":"cloud-shell"}}}`, true},
		{`{"selectedAuthType":"oauth-personal"}`, true},
		{`{"security":{"auth":{"useExternal":true}}}`, true},
		{`not json`, true},
		{`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`, false},
		{`{"security":{"auth":{"selectedType":"vertex-ai","enforcedType":"vertex-ai"}}}`, false},
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

// GOOGLE_GENAI_USE_GCA would choose a Google sign-in from the environment, so
// no inherited or queued value reaches a run.
func TestTheGoogleSignInVariableNeverReachesARun(t *testing.T) {
	h := &home{dir: "/rein/gemini/runs/run-1-x"}
	env := h.childEnv([]string{"PATH=/usr/bin", "GOOGLE_GENAI_USE_GCA=true"}, map[string]string{"google_genai_use_gca": "true"}, testKey)
	if _, n := envValue(env, "GOOGLE_GENAI_USE_GCA"); n != 0 {
		t.Errorf("GOOGLE_GENAI_USE_GCA reached the run: %v", env)
	}
}

// The older top-level choice is read too.
func TestThePersonsOlderSignInSettingIsRead(t *testing.T) {
	real := t.TempDir()
	must(t, os.WriteFile(filepath.Join(real, "settings.json"), []byte(`{"selectedAuthType":"vertex-ai"}`), 0o600))
	if got := personAuth(real); got["selectedType"] != "vertex-ai" {
		t.Fatalf("auth = %v", got)
	}
	if personAuth(t.TempDir()) != nil {
		t.Fatal("a home with no settings chose a sign-in")
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
