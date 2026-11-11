package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// agyAdapter is an adapter whose two programs are stand-ins.
func agyAdapter(t *testing.T, agyBin, geminiBin string) *Adapter {
	t.Helper()
	if geminiBin == "" {
		geminiBin = filepath.Join(t.TempDir(), "no-gemini-here")
	}
	if agyBin == "" {
		agyBin = filepath.Join(t.TempDir(), "no-agy-here")
	}
	return &Adapter{binary: geminiBin, agyBinary: agyBin, Root: filepath.Join(t.TempDir(), "gemini"),
		Items: items{KeyService + "/" + KeyName: testKey}, InterruptGrace: time.Second}
}

func runToEnd(t *testing.T, a *Adapter, spec adapter.RunSpec) ([]adapter.Event, adapter.Result, error) {
	t.Helper()
	s, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs := drainAll(t, s)
	res, werr := s.Wait()
	return evs, res, werr
}

func drainAll(t *testing.T, s adapter.Session) []adapter.Event {
	t.Helper()
	var evs []adapter.Event
	timeout := time.After(30 * time.Second)
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

func sessionSettings(t *testing.T, args []string) map[string]any {
	t.Helper()
	i := slices.Index(args, "--settings")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no --settings in %q", args)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args[i+1]), &m); err != nil {
		t.Fatalf("--settings is not JSON: %v", err)
	}
	return m
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// Off, Rein never runs Antigravity — not a run, not a version probe, not a
// preflight — even with it installed. This is the switch's whole promise
// until Google says yes.
func TestSwitchedOffAntigravityIsNeverRun(t *testing.T) {
	SetAntigravity(false)
	agyBin, agyRec := standin(t, AgyBinary, standinConfig{Fixture: "agy-tool.jsonl"})
	gemBin, gemRec := standin(t, Binary, standinConfig{Fixture: "text.jsonl", Version: "0.64.0"})
	a := agyAdapter(t, agyBin, gemBin)

	if m := a.Manifest(); m.Binary != Binary {
		t.Fatalf("manifest binary = %q, want Gemini CLI's", m.Binary)
	}
	if v := a.ProgramVersion(context.Background()); v != "0.64.0 (Gemini CLI)" {
		t.Fatalf("program version = %q", v)
	}
	_ = a.Preflight(context.Background())
	_, _, _ = runToEnd(t, a, baseSpec(t))
	if _, err := os.Stat(filepath.Join(gemRec, "run.json")); err != nil {
		t.Fatalf("Gemini CLI did not run: %v", err)
	}
	for _, f := range []string{"run.json", "versions"} {
		if _, err := os.Stat(filepath.Join(agyRec, f)); err == nil {
			t.Fatalf("Antigravity was executed (%s) while switched off", f)
		}
	}
}

// On, and installed, a run is Antigravity on the key: key mode in the
// session's own settings, the folder confined by Antigravity's rules and
// never by skipping them, the key in its environment once, the prompt on
// stdin, and nothing inherited that could move it to a sign-in or another
// host.
func TestSwitchedOnARunIsAntigravityOnTheKey(t *testing.T) {
	switchOn(t)
	t.Setenv("GOOGLE_GEMINI_BASE_URL", "http://elsewhere.example")
	t.Setenv("AGY_ADC_AUTH", "true")
	t.Setenv("Agy_Account", "someone@example.com")
	agyBin, rec := standin(t, AgyBinary, standinConfig{Fixture: "agy-tool.jsonl"})
	a := agyAdapter(t, agyBin, "")
	if m := a.Manifest(); m.Binary != AgyBinary || !m.Has(adapter.CapResume) || !m.Has(adapter.CapWorktree) {
		t.Fatalf("manifest = %+v", m)
	}
	if v := a.ProgramVersion(context.Background()); v != "1.5.5 (Antigravity)" {
		t.Fatalf("program version = %q", v)
	}
	spec := baseSpec(t)
	spec.SystemPrompt = "standing"
	spec.Model = "gemini-4-argon"
	spec.Effort = "high"
	evs, res, err := runToEnd(t, a, spec)
	if err != nil || res.Status != adapter.StatusSucceeded {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if res.SessionID != "11111111-1111-4111-8111-111111111111" || res.Summary != "All done." {
		t.Fatalf("session id %q summary %q", res.SessionID, res.Summary)
	}
	if res.Usage.InputTokens != 30 || res.Usage.OutputTokens != 15 {
		t.Fatalf("usage %+v", res.Usage)
	}
	var tools []string
	ran := false
	for _, ev := range evs {
		if ev.Kind == adapter.EventToolUse {
			tools = append(tools, ev.Tool.Name)
		}
		if ev.Kind == adapter.EventProgress && strings.Contains(ev.Text, "Running Antigravity") {
			ran = true
		}
	}
	if !slices.Equal(tools, []string{"write_file", "run_command"}) || !ran {
		t.Fatalf("tools %q, said which program ran: %v", tools, ran)
	}

	r := readRecord(t, rec)
	joined := strings.Join(r.Args, " ")
	for _, want := range []string{"--print=", "--input-format=stream-json", "--output-format=stream-json",
		"--disable-slash-commands", "--sandbox", "--mode accept-edits", "--model gemini-4-argon", "--effort high"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args lack %q: %q", want, r.Args)
		}
	}
	if strings.Contains(joined, "dangerously") || strings.Contains(joined, testKey) || strings.Contains(joined, "do the thing") {
		t.Fatalf("args skip permissions or carry the key or prompt: %q", r.Args)
	}
	set := sessionSettings(t, r.Args)
	if set["modelProvider"] != "gemini" || set["toolPermission"] != "proceed-in-sandbox" {
		t.Fatalf("settings %v", set)
	}
	perms := set["permissions"].(map[string]any)
	allow, deny := strs(perms["allow"]), strs(perms["deny"])
	if !slices.Contains(allow, "write_file("+rulePath(spec.WorktreeDir)+")") || !slices.Contains(deny, "mcp(*)") {
		t.Fatalf("allow %q deny %q", allow, deny)
	}
	real, err := filepath.EvalSymlinks(spec.WorktreeDir)
	must(t, err)
	for _, rule := range allow {
		if strings.HasPrefix(rule, "write_file(") && rule != "write_file("+rulePath(spec.WorktreeDir)+")" &&
			rule != "write_file("+rulePath(real)+")" {
			t.Fatalf("a write grant outside the task folder in a folder that is not a repository: %q", rule)
		}
	}
	if v, n := envValue(r.Env, KeyName); v != testKey || n != 1 {
		t.Fatalf("key in env %d times", n)
	}
	for _, name := range []string{"GOOGLE_GEMINI_BASE_URL", "AGY_ADC_AUTH", "AGY_ACCOUNT"} {
		if _, n := envValue(r.Env, name); n != 0 {
			t.Fatalf("%s reached Antigravity", name)
		}
	}
	var msg struct {
		Event   string `json:"event"`
		Message struct {
			Role, Content string
		} `json:"message"`
	}
	must(t, json.Unmarshal([]byte(r.Stdin), &msg))
	if msg.Event != "user" || msg.Message.Content != "standing\n\n---\n\ndo the thing" {
		t.Fatalf("stdin %q", r.Stdin)
	}
	if !samePath(r.Dir, spec.WorktreeDir) {
		t.Fatalf("ran in %q, want %q", r.Dir, spec.WorktreeDir)
	}
}

func samePath(a, b string) bool {
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && strings.EqualFold(filepath.Clean(ea), filepath.Clean(eb))
}

// A read-only run grants no writes at all and runs in the default mode, where
// every edit asks and headless asking is a refusal.
func TestReadOnlyGrantsNoWrites(t *testing.T) {
	switchOn(t)
	agyBin, rec := standin(t, AgyBinary, standinConfig{Fixture: "agy-tool.jsonl"})
	a := agyAdapter(t, agyBin, "")
	spec := baseSpec(t)
	spec.PermissionMode = adapter.PermissionReadOnly
	_, _, _ = runToEnd(t, a, spec)
	r := readRecord(t, rec)
	if slices.Contains(r.Args, "--mode") {
		t.Fatalf("read-only set a mode: %q", r.Args)
	}
	for _, rule := range strs(sessionSettings(t, r.Args)["permissions"].(map[string]any)["allow"]) {
		if strings.HasPrefix(rule, "write_file(") {
			t.Fatalf("read-only granted %q", rule)
		}
	}
}

// Antigravity ends its turn with status SUCCESS when it refuses an action
// (measured); Rein must not call that done.
func TestARefusedActionIsNotDone(t *testing.T) {
	switchOn(t)
	agyBin, _ := standin(t, AgyBinary, standinConfig{Fixture: "agy-denied.jsonl"})
	a := agyAdapter(t, agyBin, "")
	_, res, err := runToEnd(t, a, baseSpec(t))
	if res.Status != adapter.StatusFailed || err == nil || !strings.Contains(err.Error(), "refused write_file") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// A bad key ends the run in Google's own words, with the key never printed.
func TestABadKeyComesBackInGooglesWordsFromAntigravity(t *testing.T) {
	switchOn(t)
	agyBin, _ := standin(t, AgyBinary, standinConfig{Fixture: "agy-badkey.jsonl", Exit: 3})
	a := agyAdapter(t, agyBin, "")
	_, res, err := runToEnd(t, a, baseSpec(t))
	if res.Status != adapter.StatusFailed || err == nil || !strings.Contains(err.Error(), "API key not valid") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatal("the key is in the error")
	}
}

// A session that starts a Google sign-in anyway is stopped and fails as not
// on the key.
func TestAnAntigravitySignInIsStopped(t *testing.T) {
	switchOn(t)
	agyBin, _ := standin(t, AgyBinary, standinConfig{Stderr: "Error: authentication required. Opening browser to sign in", Hang: true})
	a := agyAdapter(t, agyBin, "")
	_, res, err := runToEnd(t, a, baseSpec(t))
	if res.Status != adapter.StatusFailed || !errors.Is(err, adapter.ErrNotPlanAuth) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// With the switch on but Antigravity not installed, Gemini CLI runs as before.
func TestSwitchedOnWithoutAntigravityStaysOnGeminiCLI(t *testing.T) {
	switchOn(t)
	gemBin, _ := standin(t, Binary, standinConfig{Version: "0.64.0"})
	a := agyAdapter(t, "", gemBin)
	if m := a.Manifest(); m.Binary != Binary {
		t.Fatalf("manifest binary %q", m.Binary)
	}
	if v := a.ProgramVersion(context.Background()); v != "0.64.0 (Gemini CLI)" {
		t.Fatalf("program version %q", v)
	}
}

// Both programs run on Windows (ark:rein#151): the Windows app offers Gemini,
// so Rein must accept a gemini queue there.
func TestBothProgramsListWindows(t *testing.T) {
	for _, m := range []adapter.Manifest{geminiCLIManifest(), agyManifest()} {
		ok := false
		for _, p := range m.Platforms {
			if p.OS == "windows" {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("%s leaves out Windows", m.Binary)
		}
	}
}

// Every variable that could move a session is stripped, whatever its case.
func TestAgyEnvStripsWhateverTheCase(t *testing.T) {
	env := agyEnv([]string{"PATH=/bin", "gemini_api_key=old", "Google_Cloud_Project=p", "ANTIGRAVITY_LS_ADDRESS=x", "JETSKI_USER=x"},
		map[string]string{"AGY_LLM_GATEWAY_API_KEY": "x", "FOO": "bar"}, testKey, "")
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k != "PATH" && k != "FOO" && k != KeyName {
			t.Fatalf("%s survived", kv)
		}
	}
	if v, n := envValue(env, KeyName); v != testKey || n != 1 {
		t.Fatalf("key %d times", n)
	}
}

// In a linked worktree, as Rein cuts them, a commit writes into git's shared
// folder outside the task folder: the run is granted exactly the parts a
// commit writes, never hooks or config.
func TestGrantsCoverACommitFromALinkedWorktree(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	repo, wt := filepath.Join(root, "repo"), filepath.Join(root, "wt")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", repo, "worktree", "add", "-q", "-b", "task", wt},
	} {
		if out, err := exec.Command(git, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	allow, err := agyGrants(wt, true)
	must(t, err)
	common := filepath.Join(repo, ".git")
	for _, want := range []string{
		"write_file(" + rulePath(wt) + ")",
		"write_file(" + rulePath(filepath.Join(common, "worktrees", "wt")) + ")",
		"write_file(" + rulePath(filepath.Join(common, "objects")) + ")",
		"write_file(" + rulePath(filepath.Join(common, "refs")) + ")",
		"write_file(" + rulePath(filepath.Join(common, "packed-refs.lock")) + ")",
		"read_file(" + rulePath(common) + ")",
		"read_url(*)",
	} {
		if !slices.Contains(allow, want) {
			t.Fatalf("grants %q lack %q", allow, want)
		}
	}
	for _, rule := range allow {
		if strings.Contains(rule, "hooks") || strings.HasSuffix(rule, "config)") || rule == "write_file("+rulePath(common)+")" {
			t.Fatalf("over-broad grant %q", rule)
		}
	}
	ro, err := agyGrants(wt, false)
	must(t, err)
	for _, rule := range ro {
		if strings.HasPrefix(rule, "write_file(") {
			t.Fatalf("read-only grant %q", rule)
		}
	}
}

// Gemini CLI runs a whole session on every platform, Windows included
// (ark:rein#151): its private home, trust, the key once, the prompt on stdin,
// and a result read from its stream.
func TestGeminiCLIRunsOnEveryPlatform(t *testing.T) {
	SetAntigravity(false)
	gemBin, rec := standin(t, Binary, standinConfig{Fixture: "tool.jsonl", Version: "0.64.0"})
	a := agyAdapter(t, "", gemBin)
	spec := baseSpec(t)
	_, res, err := runToEnd(t, a, spec)
	if err != nil || res.Status != adapter.StatusSucceeded {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	r := readRecord(t, rec)
	if v, n := envValue(r.Env, KeyName); v != testKey || n != 1 {
		t.Fatalf("key in env %d times", n)
	}
	if v, _ := envValue(r.Env, "GEMINI_CLI_TRUST_WORKSPACE"); v != "true" {
		t.Fatal("the worktree is not trusted")
	}
	if v, _ := envValue(r.Env, "GEMINI_CLI_HOME"); v == "" {
		t.Fatal("no private Gemini CLI home")
	}
	if r.Stdin != "do the thing" || !slices.Contains(r.Args, "--skip-trust") {
		t.Fatalf("stdin %q args %q", r.Stdin, r.Args)
	}
}
