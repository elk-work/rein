package codex

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
)

// fakeRealHome builds a stand-in for ~/.codex holding the login, the
// developer's configuration, and a history with its index — of which a private
// home may take the login and nothing else. It also points REIN_HOME at a
// scratch directory, so Rein's own Codex history lands there rather than in
// the ~/.rein of whoever runs the tests.
func fakeRealHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("auth.json", `{"pretend":"credential"}`)
	write("config.toml", "[mcp_servers.elk]\ncommand = \"npx\"\n")
	write("session_index.jsonl", "{}\n")
	write("thread_history_1.sqlite", "")
	write("thread_history_1.sqlite-wal", "")
	write("state_5.sqlite", "")
	write("state_5.sqlite-wal", "")
	if err := os.MkdirAll(filepath.Join(dir, "sessions", "2026"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", dir)
	t.Setenv(config.EnvHome, t.TempDir())
	return dir
}

func TestPrivateHomeLeavesTheDevelopersConfigBehind(t *testing.T) {
	real := fakeRealHome(t)

	h, err := newPrivateHome("run-42")
	if err != nil {
		t.Fatalf("newPrivateHome: %v", err)
	}
	defer h.remove()

	// The whole point: no config.toml, so no MCP servers, no hooks, no model
	// pin — nothing the packet did not ask for (ark:rein#21).
	if _, err := os.Lstat(filepath.Join(h.dir, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("the private home carried config.toml across; that is the bug this exists to prevent")
	}
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range []string{"auth.json", "sessions"} {
		if !got[want] {
			t.Errorf("%s was not linked in; %v", want, got)
		}
		delete(got, want)
	}
	if len(got) != 0 {
		t.Errorf("the private home started with more than the login and the sessions: %v", got)
	}

	// Carried, not copied: the vendor binary opens its own credential at a new
	// path, which is a different act from rein handling the token.
	info, err := os.Lstat(filepath.Join(h.dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("auth.json is a copy; it must be a link, so no credential is ever read or duplicated")
	}
	target, err := os.Readlink(filepath.Join(h.dir, "auth.json"))
	if err != nil || target != filepath.Join(real, "auth.json") {
		t.Errorf("auth.json links to %q, want the real home's", target)
	}

	if env := h.env(); env["CODEX_HOME"] != h.dir {
		t.Errorf("env = %v, want CODEX_HOME pointing at the private home", env)
	}
	if strings.Contains(h.dir, "run-42") == false {
		t.Errorf("the private home %q does not name its run; an abandoned one should say who left it", h.dir)
	}
}

// The regression test for ark:rein#38. A private home that links the
// developer's sessions/ makes `codex app-server` index every rollout in it
// before answering `initialize` — 1.9 GB / 64k rollouts on his Mac, past the
// 90 s startup timeout under launchd — and no Codex run started. Linking his
// state DB too made the start fast and broke resume, because Codex records
// rollout paths through the private home. So the history a session sees is
// Rein's own, and nothing of the developer's but the login is linked.
func TestPrivateHomeUsesReinsOwnHistory(t *testing.T) {
	real := fakeRealHome(t)
	h, err := newPrivateHome("run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.remove()

	reinHome, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(reinHome, "codex", "sessions")
	target, err := os.Readlink(filepath.Join(h.dir, "sessions"))
	if err != nil || target != want {
		t.Fatalf("sessions links to %q (%v), want Rein's own history %q", target, err, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("Rein's Codex history was not created: %v", err)
	}

	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "auth.json" {
			continue
		}
		if target, err := os.Readlink(filepath.Join(h.dir, e.Name())); err == nil &&
			strings.HasPrefix(target, real+string(os.PathSeparator)) {
			t.Errorf("%s links into the developer's Codex home (%s); only the login may", e.Name(), target)
		}
	}

	// A start that times out says which history it handed Codex to index.
	if hint := h.startupHint(); !strings.Contains(hint, want) || !strings.Contains(hint, "initialize") {
		t.Errorf("startupHint = %q, want it to name %s and the step it slows", hint, want)
	}
	var nilHome *privateHome
	if nilHome.startupHint() != "" {
		t.Error("a nil private home must be inert")
	}
}

func TestPrivateHomeRefusesWithoutALogin(t *testing.T) {
	// A session that cannot be isolated must not start: the failure mode is a
	// run acting as the developer, which is worse than a run that does not
	// happen.
	t.Setenv("CODEX_HOME", t.TempDir())
	_, err := newPrivateHome("run-1")
	if !errors.Is(err, adapter.ErrPreflight) {
		t.Fatalf("newPrivateHome without a login = %v, want ErrPreflight", err)
	}
	if !strings.Contains(err.Error(), "codex login") {
		t.Errorf("the error does not say what to do: %v", err)
	}
}

func TestPrivateHomeRewritesTheTranscriptPath(t *testing.T) {
	fakeRealHome(t)
	h, err := newPrivateHome("run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.remove()

	reported := filepath.Join(h.dir, "sessions", "2026", "rollout-abc.jsonl")
	want := filepath.Join(h.history, "sessions", "2026", "rollout-abc.jsonl")
	if got := h.transcriptPath(reported); got != want {
		t.Errorf("transcriptPath = %q, want %q — the file lives in Rein's history "+
			"through the sessions link, and the private one is about to be deleted", got, want)
	}
	// A path that is not inside the private home is left alone, and so is the
	// nil receiver a test double produces.
	if got := h.transcriptPath("/elsewhere/x.jsonl"); got != "/elsewhere/x.jsonl" {
		t.Errorf("transcriptPath rewrote an unrelated path: %q", got)
	}
	var nilHome *privateHome
	if got := nilHome.transcriptPath("/x"); got != "/x" {
		t.Errorf("a nil private home must be inert, got %q", got)
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

	// Codex rotating its own token replaces the link with a real file holding
	// a *newer* credential. Deleting it would log the developer out; copying it
	// back would mean this package handling a token. It does neither.
	link := filepath.Join(h.dir, "auth.json")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte(`{"newer":"credential"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	warning := h.rotatedLogin()
	if warning == "" {
		t.Fatal("a rotated login was not noticed")
	}
	if !strings.Contains(warning, h.dir) || !strings.Contains(warning, h.real) {
		t.Errorf("the warning names neither directory, so nobody can act on it: %q", warning)
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
	// The real home is untouched — the links were followed, never emptied.
	if _, err := os.Stat(filepath.Join(h.real, "auth.json")); err != nil {
		t.Errorf("removing the private home damaged the real one: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.real, "sessions", "2026")); err != nil {
		t.Errorf("removing the private home damaged the real sessions: %v", err)
	}
	// And Rein's own history outlives every session: it is what resume and a
	// transcript path point at.
	if _, err := os.Stat(filepath.Join(h.history, "sessions")); err != nil {
		t.Errorf("removing the private home took Rein's Codex history with it: %v", err)
	}
}

func TestMergeMapsLetsTheSpecWin(t *testing.T) {
	// A run that names CODEX_HOME means it; overriding it silently would be
	// the same class of mistake as silently downgrading a permission mode.
	got := mergeMaps(map[string]string{"CODEX_HOME": "/private"},
		map[string]string{"CODEX_HOME": "/explicit", "OTHER": "1"})
	if got["CODEX_HOME"] != "/explicit" || got["OTHER"] != "1" {
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
	if got := sanitizeRunID(""); got != "" {
		t.Errorf("an empty run id should add nothing, got %q", got)
	}
}
