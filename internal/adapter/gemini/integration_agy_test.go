//go:build integration

package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// The real Antigravity program (`agy`), driven by Rein as a run is. Run with:
//
//	REIN_AGY_LIVE=1 go test -tags integration ./internal/adapter/gemini/ -run LiveAgy -v
//
// Without a key these talk to a stand-in for the Gemini API on this machine,
// never to Google. Every agy request leaves through a logging proxy, and the
// hosts it reached are printed, so a run on a computer signed in to
// Antigravity shows whether anything but the stand-in was contacted.
//
// With REIN_AGY_LIVE_KEY_FILE naming a file that holds a real Google AI
// Studio key, TestLiveAgyRealKeyFinishesATask sends one small task to Google
// on that key. The key is read from the file and never printed.
//
// HOME is left as it is unless REIN_AGY_PRIVATE_HOME=1: the point on a lab
// machine signed in to Antigravity is to run beside that sign-in.

// realHome is the home before any private one replaces it.
var realHome string

func liveAgy(t *testing.T) *Adapter {
	t.Helper()
	if os.Getenv("REIN_AGY_LIVE") != "1" {
		t.Skip("set REIN_AGY_LIVE=1 to run the real Antigravity program")
	}
	a := &Adapter{Root: t.TempDir(), InterruptGrace: 5 * time.Second}
	bin, err := a.resolveAgy()
	if err != nil {
		t.Skipf("Antigravity is not installed: %v", err)
	}
	a.agyBinary = bin
	realHome, _ = os.UserHomeDir()
	if os.Getenv("REIN_AGY_PRIVATE_HOME") == "1" {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
	}
	SetAntigravity(true)
	t.Cleanup(func() { SetAntigravity(false) })
	return a
}

// egress is a forward proxy that records every host a request asked for and
// lets it through.
type egress struct {
	mu    sync.Mutex
	hosts map[string]int
	srv   *httptest.Server
}

func newEgress(t *testing.T) *egress {
	e := &egress{hosts: map[string]int{}}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		e.mu.Lock()
		e.hosts[strings.Split(host, ":")[0]]++
		e.mu.Unlock()
		if r.Method != http.MethodConnect {
			http.Error(w, "plain http not relayed", http.StatusBadGateway)
			return
		}
		up, err := net.DialTimeout("tcp", host, 10*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			up.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		down, _, err := hj.Hijack()
		if err != nil {
			up.Close()
			return
		}
		go func() { _, _ = io.Copy(up, down); up.Close() }()
		go func() { _, _ = io.Copy(down, up); down.Close() }()
	}))
	t.Cleanup(e.srv.Close)
	t.Setenv("HTTPS_PROXY", e.srv.URL)
	t.Setenv("HTTP_PROXY", e.srv.URL)
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Cleanup(func() { t.Logf("hosts Antigravity reached through the proxy: %s", e.report()) })
	return e
}

func (e *egress) report() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.hosts) == 0 {
		return "none"
	}
	var out []string
	for h, n := range e.hosts {
		out = append(out, fmt.Sprintf("%s (%d)", h, n))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// signInHosts are Google's sign-in and account-backed endpoints. A run on
// the key must reach none of them.
var signInHosts = []string{"oauth2.googleapis.com", "accounts.google.com", "cloudcode-pa.googleapis.com",
	"daily-cloudcode-pa.googleapis.com", "www.googleapis.com", "aiplatform.googleapis.com"}

func (e *egress) assertNoSignIn(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, h := range signInHosts {
		if e.hosts[h] > 0 {
			t.Errorf("Antigravity reached %s, a Google sign-in endpoint, on a run that must use only the key", h)
		}
	}
}

// standinAPI answers like the Gemini API: each request gets the next scripted
// function call, then plain text. It records which credentials arrived.
type standinAPI struct {
	mu       sync.Mutex
	calls    []map[string]any
	reqs     int
	keyHdr   int
	authHdr  int
	badKey   bool
	answered []string
	srv      *httptest.Server
}

func newStandinAPI(t *testing.T, calls []map[string]any, badKey bool) *standinAPI {
	s := &standinAPI{calls: calls, badKey: badKey}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *standinAPI) serve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Contents []struct {
			Parts []map[string]json.RawMessage `json:"parts"`
		} `json:"contents"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	responses := 0
	var last string
	for _, c := range body.Contents {
		for _, p := range c.Parts {
			if fr, ok := p["functionResponse"]; ok {
				responses++
				last = string(fr)
			}
		}
	}
	s.mu.Lock()
	s.reqs++
	if r.Header.Get("x-goog-api-key") != "" {
		s.keyHdr++
	}
	if r.Header.Get("Authorization") != "" {
		s.authHdr++
	}
	if last != "" {
		s.answered = append(s.answered, last)
	}
	bad := s.badKey
	var part map[string]any
	if responses < len(s.calls) {
		part = map[string]any{"functionCall": s.calls[responses]}
	} else {
		part = map[string]any{"text": "All done."}
	}
	s.mu.Unlock()
	if bad {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`)
		return
	}
	blob, _ := json.Marshal(map[string]any{
		"candidates":    []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{part}}, "finishReason": "STOP", "index": 0}},
		"usageMetadata": map[string]any{"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15},
		"modelVersion":  "gemini-3.1-pro-preview",
	})
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\r\n\r\n", blob)
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

// linkedWorktree makes a repository and a linked worktree of it, as Rein cuts
// a task's folder.
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

func shellWrite(path string) string {
	if runtime.GOOS == "windows" {
		return "Set-Content -Path '" + path + "' -Value x"
	}
	return "echo x > '" + path + "'"
}

func runCommand(line string) map[string]any {
	return map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": line, "toolSummary": "t", "WaitMsBeforeAsync": 8000}}
}

// A fake key reaches the stand-in in the key's header, never as a sign-in,
// and Google's refusal comes back as the run's error.
func TestLiveAgyAFakeKeyGoesOnlyToTheKeyEndpoint(t *testing.T) {
	a := liveAgy(t)
	e := newEgress(t)
	api := newStandinAPI(t, nil, true)
	a.agyBaseURL = api.srv.URL
	_, wt := linkedWorktree(t)
	res, err := liveRun(t, a, liveSpec(t, wt))
	if res.Status != adapter.StatusFailed || err == nil || !strings.Contains(err.Error(), "API key not valid") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if api.reqs == 0 || api.keyHdr != api.reqs || api.authHdr != 0 {
		t.Fatalf("stand-in saw %d requests, %d with the key header, %d with an Authorization header", api.reqs, api.keyHdr, api.authHdr)
	}
	e.assertNoSignIn(t)
}

// Edits and commands stay in the task's folder by Antigravity's own rules; a
// commit from the linked worktree works; a write outside is refused and the
// run is not reported done.
func TestLiveAgyEditsStayInTheTasksFolder(t *testing.T) {
	a := liveAgy(t)
	newEgress(t)
	repo, wt := linkedWorktree(t)
	// A folder in the person's real home, not under the temp folder the
	// sandbox allows.
	outsideHome, err := os.MkdirTemp(realHome, ".rein-agy-live-")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(outsideHome) })
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	calls := []map[string]any{
		{"name": "write_file", "args": map[string]any{"Path": filepath.Join(wt, "inside.txt"), "Content": "in"}},
		runCommand(shellWrite(filepath.Join(wt, "cmd-inside.txt"))),
		runCommand("git add -A && git -c user.name=t -c user.email=t@t commit -q -m work"),
		runCommand(shellWrite(filepath.Join(outsideHome, "cmd-outside.txt"))),
		runCommand(shellWrite(hook)),
		{"name": "write_file", "args": map[string]any{"Path": filepath.Join(outsideHome, "tool-outside.txt"), "Content": "out"}},
	}
	api := newStandinAPI(t, calls, false)
	a.agyBaseURL = api.srv.URL
	res, err := liveRun(t, a, liveSpec(t, wt))
	for _, ans := range api.answered {
		t.Logf("answer: %.300s", ans)
	}
	for _, f := range []string{"inside.txt", "cmd-inside.txt"} {
		if _, err := os.Stat(filepath.Join(wt, f)); err != nil {
			t.Errorf("%s was not written inside the folder: %v", f, err)
		}
	}
	out, _ := exec.Command("git", "-C", wt, "log", "--oneline", "-1").Output()
	if !strings.Contains(string(out), "work") {
		t.Errorf("the commit did not land: %q", out)
	}
	for _, f := range []string{filepath.Join(outsideHome, "cmd-outside.txt"), hook, filepath.Join(outsideHome, "tool-outside.txt")} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was written outside the folder", f)
		}
	}
	if res.Status != adapter.StatusFailed || err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("a refused write was reported as res=%+v err=%v", res, err)
	}
}

// A real key: one small task, sent to Google, finished in the folder.
func TestLiveAgyRealKeyFinishesATask(t *testing.T) {
	path := os.Getenv("REIN_AGY_LIVE_KEY_FILE")
	if path == "" {
		t.Skip("set REIN_AGY_LIVE_KEY_FILE to a file holding a real key")
	}
	a := liveAgy(t)
	e := newEgress(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the key file: %v", err)
	}
	_, wt := linkedWorktree(t)
	spec := liveSpec(t, wt)
	spec.ProviderKey = strings.TrimSpace(string(raw))
	spec.Model = os.Getenv("REIN_AGY_LIVE_MODEL")
	spec.Prompt = "Create a file named hello.txt in the current folder containing exactly the word hi, " +
		"then commit it with git with the message 'hello'. Do nothing else."
	res, err := liveRun(t, a, spec)
	if err != nil || res.Status != adapter.StatusSucceeded {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	b, err := os.ReadFile(filepath.Join(wt, "hello.txt"))
	if err != nil || !strings.Contains(string(b), "hi") {
		t.Fatalf("hello.txt: %q %v", b, err)
	}
	out, _ := exec.Command("git", "-C", wt, "log", "--oneline", "-1").Output()
	t.Logf("last commit: %s; usage %+v; summary %q", strings.TrimSpace(string(out)), res.Usage, res.Summary)
	e.assertNoSignIn(t)
}
