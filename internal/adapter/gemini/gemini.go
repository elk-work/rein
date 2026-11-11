// Package gemini drives Google's Gemini CLI (`gemini`) on the person's own
// Gemini API key, through `gemini -p` with stream-json output.
//
// # Why a key, and why only here
//
// Every other adapter runs on the developer's own plan login and refuses a
// metered API key outright (adapter.RunSpec.CheckPlanEnv, internal/secretenv).
// Gemini is the one exception, by Casey's ruling of 2026-11-04: the person
// brings their own Google key, it stays on their computer, Google bills them,
// and Elk never sees it. There is no plan-login path to honour instead:
// Google's terms do not allow another program to drive its agent on a
// personal Google sign-in. So this adapter never uses a Google sign-in, and
// is built so that it cannot (home.go): every session runs under a private
// Gemini CLI home whose settings enforce the API-key sign-in, and Gemini CLI
// then refuses to start without GEMINI_API_KEY rather than opening a Google
// sign-in ("When using Gemini API, you must specify the GEMINI_API_KEY
// environment variable", measured against Gemini CLI 0.64, 2026-11-04).
//
// # Why Gemini CLI
//
// It is the program Elk's installer sets up for Gemini (scout
// scripts/connect-agent.mjs), and Google still documents it on a Google AI
// Studio key for headless use (geminicli.com/docs/get-started/authentication:
// "you can use an API key from Google AI Studio … Set the GEMINI_API_KEY
// environment variable"). Google moved free and Google One accounts to
// Antigravity CLI on 2026-06-18 and says Gemini 4 models need Antigravity, so
// a free-tier key or a Gemini 4 model may need an Antigravity CLI adapter
// later; docs/adapter-gemini.md has the detail.
//
// # The key
//
// The key is GEMINI_API_KEY in Rein's own secret store — `rein secret set
// GEMINI_API_KEY`, the keychain on a Mac. The run loop reads it for a gemini
// queue alone and hands it over in [adapter.RunSpec.ProviderKey]; this adapter
// sets it on its own child process and nowhere else. GEMINI_API_KEY stays a
// metered name in internal/secretenv, so no other adapter, no task process and
// no queue's environment can carry one.
package gemini

import (
	"context"
	"errors"
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

// KeyName is the variable Gemini CLI reads the Gemini API key from, and the
// secret's name in Rein's store.
const KeyName = "GEMINI_API_KEY"

// KeyService is the keychain service the key lives under — the Elk app's
// managed secrets, runner.ManagedSecretService, which this package cannot
// import. A runner test holds the two equal.
const KeyService = "elk-secret"

// InstallCommand is Google's own install command for Gemini CLI on a Mac
// (github.com/google-gemini/gemini-cli). Both of Google's Mac routes need a
// developer tool already on the machine — Homebrew, or Node.js for
// `npm install -g @google/gemini-cli` — and Google publishes no installer that
// needs neither.
const InstallCommand = "brew install gemini-cli"

// InstallCommandWindows is Google's other route, and the only one on Windows;
// it needs Node.js.
const InstallCommandWindows = "npm install -g @google/gemini-cli"

// MissingKeyLine is what a person reads when no key is stored.
const MissingKeyLine = "Add your Google key in Elk to use Gemini."

func init() { adapter.MustRegister(New()) }

// Adapter drives Gemini CLI. Safe for concurrent use.
type Adapter struct {
	binary string

	// agyBinary is Antigravity's program, for tests; empty is [AgyBinary].
	agyBinary string
	// agyBaseURL points Antigravity at a test stand-in for the Gemini API. It
	// is set only by tests: in a real run GOOGLE_GEMINI_BASE_URL is stripped,
	// since it would send the key to another host.
	agyBaseURL string

	// Items reads the key for Preflight. Nil opens this machine's store the
	// way the run loop does (keyring.OpenItems, REIN_KEYRING).
	Items keyring.ItemReader

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

// Manifest implements [adapter.Adapter]: Antigravity's while Elk's server has
// it switched on (switch.go), Gemini CLI's otherwise.
func (a *Adapter) Manifest() adapter.Manifest {
	if a.useAgy() {
		return agyManifest()
	}
	return geminiCLIManifest()
}

// useAgy reports whether a new run should be Antigravity: the switch is on,
// this computer can run its sandbox, and a version Rein measured is
// installed. Otherwise Gemini CLI runs, as before. Off, agy is never looked
// for. Start asks once and holds the answer for the whole run.
func (a *Adapter) useAgy() bool {
	// Linux is left out until its sandbox is measured: there the deny rules
	// that keep git's pointer files safe may not hold.
	if !AntigravityOn() || (runtime.GOOS == "linux" && !agyLinux) || sandboxUnsupported() != "" {
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
		Notes: "Runs on the person's own Gemini API key, never a Google sign-in (Casey, " +
			"2026-11-04). approvals is `no`: headless Gemini CLI cannot ask, so the usable " +
			"modes are read_only (`--approval-mode plan`, measured: write_file denied) and " +
			"full (`--approval-mode yolo`, measured: write_file and run_shell_command ran). " +
			"resume is `no`: each run has its own private Gemini CLI home, which holds the " +
			"settings that enforce the key and the run's MCP servers, and Gemini CLI keeps " +
			"its sessions there. mcp is `yes` for allow-listed servers; the Elk run " +
			"connector is not offered, as for Codex and Grok, because Rein has not verified " +
			"Gemini CLI's excludeTools against its proxy. worktree is `partial`: in full " +
			"mode the agent can reach outside the worktree.",
	}
}

// ---------------------------------------------------------------- errors ---

// keyError is the missing-key refusal: a preflight failure whose message is
// the one plain line a person reads.
type keyError struct{}

func (keyError) Error() string { return MissingKeyLine }

// Is makes [errors.Is] match both [adapter.ErrNoProviderKey] and
// [adapter.ErrPreflight].
func (keyError) Is(target error) bool {
	return target == adapter.ErrNoProviderKey || target == adapter.ErrPreflight
}

// ErrNoKey is the error every missing-key path returns.
var ErrNoKey error = keyError{}

func notInstalled() error {
	how := InstallCommand
	if runtime.GOOS == "windows" {
		how = InstallCommandWindows
	}
	return fmt.Errorf("%w: Gemini CLI (`gemini`) is not installed on this computer. Install it with: %s (%w)",
		adapter.ErrPreflight, how, exec.ErrNotFound)
}

// ------------------------------------------------------------- preflight ---

// Preflight implements [adapter.Adapter]: the program is installed and
// answers, and the key is stored.
//
// That is the whole sign-in probe, and it is presence-only. Gemini CLI has no
// command that checks a key without sending a prompt — `gemini --version`
// reads nothing, `gemini mcp list` reads only settings — so nothing can tell a
// good key from a revoked one without spending a task, and Rein does not call
// Google's API itself with the person's key. A bad key surfaces as the run's
// own error, in Google's words ("API key not valid").
func (a *Adapter) Preflight(ctx context.Context) error {
	if a.useAgy() {
		return a.preflightAgy()
	}
	bin, err := a.resolveBinary()
	if err != nil {
		return err
	}
	if _, err := a.readKey(); err != nil {
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
	return nil
}

// readKey reads the stored key for Preflight. A value too short to redact is
// treated as missing, as a scoped secret is.
func (a *Adapter) readKey() (string, error) {
	items := a.Items
	if items == nil {
		dir, err := config.Dir()
		if err != nil {
			return "", fmt.Errorf("%w: %v", adapter.ErrPreflight, err)
		}
		if items, err = keyring.OpenItems(dir); err != nil {
			return "", fmt.Errorf("%w: %v", adapter.ErrPreflight, err)
		}
	}
	v, err := items.ReadItem(KeyService, KeyName)
	if errors.Is(err, keyring.ErrItemNotFound) {
		return "", ErrNoKey
	}
	if err != nil {
		return "", fmt.Errorf("%w: the Google key could not be read from this computer's keychain: %v",
			adapter.ErrPreflight, err)
	}
	v = strings.TrimSpace(v)
	if len(v) < secretenv.MinValueLen {
		return "", ErrNoKey
	}
	return v, nil
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
	// may carry a key of any vendor's, this one's included. The key arrives
	// in ProviderKey, which is not Env.
	if err := spec.CheckPlanEnv(); err != nil {
		return nil, err
	}
	if len(spec.AllowedTools) > 0 || len(spec.DeniedTools) > 0 {
		return nil, fmt.Errorf("%w: Rein drives no per-run tool list for Google's programs, so a run that narrows its tools cannot be honoured",
			adapter.ErrNotSupported)
	}
	key := strings.TrimSpace(spec.ProviderKey)
	if len(key) < secretenv.MinValueLen {
		return nil, ErrNoKey
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
	// A repository's own Gemini settings can choose the sign-in, and the
	// workspace layer outranks the private home's (measured: a checked-in
	// `enforcedType: oauth-personal` opened Google's sign-in page). Refuse
	// before anything starts.
	if err := checkWorkspaceAuth(spec.WorktreeDir); err != nil {
		return nil, err
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
	h, err := a.newHome(spec)
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
		return fmt.Errorf("%w: this run's conversation is Antigravity's, and Antigravity is switched off or "+
			"unavailable on this computer now, so it cannot be continued", adapter.ErrNotSupported)
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
	name, bin := "Gemini CLI", ""
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
