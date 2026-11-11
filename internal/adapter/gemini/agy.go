package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// Antigravity's command-line program (`agy`), on the person's own key.
//
// Driven only while Elk's server has the switch on (switch.go). Measured
// against agy 1.5.5 on darwin/arm64, 2026-11-10, against a local stand-in for
// the Gemini API that logged which credential arrived; no real key was used.
//
// # The key, never a Google sign-in
//
// agy uses a key only when its settings say `"modelProvider": "gemini"`
// (antigravity.google/docs/install: "Only setting a GEMINI_API_KEY environment
// variable on its own has no effect"). Rein passes that in `--settings`, which
// agy applies to the one session and never saves, so the person's own `agy`
// is never moved onto their key. Measured: with it, the model call carries the
// key in `x-goog-api-key` and no `Authorization` header; with it and no key,
// agy refuses to start ("modelProvider is set to \"gemini\" … but the
// GEMINI_API_KEY environment variable is not set") rather than signing in.
//
// A saved Google sign-in lives in the operating system's keyring, not in a
// folder Rein could hide, so the session settings are the guard, not a private
// home. The child never inherits a variable that could move it onto a Google
// sign-in, Google Cloud billing or another host (agyStripped), and a session
// whose output says it is starting a sign-in is killed.
//
// # Edits stay in the task's folder, by Antigravity's own rules
//
// Not by turning its checks off: `--dangerously-skip-permissions` is never
// passed. A full run is `--mode accept-edits --sandbox` with
// `toolPermission: proceed-in-sandbox` and allow rules for the task folder and
// the parts of git's own folder a commit writes (agyGrants). Measured:
//
//   - write_file inside the folder: done. Outside it: refused — headless agy
//     cannot ask, so it denies, and the turn ends (see deniedActions).
//   - A command writing inside the folder: done. Outside it, or into git's
//     hooks: "operation not permitted", from the terminal sandbox.
//   - git commit in a linked worktree: done, with the grants below; writing
//     git's hooks folder: refused.
//   - The network: closed in the sandbox until `read_url(*)` is granted;
//     granted, so a run can push and fetch packages.
//
// Settings given with `--settings` replace the same keys in the person's own
// settings file (measured: a user file allowing writes elsewhere and
// `always-proceed` did not widen a run), and a repository's checked-in
// settings did not either.
//
// A read-only run is the default mode with the same sandbox and no write
// grants: every write and edit is refused, commands still read.
// (`--mode plan` has no effect alongside `--disable-slash-commands`.)

// AgyBinary is Antigravity's command-line program.
const AgyBinary = "agy"

// AgyName is what a person reads for it.
const AgyName = "Antigravity"

// AgyInstallCommand is Google's installer for a Mac or Linux
// (antigravity.google/docs/getting-started). It puts one program at
// ~/.local/bin/agy and needs no developer tools.
const AgyInstallCommand = "curl -fsSL https://antigravity.google/cli/install.sh | bash"

// AgyInstallCommandWindows is Google's installer for Windows, in PowerShell;
// it puts agy at %LOCALAPPDATA%\agy\bin.
const AgyInstallCommandWindows = "irm https://antigravity.google/cli/install.ps1 | iex"

func agyNotInstalled() error {
	how := "Install it with: " + AgyInstallCommand
	if runtime.GOOS == "windows" {
		how = "Install it in PowerShell with: " + AgyInstallCommandWindows
	}
	return fmt.Errorf("%w: Antigravity's command-line program (`agy`) is not installed on this computer. %s (%w)",
		adapter.ErrPreflight, how, exec.ErrNotFound)
}

func (a *Adapter) resolveAgy() (string, error) {
	bin := a.agyBinary
	if bin == "" {
		bin = AgyBinary
	}
	if path, err := exec.LookPath(bin); err == nil {
		return path, nil
	}
	// Google's installer puts it in ~/.local/bin on a Mac or Linux and in
	// %LOCALAPPDATA%\agy\bin on Windows (antigravity.google/docs/getting-started),
	// which a service started outside a login shell may not have on PATH.
	if bin == AgyBinary {
		if p := agyInstalledPath(); p != "" {
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				return p, nil
			}
		}
	}
	return "", agyNotInstalled()
}

// agyMinVersion is the oldest Antigravity whose session settings, permission
// rules and sandbox Rein has measured. An older one is not driven: Gemini CLI
// runs instead.
var agyMinVersion = [3]int{1, 5, 5}

var versionPattern = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// agyVersions remembers each program file's answer, by path, size and time,
// so the manifest does not run it on every call.
var agyVersions struct {
	sync.Mutex
	seen map[string]bool
}

func agyVersionOK(bin string) bool {
	info, err := os.Stat(bin)
	if err != nil {
		return false
	}
	key := fmt.Sprintf("%s|%d|%d", bin, info.Size(), info.ModTime().UnixNano())
	agyVersions.Lock()
	ok, seen := agyVersions.seen[key]
	agyVersions.Unlock()
	if seen {
		return ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ok = versionAtLeast(adapter.ReadProgramVersion(ctx, bin), agyMinVersion)
	agyVersions.Lock()
	if agyVersions.seen == nil {
		agyVersions.seen = map[string]bool{}
	}
	agyVersions.seen[key] = ok
	agyVersions.Unlock()
	return ok
}

// versionAtLeast reads the first x.y.z in v; one it cannot read is too old.
func versionAtLeast(v string, min [3]int) bool {
	m := versionPattern.FindStringSubmatch(v)
	if m == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		n, _ := strconv.Atoi(m[i+1])
		if n != min[i] {
			return n > min[i]
		}
	}
	return true
}

// agyInstalledPath is where Google's installer puts agy on this platform.
func agyInstalledPath() string {
	if runtime.GOOS == "windows" {
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return ""
			}
			local = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(local, "agy", "bin", AgyBinary+".exe")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "bin", AgyBinary)
}

// agyStripped are variables an Antigravity session never inherits, whatever a
// queue says. AGY_ADC_AUTH and AGY_ACCOUNT pick a Google sign-in or Google
// Cloud billing; AGY_LLM_GATEWAY_* and GOOGLE_GEMINI_BASE_URL send the key to
// another host; ANTIGRAVITY_* and JETSKI_* are its internal switches (the
// names are from the agy 1.5.5 binary).
var agyStripped = []string{"AGY_", "ANTIGRAVITY_", "JETSKI_", "GEMINI_", "GOOGLE_GEMINI_", "GOOGLE_GENAI_",
	"GOOGLE_CLOUD_", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "CLOUD_SHELL"}

func agyStrippedName(name string) bool {
	up := strings.ToUpper(name) // Windows variable names ignore case
	for _, p := range agyStripped {
		if strings.HasPrefix(up, p) {
			return true
		}
	}
	return false
}

// agyEnv is the session's environment: base (the scoped inheritance) and the
// spec's own variables, less agyStripped, then the key. baseURL is a test
// stand-in's address and empty everywhere else.
func agyEnv(base []string, over map[string]string, key, baseURL string) []string {
	out := make([]string, 0, len(base)+len(over)+2)
	for _, kv := range base {
		i := strings.IndexByte(kv, '=')
		if i <= 0 || agyStrippedName(kv[:i]) {
			continue
		}
		if _, shadowed := over[kv[:i]]; shadowed {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range over {
		if !agyStrippedName(k) {
			out = append(out, k+"="+v)
		}
	}
	out = append(out, KeyName+"="+key)
	if baseURL != "" {
		out = append(out, "GOOGLE_GEMINI_BASE_URL="+baseURL)
	}
	return out
}

// rulePath is a path as agy's permission rules take it: forward slashes, as
// its documentation writes them. How agy matches a Windows path against a
// rule, drive letter included, has not been measured (docs/adapter-gemini.md).
func rulePath(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

// agyGrants are the permission rules for a run: what it may write, what it
// may never write, and what it may read.
//
// A full run may write the task's folder and the parts of git's own folder a
// commit writes. A Rein worktree is a linked worktree, so its git folder is
// outside it: `.git` is a file naming <common>/worktrees/<id>, and a commit
// writes there, into the shared object store, refs and logs.
//
// What would let a run plant code that runs outside the sandbox, the next time
// git runs there (Rein removing the worktree, a person looking at it), is
// never writable: the hooks and config of the shared folder are not granted,
// and the files that point git at a folder — the worktree's `.git` file, and
// the private folder's commondir, gitdir and config.worktree — are denied by
// rule. A deny rule beats an allow rule, for agy's own file tool and for
// commands in its sandbox alike (measured 2026-11-10: both refused, and a
// commit still worked).
//
// The sandbox sees real paths (/private/var for /var on a Mac), so every path
// is given under both names when they differ.
func agyGrants(worktree string, write bool) (allow, deny []string, err error) {
	rule := func(list *[]string, verb, path string) {
		*list = append(*list, verb+"("+rulePath(path)+")")
		if real, err := filepath.EvalSymlinks(path); err == nil && rulePath(real) != rulePath(path) {
			*list = append(*list, verb+"("+rulePath(real)+")")
		} else if err != nil {
			// Not there yet (a lock file): resolve its folder instead.
			if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
				if r := filepath.Join(dir, filepath.Base(path)); rulePath(r) != rulePath(path) {
					*list = append(*list, verb+"("+rulePath(r)+")")
				}
			}
		}
	}
	private, common, err := gitDirs(worktree)
	if err != nil {
		return nil, nil, err
	}
	if write {
		rule(&allow, "write_file", worktree)
		if common != "" {
			if private != common {
				rule(&allow, "write_file", private)
			}
			// packed-refs and its lock: a commit's ref update takes the lock
			// (measured: without them git commits but prints an error).
			for _, sub := range []string{"objects", "refs", "logs", "packed-refs", "packed-refs.lock"} {
				rule(&allow, "write_file", filepath.Join(common, sub))
			}
		}
	}
	if common != "" {
		rule(&allow, "read_file", common)
		if private != common {
			rule(&deny, "write_file", filepath.Join(worktree, ".git"))
			for _, f := range []string{"commondir", "gitdir", "config.worktree"} {
				rule(&deny, "write_file", filepath.Join(private, f))
			}
		} else {
			// A plain repository's own git folder sits inside the task folder.
			for _, f := range []string{"config", "config.worktree", "hooks"} {
				rule(&deny, "write_file", filepath.Join(common, f))
			}
		}
	}
	// The terminal sandbox has no network until a host is granted. Granted, so
	// a run can push, fetch and install packages, as every other agent can.
	allow = append(allow, "read_url(*)")
	return allow, deny, nil
}

// gitDirs reads where a worktree's git folders are without running git:
// private is the worktree's own (HEAD, index), common is the shared one.
// Both are "" outside a repository.
func gitDirs(worktree string) (private, common string, err error) {
	dotgit := filepath.Join(worktree, ".git")
	info, err := os.Stat(dotgit)
	switch {
	case os.IsNotExist(err):
		return "", "", nil
	case err != nil:
		return "", "", fmt.Errorf("gemini: reading the task folder's git folder: %w", err)
	case info.IsDir():
		return dotgit, dotgit, nil
	}
	raw, err := os.ReadFile(dotgit)
	if err != nil {
		return "", "", fmt.Errorf("gemini: reading the task folder's .git file: %w", err)
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return "", "", fmt.Errorf("gemini: the task folder's .git file does not name a git folder")
	}
	private = strings.TrimSpace(dir)
	if !filepath.IsAbs(private) {
		private = filepath.Join(worktree, private)
	}
	common = private
	if c, err := os.ReadFile(filepath.Join(private, "commondir")); err == nil {
		common = strings.TrimSpace(string(c))
		if !filepath.IsAbs(common) {
			common = filepath.Join(private, common)
		}
	}
	return filepath.Clean(private), filepath.Clean(common), nil
}

// agyPermissions are the session's permission settings. A full run is
// accept-edits with sandboxed commands; a read-only run is the default mode
// (every edit asks, and headless asking is a refusal) with the same sandbox
// and no write grants. Either way the person's own MCP servers and plugins
// load — `--settings` has no key for them — so their tools are denied by
// rule: a run must not act through the person's own connections, the Elk one
// among them (ark:rein#21).
func agyPermissions(spec adapter.RunSpec) (mode string, perms map[string]any, err error) {
	var write bool
	switch spec.PermissionMode {
	case adapter.PermissionFull:
		mode, write = "accept-edits", true
	case adapter.PermissionReadOnly:
		mode = ""
	default:
		return "", nil, fmt.Errorf("%w: permission mode %q has no Antigravity mapping: headless agy cannot ask, so only read_only and full can be honoured",
			adapter.ErrNotSupported, string(spec.PermissionMode))
	}
	allow, deny, err := agyGrants(spec.WorktreeDir, write)
	if err != nil {
		return "", nil, err
	}
	return mode, map[string]any{"allow": allow, "deny": append([]string{"mcp(*)"}, deny...), "ask": []string{}}, nil
}

// agySettings is the `--settings` JSON. agy refuses a key it does not know
// (measured: `unknown setting "mcpServers"`), so only documented keys go in.
func agySettings(perms map[string]any) (string, error) {
	blob, err := json.Marshal(map[string]any{
		"modelProvider":  "gemini",
		"toolPermission": "proceed-in-sandbox",
		"permissions":    perms,
	})
	if err != nil {
		return "", fmt.Errorf("gemini: encoding Antigravity's session settings: %w", err)
	}
	return string(blob), nil
}

// buildAgyArgs is the command line. The prompt goes on stdin as one
// stream-json message, and the key is in the environment; neither is here.
func buildAgyArgs(spec adapter.RunSpec) ([]string, error) {
	mode, perms, err := agyPermissions(spec)
	if err != nil {
		return nil, err
	}
	settings, err := agySettings(perms)
	if err != nil {
		return nil, err
	}
	// `--print=` with an empty value: the stdin message is the prompt, and a
	// bare `-p` would take the next flag as one.
	args := []string{"--print=", "--input-format=stream-json", "--output-format=stream-json",
		// A prompt starting with "/" must not run one of the person's skills.
		"--disable-slash-commands",
		"--sandbox",
		"--settings", settings}
	if mode != "" {
		args = append(args, "--mode", mode)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Effort != "" {
		args = append(args, "--effort", spec.Effort)
	}
	if id, ok := strings.CutPrefix(spec.ResumeID, agyResumePrefix); ok && id != "" {
		args = append(args, "--conversation", id)
	}
	return args, nil
}

// agyResumePrefix marks a session id as Antigravity's conversation id. Elk
// hands a run's session id back for a follow-up round, and the program can
// change between rounds when the switch flips; the mark keeps one program's id
// from ever reaching the other (Start).
const agyResumePrefix = "agy:"

// agyStdin is the one stream-json user message agy reads. Its field is
// "event", not "type": agy refuses the latter.
func agyStdin(spec adapter.RunSpec) ([]byte, error) {
	text := spec.Prompt
	if text == "" {
		text = "Continue the work already in progress in this conversation."
	}
	if spec.SystemPrompt != "" {
		// agy has no flag for standing instructions, so they go first.
		text = spec.SystemPrompt + "\n\n---\n\n" + text
	}
	blob, err := json.Marshal(map[string]any{
		"event":   "user",
		"message": map[string]any{"role": "user", "content": text},
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: encoding the prompt: %w", err)
	}
	return append(blob, '\n'), nil
}

// agyManifest is the adapter's manifest while Antigravity is switched on.
func agyManifest() adapter.Manifest {
	return adapter.Manifest{
		Kind:   Kind,
		Binary: AgyBinary,
		Capabilities: map[adapter.Capability]adapter.Support{
			adapter.CapGit:              adapter.SupportYes,
			adapter.CapWorktree:         adapter.SupportYes,
			adapter.CapFileEdit:         adapter.SupportYes,
			adapter.CapShell:            adapter.SupportYes,
			adapter.CapMCP:              adapter.SupportNo,
			adapter.CapResume:           adapter.SupportYes,
			adapter.CapStructuredEvents: adapter.SupportYes,
			adapter.CapApprovals:        adapter.SupportNo,
			adapter.CapSteer:            adapter.SupportNo,
		},
		Platforms: []adapter.Platform{adapter.AnyArch("darwin"), adapter.AnyArch("linux"), adapter.AnyArch("windows")},
		Notes: "Antigravity's command-line program on the person's own key, never a Google sign-in; " +
			"driven only while Elk's server has it switched on. worktree is `yes`: edits and commands " +
			"stay in the task's folder by Antigravity's own rules (accept-edits, the terminal sandbox, " +
			"allow rules for the folder and git's object store), measured. mcp is `no`: agy takes no " +
			"server from Rein, and the person's own servers' tools are denied by rule. approvals is " +
			"`no`: headless agy refuses what it would ask. resume is `yes`, by conversation id.",
	}
}

// startAgy starts one Antigravity session.
//
// notes are progress lines for the run's log (Start's account of anything it
// left out).
func (a *Adapter) startAgy(spec adapter.RunSpec, key string, notes []string) (adapter.Session, error) {
	if why := sandboxUnsupported(); why != "" {
		return nil, fmt.Errorf("%w: %s", adapter.ErrPreflight, why)
	}
	args, err := buildAgyArgs(spec)
	if err != nil {
		return nil, err
	}
	stdin, err := agyStdin(spec)
	if err != nil {
		return nil, err
	}
	bin, err := a.resolveAgy()
	if err != nil {
		return nil, err
	}
	env := agyEnv(spec.InheritedEnv(), spec.Env, key, a.agyBaseURL)
	return a.launch(spec, launch{
		program: AgyName, bin: bin, args: args, env: env, stdin: stdin, key: key,
		decode: decodeAgy, signIn: agySignInMarkers, fold: true, notes: notes,
	})
}

// agySignInMarkers are agy's words when it is about to start a Google
// sign-in. The session settings make that unreachable; seeing one anyway
// stops the session.
// Generic phrases ("Opening browser", a Google sign-in address) are left out:
// Antigravity has a browser tool, and a task may be about sign-in pages.
var agySignInMarkers = []string{"authentication required", "Sign in with Google", "paste the displayed code"}

// preflightAgy checks the program answers and the key is stored. Like Gemini
// CLI's, it cannot tell a good key from a revoked one without spending a
// task.
func (a *Adapter) preflightAgy() error {
	if why := sandboxUnsupported(); why != "" {
		return fmt.Errorf("%w: %s", adapter.ErrPreflight, why)
	}
	if _, err := a.resolveAgy(); err != nil {
		return err
	}
	_, err := a.readKey()
	return err
}
