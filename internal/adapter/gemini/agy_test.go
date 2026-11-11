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
		RealHome: t.TempDir(), Items: items{KeyService + "/" + KeyName: testKey}, InterruptGrace: time.Second}
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

// Installed, Antigravity runs — no switch, no key (Casey's ruling,
// 2026-11-11). The session's settings name no model provider, so the person's
// own setup stands; no key is set when none is stored; a queue's variables
// reach it; the folder is confined by Antigravity's rules and never by
// skipping them; the prompt goes on stdin. On the old code this run was Gemini
// CLI (the switch was off) and, switched on, carried modelProvider "gemini".
func TestARunIsAntigravityOnThePersonsOwnSetup(t *testing.T) {
	agyOn(t)
	agyBin, rec := standin(t, AgyBinary, standinConfig{Fixture: "agy-tool.jsonl"})
	gemBin, gemRec := standin(t, Binary, standinConfig{Fixture: "text.jsonl", Version: "0.64.0"})
	a := agyAdapter(t, agyBin, gemBin)
	a.Items = items{}
	if m := a.Manifest(); m.Binary != AgyBinary || !m.Has(adapter.CapResume) || !m.Has(adapter.CapWorktree) {
		t.Fatalf("manifest = %+v", m)
	}
	if v := a.ProgramVersion(context.Background()); v != "1.5.5 (Antigravity)" {
		t.Fatalf("program version = %q", v)
	}
	spec := baseSpec(t)
	spec.ProviderKey = ""
	spec.SystemPrompt = "standing"
	spec.Model = "gemini-4-argon"
	spec.Effort = "high"
	spec.Env = map[string]string{"AGY_ACCOUNT": "person@example.com", "GOOGLE_CLOUD_PROJECT": "their-project"}
	evs, res, err := runToEnd(t, a, spec)
	if err != nil || res.Status != adapter.StatusSucceeded {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, err := readRecordErr(gemRec); err == nil {
		t.Fatal("Gemini CLI ran with Antigravity installed")
	}
	if res.SessionID != agyResumePrefix+"11111111-1111-4111-8111-111111111111" || res.Summary != "All done." {
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
			if strings.Contains(ev.Text, "key") {
				t.Errorf("the run says it is on a key: %q", ev.Text)
			}
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
	if strings.Contains(joined, "dangerously") || strings.Contains(joined, "do the thing") {
		t.Fatalf("args skip permissions or carry the prompt: %q", r.Args)
	}
	set := sessionSettings(t, r.Args)
	if _, ok := set["modelProvider"]; ok {
		t.Fatalf("the session moves Antigravity onto a model provider: %v", set)
	}
	if set["toolPermission"] != "proceed-in-sandbox" {
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
	if _, n := envValue(r.Env, KeyName); n != 0 {
		t.Fatalf("a key reached Antigravity though none is stored")
	}
	for name, want := range map[string]string{"AGY_ACCOUNT": "person@example.com", "GOOGLE_CLOUD_PROJECT": "their-project"} {
		if v, n := envValue(r.Env, name); n != 1 || v != want {
			t.Fatalf("%s reached Antigravity %d times as %q", name, n, v)
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

// A key the person stored for Rein reaches Antigravity once, in its
// environment and nowhere on the command line, for someone whose own
// settings put agy on a key.
func TestAStoredKeyReachesAntigravityOnce(t *testing.T) {
	agyOn(t)
	agyBin, rec := standin(t, AgyBinary, standinConfig{Fixture: "agy-tool.jsonl", NeedKey: true})
	a := agyAdapter(t, agyBin, "")
	spec := baseSpec(t)
	spec.Env = map[string]string{"FOO": "bar"}
	_, res, err := runToEnd(t, a, spec)
	if err != nil || res.Status != adapter.StatusSucceeded {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	r := readRecord(t, rec)
	if v, n := envValue(r.Env, KeyName); v != testKey || n != 1 {
		t.Fatalf("key in env %d times", n)
	}
	if strings.Contains(strings.Join(r.Args, " "), testKey) {
		t.Fatal("the key is on the command line")
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
	agyOn(t)
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
	agyOn(t)
	agyBin, _ := standin(t, AgyBinary, standinConfig{Fixture: "agy-denied.jsonl"})
	a := agyAdapter(t, agyBin, "")
	_, res, err := runToEnd(t, a, baseSpec(t))
	if res.Status != adapter.StatusFailed || err == nil || !strings.Contains(err.Error(), "refused write_file") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// A bad key ends the run in Google's own words, with the key never printed.
func TestABadKeyComesBackInGooglesWordsFromAntigravity(t *testing.T) {
	agyOn(t)
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

// An Antigravity that is not signed in asks for a Google sign-in and waits
// for a code (measured on the lab Mac). Nobody is there to give one, so the
// session is stopped at once and the run fails as "set up Antigravity first",
// a login failure (agent_login), not a refusal of the person's sign-in.
func TestAnAntigravityThatIsNotSignedInFailsAsNotSetUp(t *testing.T) {
	agyOn(t)
	agyBin, _ := standin(t, AgyBinary, standinConfig{
		Stderr: "Authentication required. Please visit the URL to log in:\n  https://accounts.google.com/o/oauth2/auth?x=y\n\nWaiting for authentication (timeout 60s)...",
		Hang:   true})
	a := agyAdapter(t, agyBin, "")
	start := time.Now()
	_, res, err := runToEnd(t, a, baseSpec(t))
	if res.Status != adapter.StatusFailed || !errors.Is(err, adapter.ErrLoginRequired) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if errors.Is(err, adapter.ErrNotPlanAuth) {
		t.Fatalf("reported as a refused sign-in: %v", err)
	}
	if !strings.Contains(err.Error(), NotSetUpLine(AgyName)) {
		t.Fatalf("err = %q", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("the run waited for the sign-in")
	}
}

// Fed its prompt on stdin as Rein does, an Antigravity that is not signed in
// ends the turn at once with "authentication failed or timed out" (measured on
// the lab Mac). That is "set up Antigravity first", not a generic failure.
func TestAnAntigravityTurnThatFailsToSignInIsNotSetUp(t *testing.T) {
	agyOn(t)
	agyBin, _ := standin(t, AgyBinary, standinConfig{Fixture: "agy-notsignedin.jsonl", Exit: 1,
		Stderr: "Error: authentication timed out.\nerror: authentication failed or timed out"})
	_, res, err := runToEnd(t, agyAdapter(t, agyBin, ""), baseSpec(t))
	if res.Status != adapter.StatusFailed || !errors.Is(err, adapter.ErrLoginRequired) ||
		!strings.Contains(err.Error(), NotSetUpLine(AgyName)) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// Preflight asks `agy models`, which says "Please sign in" and exits 0 when
// agy is not signed in (measured). On the old code Preflight only looked for a
// stored key.
func TestPreflightAsksAntigravityWhetherItIsSignedIn(t *testing.T) {
	agyOn(t)
	out := "Fetching available models...\nError: Please sign in to view available models. Launch the CLI without arguments to sign in."
	agyBin, _ := standin(t, AgyBinary, standinConfig{Models: out})
	a := agyAdapter(t, agyBin, "")
	a.Items = items{}
	err := a.Preflight(context.Background())
	if !errors.Is(err, adapter.ErrLoginRequired) || !errors.Is(err, adapter.ErrPreflight) ||
		!strings.Contains(err.Error(), NotSetUpLine(AgyName)) {
		t.Fatalf("err = %v", err)
	}
	okBin, okRec := standin(t, AgyBinary, standinConfig{Models: "Fetching available models...\ngemini-4-argon\ngemini-4-carbon"})
	if err := agyAdapter(t, okBin, "").Preflight(context.Background()); err != nil {
		t.Fatalf("a signed-in Antigravity failed preflight: %v", err)
	}
	// The stored key goes to the question too, for a person on a key.
	if b, err := os.ReadFile(filepath.Join(okRec, "models")); err != nil || strings.TrimSpace(string(b)) != testKey {
		t.Fatalf("models was not asked with the stored key: %q %v", b, err)
	}
}

// Without Antigravity installed, Gemini CLI runs.
func TestWithoutAntigravityGeminiCLIRuns(t *testing.T) {
	agyOn(t)
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

// The person's and the queue's variables reach Antigravity as they are; a
// stored key replaces any other GEMINI_API_KEY and appears once.
func TestAgyEnvKeepsThePersonsVariablesAndTheStoredKeyOnce(t *testing.T) {
	env := agyEnv([]string{"PATH=/bin", "gemini_api_key=old", "Google_Cloud_Project=p"},
		map[string]string{"AGY_ACCOUNT": "x", "FOO": "bar"}, testKey, "")
	for _, name := range []string{"PATH", "Google_Cloud_Project", "AGY_ACCOUNT", "FOO"} {
		if _, n := envValue(env, name); n != 1 {
			t.Fatalf("%s reached it %d times", name, n)
		}
	}
	if v, n := envValue(env, KeyName); v != testKey || n != 1 {
		t.Fatalf("key %d times", n)
	}
	if _, n := envValue(agyEnv([]string{"PATH=/bin"}, nil, "", ""), KeyName); n != 0 {
		t.Fatal("a key appeared from nowhere")
	}
}

// In a linked worktree, as Rein cuts them, a commit writes into git's shared
// folder outside the task folder: the run is granted exactly the parts a
// commit writes, never hooks or config, and the files that point git at a
// folder are denied, so a run cannot redirect git to hooks it planted. On a
// Mac the temporary folder is behind a symlink: the task folder is there under
// both names, and git's folders under the real one, which is the one git
// writes into the .git file and the one the sandbox sees.
func TestGrantsCoverACommitFromALinkedWorktree(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
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
	allow, deny, err := agyGrants(wt, true)
	must(t, err)
	realRoot, err := filepath.EvalSymlinks(root)
	must(t, err)
	if !slices.Contains(allow, "write_file("+rulePath(wt)+")") {
		t.Fatalf("grants %q lack the task folder as named", allow)
	}
	for _, r := range []string{realRoot} {
		common := filepath.Join(r, "repo", ".git")
		private := filepath.Join(common, "worktrees", "wt")
		for _, want := range []string{
			"write_file(" + rulePath(filepath.Join(r, "wt")) + ")",
			"write_file(" + rulePath(private) + ")",
			"write_file(" + rulePath(filepath.Join(common, "objects")) + ")",
			"write_file(" + rulePath(filepath.Join(common, "refs")) + ")",
			"write_file(" + rulePath(filepath.Join(common, "packed-refs.lock")) + ")",
			"read_file(" + rulePath(common) + ")",
		} {
			if !slices.Contains(allow, want) {
				t.Fatalf("grants %q lack %q", allow, want)
			}
		}
		for _, want := range []string{
			"write_file(" + rulePath(filepath.Join(r, "wt", ".git")) + ")",
			"write_file(" + rulePath(filepath.Join(private, "commondir")) + ")",
			"write_file(" + rulePath(filepath.Join(private, "gitdir")) + ")",
			"write_file(" + rulePath(filepath.Join(private, "config.worktree")) + ")",
		} {
			if !slices.Contains(deny, want) {
				t.Fatalf("denies %q lack %q", deny, want)
			}
		}
		for _, rule := range allow {
			if strings.Contains(rule, "hooks") || strings.HasSuffix(rule, "config)") || rule == "write_file("+rulePath(common)+")" {
				t.Fatalf("over-broad grant %q", rule)
			}
		}
	}
	if !slices.Contains(allow, "read_url(*)") {
		t.Fatal("no network for the sandbox")
	}
	ro, roDeny, err := agyGrants(wt, false)
	must(t, err)
	for _, rule := range ro {
		if strings.HasPrefix(rule, "write_file(") {
			t.Fatalf("read-only grant %q", rule)
		}
	}
	if len(roDeny) == 0 {
		t.Fatal("a read-only run lost the git denies")
	}
}

// A plain repository's git folder is inside the task folder, so its config
// and hooks are denied outright.
func TestGrantsDenyAPlainRepositorysHooksAndConfig(t *testing.T) {
	wt := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(wt, ".git", "hooks"), 0o700))
	_, deny, err := agyGrants(wt, true)
	must(t, err)
	for _, f := range []string{"config", "hooks"} {
		if want := "write_file(" + rulePath(filepath.Join(wt, ".git", f)) + ")"; !slices.Contains(deny, want) {
			t.Fatalf("denies %q lack %q", deny, want)
		}
	}
}

// Gemini CLI runs a whole session on every platform, Windows included
// (ark:rein#151): its private home, trust, a stored key once, the prompt on stdin,
// and a result read from its stream.
func TestGeminiCLIRunsOnEveryPlatform(t *testing.T) {
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
