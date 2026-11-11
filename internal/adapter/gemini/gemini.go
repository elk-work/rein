// Package gemini drives Google's two coding programs for a gemini queue:
// Antigravity's command-line program (`agy`, agy.go) where it is installed,
// and Gemini CLI (`gemini`) otherwise.
//
// # The person's own setup, like Codex and Claude Code
//
// Casey's ruling, 2026-11-11: the person already has Antigravity or Gemini CLI
// installed and set up, and Rein starts it with whatever sign-in or key it
// already has. Elk installs nothing, asks for no key and manages no sign-in,
// exactly as with Codex and Claude Code. He accepted, for the person's own
// account, the risk Google's Antigravity FAQ names for driving `agy` from
// another program on a consumer plan (antigravity.google/docs/faq).
//
// So neither program is moved onto a key or off a sign-in. Antigravity reads
// its own settings and saved Google sign-in; Gemini CLI runs under a private
// home (home.go) that carries only the person's choice of sign-in and the
// files that hold it, the way Codex's private home carries only its login.
//
// # A stored key, if there is one
//
// Someone whose program runs on a Google key keeps it in their own shell, which
// a Rein service never sees, and the daemon's environment never passes
// GEMINI_API_KEY down (internal/secretenv). So a key stored for Rein —
// `rein secret set GEMINI_API_KEY`, the keychain on a Mac — is handed to this
// adapter alone in [adapter.RunSpec.ProviderKey] and set on its own child
// process. It is optional and never asked for.
//
// # Not set up
//
// Headless, neither program can finish a Google sign-in: Antigravity prints a
// sign-in address and waits sixty seconds for a code (measured, agy 1.5.5,
// 2026-11-11). Rein does not wait for a sign-in nobody is there to finish. A
// session that says it needs one is stopped and fails as "set up Antigravity
// first", which reaches Elk as agent_login, the way an expired Claude Code
// sign-in does. Preflight asks Antigravity the same question up front.
package gemini

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/secretenv"
)

// Kind is the agent kind this adapter registers under.
const Kind = "gemini"

// Binary is the executable the adapter drives: Gemini CLI.
const Binary = "gemini"

// KeyName is the variable both programs read a Google key from, and the
// optional stored secret's name in Rein's store.
const KeyName = "GEMINI_API_KEY"

// KeyService is the keychain service a stored key lives under — the Elk app's
// managed secrets, runner.ManagedSecretService, which this package cannot
// import. A runner test holds the two equal.
const KeyService = "elk-secret"

// GeminiCLIName is what a person reads for Gemini CLI.
const GeminiCLIName = "Gemini CLI"

// NotSetUpLine is the one line a person reads when the program Rein would run
// is not installed or not signed in on this computer.
func NotSetUpLine(program string) string { return "Set up " + program + " on this computer first." }

func init() { adapter.MustRegister(New()) }

// Adapter drives Gemini CLI. Safe for concurrent use.
type Adapter struct {
	binary string

	// agyBinary is Antigravity's program, for tests; empty is [AgyBinary].
	agyBinary string
	// agyBaseURL points Antigravity at a test stand-in for the Gemini API. It
	// is set only by tests.
	agyBaseURL string
	// agyTestSettings are added to Antigravity's session settings, so a live
	// test can play a person whose own settings put agy on a key. Set only by
	// tests; Rein's own settings win over them.
	agyTestSettings map[string]any

	// Items reads a stored key for Preflight. Nil opens this machine's store
	// the way the run loop does (keyring.OpenItems, REIN_KEYRING).
	Items keyring.ItemReader

	// RealHome is the person's own Gemini CLI home, the folder holding
	// `.gemini`. Empty is $GEMINI_CLI_HOME, else the user's home folder. Set
	// by tests.
	RealHome string

	// Root is where the adapter keeps each run's private Gemini CLI home.
	// Empty is <Rein's config dir>/gemini.
	Root string

	// InterruptGrace is how long Interrupt waits after SIGINT before killing
	// the process group. Zero is [defaultInterruptGrace].
	InterruptGrace time.Duration
}

const defaultInterruptGrace = 10 * time.Second

// New returns the Gemini adapter.
func New() *Adapter { return &Adapter{binary: Binary} }

// Name implements [adapter.Adapter].
func (a *Adapter) Name() string { return Kind }

// Manifest implements [adapter.Adapter]: Antigravity's where it is installed,
// Gemini CLI's otherwise.
func (a *Adapter) Manifest() adapter.Manifest {
	if a.useAgy() {
		return agyManifest()
	}
	return geminiCLIManifest()
}

// useAgy reports whether a new run should be Antigravity: this computer can
// run its sandbox and a version Rein measured is installed. Otherwise Gemini
// CLI runs. Start asks once and holds the answer for the whole run.
func (a *Adapter) useAgy() bool {
	// Linux is left out until its sandbox is measured: there the deny rules
	// that keep git's pointer files safe may not hold.
	if (runtime.GOOS == "linux" && !agyLinux) || sandboxUnsupported() != "" {
		return false
	}
	bin, err := a.resolveAgy()
	return err == nil && agyVersionOK(bin)
}

// geminiCLIManifest is Gemini CLI's manifest.
//
// Checked against Gemini CLI 0.64 on darwin/arm64, 2026-11-04, against a local
// stand-in for the Gemini API (no real key exists for Rein's tests) and once
// against Google's own API with a key Google refused.
func geminiCLIManifest() adapter.Manifest {
	return adapter.Manifest{
		Kind:   Kind,
		Binary: Binary,
		Capabilities: map[adapter.Capability]adapter.Support{
			adapter.CapGit:              adapter.SupportYes,
			adapter.CapWorktree:         adapter.SupportPartial,
			adapter.CapFileEdit:         adapter.SupportYes,
			adapter.CapShell:            adapter.SupportYes,
			adapter.CapMCP:              adapter.SupportYes,
			adapter.CapResume:           adapter.SupportNo,
			adapter.CapStructuredEvents: adapter.SupportYes,
			adapter.CapApprovals:        adapter.SupportNo,
			adapter.CapSteer:            adapter.SupportNo,
		},
		// Windows since ark:rein#151: the same program from npm (gemini.cmd),
		// the same settings and environment; a run is killed by process tree.
		Platforms: []adapter.Platform{adapter.AnyArch("darwin"), adapter.AnyArch("linux"), adapter.AnyArch("windows")},
		Notes: "Runs on the person's own Gemini CLI setup, their sign-in or key (Casey, " +
			"2026-11-11). approvals is `no`: headless Gemini CLI cannot ask, so the usable " +
			"modes are read_only (`--approval-mode plan`, measured: write_file denied) and " +
			"full (`--approval-mode yolo`, measured: write_file and run_shell_command ran). " +
			"resume is `no`: each run has its own private Gemini CLI home, which holds the " +
			"person's choice of sign-in and the run's MCP servers, and Gemini CLI keeps " +
			"its sessions there. mcp is `yes` for allow-listed servers; the Elk run " +
			"connector is not offered, as for Codex and Grok, because Rein has not verified " +
			"Gemini CLI's excludeTools against its proxy. worktree is `partial`: in full " +
			"mode the agent can reach outside the worktree.",
	}
}

// ---------------------------------------------------------------- errors ---

func notInstalled() error {
	return fmt.Errorf("%w: Gemini CLI (`gemini`) is not installed on this computer. %s (%w)",
		adapter.ErrPreflight, NotSetUpLine(GeminiCLIName), exec.ErrNotFound)
}

// ------------------------------------------------------------- preflight ---

// Preflight implements [adapter.Adapter]: the program is installed, answers,
// and is set up.
//
// For Gemini CLI "set up" is presence-only: the person's settings choose a
// sign-in, or a key is stored. Gemini CLI has no command that checks a
// sign-in without sending a prompt, and its Google sign-in lives in the
// keychain, which Rein does not read; a sign-in that has lapsed surfaces as
// the run's own error. Antigravity is asked (preflightAgy).
func (a *Adapter) Preflight(ctx context.Context) error {
	if a.useAgy() {
		return a.preflightAgy(ctx)
	}
	bin, err := a.resolveBinary()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = secretenv.Filter(os.Environ())
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("%w: `gemini --version` failed (%v): %s", adapter.ErrPreflight, err, firstLine(text))
	}
	if text == "" {
		return fmt.Errorf("%w: `gemini --version` printed nothing", adapter.ErrPreflight)
	}
	if a.storedKey() == "" && personAuth(a.realGeminiDir()) == nil {
		return notSetUp(GeminiCLIName, "its settings choose no sign-in")
	}
	return nil
}

// storedKey reads a key stored for Rein, for Preflight. None, or one that
// cannot be read, is "": a key is optional.
func (a *Adapter) storedKey() string {
	items := a.Items
	if items == nil {
		dir, err := config.Dir()
		if err != nil {
			return ""
		}
		if items, err = keyring.OpenItems(dir); err != nil {
			return ""
		}
	}
	v, err := items.ReadItem(KeyService, KeyName)
	if err != nil {
		return ""
	}
	if v = strings.TrimSpace(v); len(v) < secretenv.MinValueLen {
		return ""
	}
	return v
}

func (a *Adapter) resolveBinary() (string, error) {
	bin := a.binary
	if bin == "" {
		bin = Binary
	}
	if path, err := exec.LookPath(bin); err == nil {
		return path, nil
	}
	// Homebrew's prefix, or npm's on Windows, for a service started outside
	// a login shell.
	if bin == Binary {
		for _, p := range fallbackGeminiPaths() {
			if info, err := os.Stat(p); err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0) {
				return p, nil
			}
		}
	}
	return "", notInstalled()
}

func fallbackGeminiPaths() []string {
	if runtime.GOOS != "windows" {
		return []string{"/opt/homebrew/bin/gemini", "/usr/local/bin/gemini"}
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		// npm install -g puts its shims in %APPDATA%\npm.
		return []string{filepath.Join(appData, "npm", "gemini.cmd")}
	}
	return nil
}

func (a *Adapter) root() (string, error) {
	if a.Root != "" {
		return a.Root, nil
	}
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, Kind), nil
}

// ----------------------------------------------------------------- start ---

// approvalMode maps Rein's mode onto Gemini CLI's. Only the two modes that
// decide up front are reachable: CheckSupport refuses ask and accept_edits
// because approvals is `no`.
func approvalMode(m adapter.PermissionMode) (string, error) {
	switch m {
	case adapter.PermissionReadOnly:
		// Gemini CLI's own read-only mode. Measured: a write_file to the
		// worktree was refused ("Access denied: plan path … must be within
		// the designated plans directory") and no file appeared.
		return "plan", nil
	case adapter.PermissionFull:
		return "yolo", nil
	default:
		return "", fmt.Errorf("%w: permission mode %q has no Gemini CLI mapping: headless Gemini CLI cannot ask, so only read_only and full can be honoured",
			adapter.ErrNotSupported, string(m))
	}
}

// buildArgs is the command line. The prompt is never on it — it goes on stdin,
// which `-p` appends to — and neither is the key.
func buildArgs(spec adapter.RunSpec) ([]string, error) {
	mode, err := approvalMode(spec.PermissionMode)
	if err != nil {
		return nil, err
	}
	args := []string{"--prompt", "", "--output-format", "stream-json", "--approval-mode", mode,
		// The documented headless switch for a folder Gemini CLI has not been
		// told to trust (geminicli.com/docs/cli/trusted-folders). Without it a
		// headless run exits 55 before it starts, and an untrusted folder
		// connects no MCP server (ark:rein#119). GEMINI_CLI_TRUST_WORKSPACE is
		// set as well (home.go).
		"--skip-trust"}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	return args, nil
}

// promptText is what goes on stdin. Gemini CLI has no flag for an appended
// system prompt, so the standing instructions go first, as the contract asks.
func promptText(spec adapter.RunSpec) string {
	if spec.SystemPrompt == "" {
		return spec.Prompt
	}
	return spec.SystemPrompt + "\n\n---\n\n" + spec.Prompt
}

// Start implements [adapter.Adapter].
func (a *Adapter) Start(ctx context.Context, spec adapter.RunSpec) (adapter.Session, error) {
	spec.AgentKind = Kind
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	// Which program runs is decided once, here, and everything below holds
	// to it: the switch can flip while a run is under way.
	agy := a.useAgy()
	m := geminiCLIManifest()
	if agy {
		m = agyManifest()
	}
	if err := checkResume(spec.ResumeID, agy); err != nil {
		return nil, err
	}
	if err := adapter.CheckSupport(m, spec); err != nil {
		return nil, err
	}
	// Hosted runs are Claude Code or Codex only, and no queue's environment
	// may carry a key of any vendor's, this one's included. A stored key
	// arrives in ProviderKey, which is not Env.
	if err := spec.CheckPlanEnv(); err != nil {
		return nil, err
	}
	if len(spec.AllowedTools) > 0 || len(spec.DeniedTools) > 0 {
		return nil, fmt.Errorf("%w: Rein drives no per-run tool list for Google's programs, so a run that narrows its tools cannot be honoured",
			adapter.ErrNotSupported)
	}
	// Optional: with none, the program runs on its own setup.
	key := strings.TrimSpace(spec.ProviderKey)
	if len(key) < secretenv.MinValueLen {
		key = ""
	}
	if err := checkArgs(spec); err != nil {
		return nil, err
	}
	if agy {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return a.startAgy(spec, key)
	}
	args, err := buildArgs(spec)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bin, err := a.resolveBinary()
	if err != nil {
		return nil, err
	}
	h, err := a.newHome(spec, key)
	if err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	s, err := a.startProcess(spec, bin, args, h, key)
	if err != nil {
		h.remove()
		return nil, err
	}
	return s, nil
}

// checkResume keeps one program's conversation id from reaching the other.
// Antigravity's ids carry agyResumePrefix; Gemini CLI cannot continue one at
// all. Either way a mismatch is a refusal, not a fresh start with only the
// follow-up's words for a prompt.
func checkResume(id string, agy bool) error {
	if id == "" {
		return nil
	}
	isAgy := strings.HasPrefix(id, agyResumePrefix)
	if isAgy && !argPattern.MatchString(strings.TrimPrefix(id, agyResumePrefix)) {
		return fmt.Errorf("%w: the conversation id %q is not one Antigravity gives", adapter.ErrNotSupported, id)
	}
	switch {
	case isAgy && !agy:
		return fmt.Errorf("%w: this run's conversation is Antigravity's, and Antigravity is no longer "+
			"available on this computer, so it cannot be continued", adapter.ErrNotSupported)
	case !isAgy && agy:
		return fmt.Errorf("%w: this run began on Gemini CLI, which cannot continue a conversation, and "+
			"Antigravity cannot continue one it did not start", adapter.ErrNotSupported)
	}
	return nil
}

// argPattern is what a model or effort may look like on a command line. On
// Windows, Gemini CLI is an npm shim run through cmd.exe, which does not
// escape special characters, and the model comes from Elk's settings.
var argPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)

func checkArgs(spec adapter.RunSpec) error {
	for name, v := range map[string]string{"model": spec.Model, "effort": spec.Effort} {
		if v != "" && (len(v) > 128 || !argPattern.MatchString(v)) {
			return fmt.Errorf("%w: the %s %q is not a name Google's programs take", adapter.ErrNotSupported, name, v)
		}
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// ProgramVersion implements [adapter.ProgramVersioner]: the version with the
// program's name, "1.5.5 (Antigravity)" or "0.64.0 (Gemini CLI)", which is how
// Elk learns which Google program this queue runs.
func (a *Adapter) ProgramVersion(ctx context.Context) string {
	name, bin := GeminiCLIName, ""
	if a.useAgy() {
		name, bin = AgyName, mustPath(a.resolveAgy())
	} else {
		bin = mustPath(a.resolveBinary())
	}
	v := adapter.ReadProgramVersion(ctx, bin)
	if v == "" {
		return ""
	}
	return v + " (" + name + ")"
}

func mustPath(p string, err error) string {
	if err != nil {
		return ""
	}
	return p
}
