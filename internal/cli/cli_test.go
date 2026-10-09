package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/cli"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
)

// run executes the tree with an isolated Rein home and the file-backed keyring,
// so no test ever touches the developer's real config or prompts their keychain.
func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if os.Getenv(config.EnvHome) == "" {
		t.Setenv(config.EnvHome, t.TempDir())
	}
	t.Setenv(keyring.EnvBackend, "file")
	// Declare the host capability list rather than probing for it, so a test
	// never shells out to `gh auth status` and never depends on whether this
	// machine happens to be logged in.
	t.Setenv(runner.EnvCapabilities, "github-cli")
	var out, errb bytes.Buffer
	code = cli.Execute(cli.Options{Version: "v0.0.0-test", Args: args, Stdout: &out, Stderr: &errb})
	return out.String(), errb.String(), code
}

func TestHelpListsEveryCommand(t *testing.T) {
	out, _, code := run(t, "--help")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"enrol", "run", "attach", "tail", "status", "version", "service", "config"} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not mention %q:\n%s", want, out)
		}
	}
}

func TestBareInvocationPrintsHelp(t *testing.T) {
	out, _, code := run(t)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — a service manager launching `rein` with no args needs a legible answer", code)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("bare invocation did not print usage:\n%s", out)
	}
}

func TestPerCommandHelp(t *testing.T) {
	for _, cmd := range []string{"enrol", "run", "attach", "tail", "status", "version", "service", "config"} {
		t.Run(cmd, func(t *testing.T) {
			out, _, code := run(t, cmd, "--help")
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if !strings.Contains(out, "rein "+cmd) {
				t.Errorf("help for %q does not name the command:\n%s", cmd, out)
			}
		})
	}
}

func TestFlagsExist(t *testing.T) {
	// The flags are part of the contract even where the command is still a
	// stub: Lane B and the run-loop lane build against this surface.
	for _, tc := range []struct {
		command string
		flags   []string
	}{
		{"enrol", []string{"--claim-code", "--token", "--workspace", "--queue", "--agent-kind", "--host-id", "--mcp-url", "--revoke"}},
		{"run", []string{"--queue", "--once", "--poll-interval", "--max-concurrent", "--dry-run"}},
		{"attach", []string{"--read-only"}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			out, _, _ := run(t, tc.command, "--help")
			for _, f := range tc.flags {
				if !strings.Contains(out, f) {
					t.Errorf("%s help is missing %s:\n%s", tc.command, f, out)
				}
			}
		})
	}
}

// Nothing is a stub any more: `attach` was the last one and ark:rein#26
// implemented it. A command that works must not answer with the stub's exit
// code, and none of them describe themselves as unbuilt.
// shortTempDir is a temp directory whose name does not carry the test's, for
// the tests whose Rein home has to hold a unix socket path.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestNoCommandIsAStub(t *testing.T) {
	for _, c := range cli.NewRootCommand("v0").Commands() {
		if strings.Contains(c.Long, "not implemented in v0") {
			t.Errorf("%s still describes itself as a stub", c.Name())
		}
	}
}

// With no `rein run` going there is nothing to attach to, and the error has to
// say what to do about that rather than only that a socket was missing.
//
// The home is a SHORT temp directory, not t.TempDir(): the latter embeds the
// test's name, and on the macOS runner that came to 102 bytes with the
// endpoint on the end — two short of the kernel's limit, which is a different
// error and a different message.
func TestAttachWithNoRunner(t *testing.T) {
	t.Setenv(config.EnvHome, shortTempDir(t))
	_, errb, code := run(t, "attach", "run-123")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\nstderr: %s", code, errb)
	}
	for _, want := range []string{"no runner is listening", "rein service status", "rein tail"} {
		if !strings.Contains(errb, want) {
			t.Errorf("stderr does not mention %q: %q", want, errb)
		}
	}
}

// --read-only never opens a control connection: following a run without taking
// it over is exactly `rein tail`, and the daemon must not step back for
// somebody who has no intention of driving.
func TestAttachReadOnlyNeedsNoRunner(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	store, err := runlog.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	w := store.Create(runlog.Meta{RunID: "abc12345", Queue: "mac-claude"})
	w.Event(adapter.Event{Kind: adapter.EventText, Text: "still going"})
	w.Close(runlog.StatusSubmitted)

	out, errb, code := run(t, "attach", "abc12345", "--read-only")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, errb)
	}
	if !strings.Contains(out, "read-only") || !strings.Contains(out, "still going") {
		t.Errorf("stdout = %q", out)
	}
}

func TestVersion(t *testing.T) {
	out, _, code := run(t, "version")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(out) != "rein v0.0.0-test" {
		t.Fatalf("version = %q, want %q", strings.TrimSpace(out), "rein v0.0.0-test")
	}
	// --version must agree with the subcommand.
	flagOut, _, code := run(t, "--version")
	if code != 0 {
		t.Fatalf("--version exit = %d, want 0", code)
	}
	if strings.TrimSpace(flagOut) != strings.TrimSpace(out) {
		t.Errorf("--version = %q but `version` = %q", flagOut, out)
	}
}

func TestStatusWithNoConfig(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	out, errb, code := run(t, "status")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, errb)
	}
	if !strings.Contains(out, "not created yet") {
		t.Errorf("status did not say the config is missing:\n%s", out)
	}
	if !strings.Contains(out, "run `rein enrol`") {
		t.Errorf("status did not point at enrolment:\n%s", out)
	}
}

func TestStatusReportsQueuesAndTokenState(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	t.Setenv(keyring.EnvBackend, "file")

	cfg := config.Config{
		HostID:    "mac",
		Workspace: "elk-scout",
		WorkDir:   filepath.Join(home, "work"),
		Elk:       config.Elk{MCPURL: "https://example.test/elk-mcp"},
		Queues: []config.Queue{
			{Name: "mac-claude", AgentKind: "claude"},
			{Name: "mac-codex", AgentKind: "codex", Land: config.LandBranch},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	store := keyring.NewFileStore(filepath.Join(home, keyring.FileName))
	if err := store.Set("elk-scout", "mac-claude", "secret-token-value"); err != nil {
		t.Fatal(err)
	}

	out, errb, code := run(t, "status")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, errb)
	}
	for _, want := range []string{"mac-claude", "claude", "mac-codex", "codex", "elk-scout", "stored"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output is missing %q:\n%s", want, out)
		}
	}
	// Each queue's landing policy (ark:rein#47), the default spelled out
	// rather than left blank: "merge" is a policy, not an absence of one.
	for _, want := range []string{"LAND", "merge", "branch"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output is missing the landing policy %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "missing — run `rein enrol`") {
		t.Errorf("status did not flag the queue with no token:\n%s", out)
	}
	// status must never print a token.
	if strings.Contains(out, "secret-token-value") {
		t.Fatalf("status printed the token itself:\n%s", out)
	}
}

func TestStatusHonoursConfigFlag(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	path := filepath.Join(t.TempDir(), "elsewhere.toml")
	src := "host_id = \"win\"\nworkspace = \"pulse\"\n\n[[queues]]\nname = \"win-codex\"\nagent_kind = \"codex\"\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errb, code := run(t, "status", "--config", path)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, errb)
	}
	if !strings.Contains(out, "win-codex") {
		t.Errorf("status ignored --config:\n%s", out)
	}
}

func TestStatusRejectsBrokenConfig(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	path := filepath.Join(t.TempDir(), "broken.toml")
	// Over the 12-byte cap: a rejection, not a truncation.
	src := "[[queues]]\nname = \"mac-verylongname\"\nagent_kind = \"claude\"\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errb, code := run(t, "status", "--config", path)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb, "12-byte cap") {
		t.Errorf("error does not explain the problem: %q", errb)
	}
}

func TestUnknownCommandFails(t *testing.T) {
	_, _, code := run(t, "teleport")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func TestTreeIsStable(t *testing.T) {
	// The command tree is the v0 deliverable; a rename should break a test,
	// not a downstream lane.
	root := cli.NewRootCommand("v0")
	want := map[string]bool{
		"enrol": true, "run": true, "attach": true, "tail": true,
		"status": true, "version": true, "service": true,
	}
	got := map[string]bool{}
	for _, c := range root.Commands() {
		got[c.Name()] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("command %q is missing from the tree", name)
		}
	}
}

// `rein tail` reads the run logs under the Rein home and talks to nothing
// else — no config, no keychain, no Elk. That is what makes it safe to point
// at a machine whose service is mid-run.
func TestTailReplaysARecordedRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	store, err := runlog.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	w := store.Create(runlog.Meta{RunID: "abc12345", Queue: "mac-claude", Direction: "Port the sheet"})
	w.Runner(runlog.KindClaimed, "claimed on queue mac-claude")
	w.Event(adapter.Event{Kind: adapter.EventText, Text: "on it"})
	w.Close(runlog.StatusSubmitted)

	out, errOut, code := run(t, "tail", "abc12345", "--interval", "1ms")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, errOut)
	}
	for _, want := range []string{"claimed on queue mac-claude", "on it", "finished submitted"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// An unknown run is an error a person can act on: it says where the logs are
// rather than only that it could not find one.
func TestTailUnknownRun(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	_, errOut, code := run(t, "tail", "nope", "--interval", "1ms")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "no such run") || !strings.Contains(errOut, "Run logs live in") {
		t.Errorf("stderr = %q", errOut)
	}
}
