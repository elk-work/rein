package grok

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// fakeRealHome builds a stand-in for ~/.grok holding a login, session state,
// the developer's config — and a symlinked hook of the kind that makes Grok's
// sandbox refuse to start (ark:rein#16).
func fakeRealHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"pretend":"credential"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[mcp_servers.elk-agent]\nurl = \"https://api.elk.work/mcp/secret\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions", "proj"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/elsewhere/hook.json", filepath.Join(dir, "hooks", "linked.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", dir)
	return dir
}

func TestPrivateHomeLeavesTheDevelopersConfigBehind(t *testing.T) {
	real := fakeRealHome(t)

	h, err := newPrivateHome("arun-18298891")
	if err != nil {
		t.Fatalf("newPrivateHome: %v", err)
	}
	defer h.remove()

	// The config.toml is where the Elk connector lived. Its absence is the fix.
	if _, err := os.Lstat(filepath.Join(h.dir, "config.toml")); !os.IsNotExist(err) {
		t.Fatal("the private home carried config.toml across; that is the bug this exists to prevent")
	}

	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range []string{"auth.json", "sessions", "hooks", "hooks-paths"} {
		if !got[want] {
			t.Errorf("%s is missing from the private home; %v", want, got)
		}
		delete(got, want)
	}
	if len(got) != 0 {
		t.Errorf("the private home holds more than it should: %v", got)
	}

	info, err := os.Lstat(filepath.Join(h.dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("auth.json is a copy; it must be a link, so no credential is ever read or duplicated")
	}
	if target, _ := os.Readlink(filepath.Join(h.dir, "auth.json")); target != filepath.Join(real, "auth.json") {
		t.Errorf("auth.json links to %q, want the real home's", target)
	}

	// Real, and holding nothing of the developer's. Grok's sandbox refuses to
	// start when a hook source path has a symlink component, and the
	// developer's hooks directory has one — so the private home must not
	// borrow it. The one file allowed in it is Rein's own StopFailure hook
	// (ark:rein#40), and that is a real file too.
	hooks, err := os.Lstat(filepath.Join(h.dir, "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	if hooks.Mode()&os.ModeSymlink != 0 || !hooks.IsDir() {
		t.Error("hooks must be a real directory, or the sandbox refuses to start")
	}
	inside, err := os.ReadDir(filepath.Join(h.dir, "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range inside {
		names = append(names, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			t.Errorf("hooks/%s is a symlink; the sandbox refuses to start with one", e.Name())
		}
	}
	want := []string{}
	if hookSupported() {
		want = []string{stopFailureHookName}
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("hooks/ holds %v, want only Rein's own %v", names, want)
	}

	// No symlink anywhere on the way to the home either — /var on macOS is one.
	if resolved, err := filepath.EvalSymlinks(h.dir); err != nil || resolved != h.dir {
		t.Errorf("the private home %q has a symlink component (resolves to %q, %v)", h.dir, resolved, err)
	}
}

func TestStopFailureHookRecordsARateLimit(t *testing.T) {
	if !hookSupported() {
		t.Skip("no StopFailure hook on this platform")
	}
	fakeRealHome(t)
	h, err := newPrivateHome("run-rl")
	if err != nil {
		t.Fatal(err)
	}
	defer h.remove()
	if h.stopFailureLog == "" {
		t.Fatal("no StopFailure log path; the hook was not installed")
	}

	// The hook Grok would run, run the way Grok runs it: the payload on stdin.
	var hook struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type, Command string
			} `json:"hooks"`
		} `json:"hooks"`
	}
	b, err := os.ReadFile(filepath.Join(h.dir, "hooks", stopFailureHookName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &hook); err != nil {
		t.Fatalf("the hook file is not valid JSON: %v", err)
	}
	groups := hook.Hooks["StopFailure"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Type != "command" {
		t.Fatalf("want one StopFailure command hook, got %s", b)
	}
	command := groups[0].Hooks[0].Command
	for _, payload := range []string{
		`{"hook_event_name":"StopFailure","error":"server_error","errorDetails":"502"}`,
		"{\n  \"hook_event_name\": \"StopFailure\",\n  \"error\": \"rate_limit\",\n  \"errorDetails\": \"429 weekly limit\"\n}",
	} {
		cmd := exec.Command("sh", "-c", command)
		cmd.Stdin = strings.NewReader(payload)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("running the hook: %v: %s", err, out)
		}
	}

	fails := readStopFailures(h.stopFailureLog)
	if len(fails) != 2 || fails[0].Error != "server_error" || fails[1].Error != "rate_limit" {
		t.Fatalf("read back %+v", fails)
	}
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	rl := rateLimitFrom(fails, "", now)
	if rl == nil || !rl.Exhausted || rl.Source != adapter.SourceGrokStopFailure || !rl.ExhaustedUntil.IsZero() ||
		!rl.SampledAt.Equal(now) {
		t.Fatalf("rateLimitFrom = %+v; want exhausted, from the StopFailure hook, with no stated reset", rl)
	}
}

func TestRateLimitFromIsNarrow(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		fails   []stopFailure
		errText string
		want    bool
	}{
		{"nothing failed", nil, "", false},
		{"a server error is not a rate limit", []stopFailure{{Error: "server_error"}}, "", false},
		{"classified rate limit", []stopFailure{{Error: "rate_limit"}}, "", true},
		// With a hook report, the classification wins over the text: the
		// runtime has already said what kind of failure it was.
		{"the classification beats the text", []stopFailure{{Error: "authentication_failed"}},
			"429 Too Many Requests", false},
		{"no hook report: a 429 in the text", nil, "grok: rpc error: 429 Too Many Requests", true},
		{"no hook report: rate limit spelled out", nil, "rate_limit_exceeded", true},
		{"no hook report: an ordinary failure", nil, "grok: the turn stopped at the token limit", false},
	} {
		got := rateLimitFrom(tc.fails, tc.errText, now) != nil
		if got != tc.want {
			t.Errorf("%s: exhausted = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPrivateHomeEnvClosesEveryDiscoveryPathItCan(t *testing.T) {
	fakeRealHome(t)
	h, err := newPrivateHome("run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.remove()

	env := h.env()
	if env["GROK_HOME"] != h.dir {
		t.Errorf("GROK_HOME = %q, want the private home", env["GROK_HOME"])
	}
	// GROK_HOME closes the config-file sources; these close Grok's scan of
	// Claude's and Cursor's MCP configuration, which lives under $HOME and so
	// is not moved by GROK_HOME.
	for _, k := range []string{"GROK_CLAUDE_MCPS_ENABLED", "GROK_CURSOR_MCPS_ENABLED"} {
		if env[k] != "0" {
			t.Errorf("%s = %q, want 0", k, env[k])
		}
	}
}

func TestPrivateHomeRefusesWithoutALogin(t *testing.T) {
	t.Setenv("GROK_HOME", t.TempDir())
	_, err := newPrivateHome("run-1")
	if !errors.Is(err, adapter.ErrPreflight) {
		t.Fatalf("newPrivateHome without a login = %v, want ErrPreflight", err)
	}
	if !strings.Contains(err.Error(), "grok login") {
		t.Errorf("the error does not say what to do: %v", err)
	}
}

func TestPrivateHomeKeepsARotatedLogin(t *testing.T) {
	fakeRealHome(t)
	h, err := newPrivateHome("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if h.rotatedLogin() != "" {
		t.Fatal("a freshly linked login was reported as rotated")
	}
	link := filepath.Join(h.dir, "auth.json")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte(`{"newer":"credential"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if h.rotatedLogin() == "" {
		t.Fatal("a rotated login was not noticed")
	}
	h.remove()
	if _, err := os.Stat(h.dir); err != nil {
		t.Fatalf("remove deleted a home holding a rotated credential: %v", err)
	}
	_ = os.RemoveAll(h.dir)
}

func TestPrivateHomeRemovesItselfWhenClean(t *testing.T) {
	fakeRealHome(t)
	h, err := newPrivateHome("run-1")
	if err != nil {
		t.Fatal(err)
	}
	h.remove()
	if _, err := os.Stat(h.dir); !os.IsNotExist(err) {
		t.Errorf("the private home outlived its session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.real, "auth.json")); err != nil {
		t.Errorf("removing the private home damaged the real one: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.real, "sessions", "proj")); err != nil {
		t.Errorf("removing the private home damaged the real session state: %v", err)
	}
}

func TestMergeMapsLetsTheSpecWin(t *testing.T) {
	got := mergeMaps(map[string]string{"GROK_HOME": "/private"},
		map[string]string{"GROK_HOME": "/explicit", "OTHER": "1"})
	if got["GROK_HOME"] != "/explicit" || got["OTHER"] != "1" {
		t.Errorf("mergeMaps = %v", got)
	}
}

func TestSanitizeRunID(t *testing.T) {
	if got := sanitizeRunID("arun-18298891"); got != "arun-18298891-" {
		t.Errorf("sanitizeRunID = %q", got)
	}
	if got := sanitizeRunID("../../etc/passwd"); strings.ContainsAny(got, "./") {
		t.Errorf("sanitizeRunID let a path separator through: %q", got)
	}
}

func TestAssembledRunEnvDisablesForeignConfiguration(t *testing.T) {
	h := &privateHome{dir: "/private/run"}
	extra := map[string]string{"GROK_HOME": "/developer", "CUSTOM": "kept"}
	base := []string{"GROK_HOME=/developer", "CUSTOM=old"}
	for name := range h.env() {
		if name != "GROK_HOME" {
			extra[name] = "1"
			base = append(base, name+"=1")
		}
	}
	env := h.runEnv(base, extra)
	got := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if _, duplicate := got[key]; duplicate {
			t.Fatalf("duplicate env %s", key)
		}
		got[key] = value
	}
	for name, want := range h.env() {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	if got["CUSTOM"] != "kept" {
		t.Fatal("caller env was lost")
	}
	if _, present := got["GROK_HOOKS_ENABLED"]; present {
		t.Fatal("native hooks must remain enabled")
	}
}
