// Package gemini drives Gemini CLI (`gemini`) for a gemini queue.
//
// # The person's own Gemini CLI, on their own key
//
// Casey's rulings, 2026-11-11 (Elk #1755): Rein starts the Gemini CLI already
// installed on the person's computer, set up as it already is, the way it
// starts Codex and Claude Code. Elk installs nothing and adds no key step. And
// Rein drives it only on the person's own Google AI Studio or Google Cloud
// key, which Google allows; it never drives a personal Google sign-in, in any
// form. Antigravity is never started: it is a place a person adds Elk's
// connector, not a worker.
//
// So a run uses the person's own key setup: their settings' choice of the key
// sign-in (`gemini-api-key`) or Vertex AI (`vertex-ai`), or their own
// `.env`, under a private home (home.go) that carries nothing else of theirs.
// A choice of a Google sign-in is never carried into a run, and the files that
// hold one are never linked.
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
// A Gemini CLI with no key setup is not run: Preflight and Start refuse it as
// [NotSetUpLine], which reaches Elk as agent_login, the way an expired Claude
// Code sign-in does. A session that nevertheless asks for a Google sign-in is
// stopped at once and fails the same way: nobody is there to finish it, and
// Rein never would.
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

// KeyName is the variable Gemini CLI reads a Google key from, and the
// optional stored secret's name in Rein's store.
const KeyName = "GEMINI_API_KEY"

// KeyService is the keychain service a stored key lives under — the Elk app's
// managed secrets, runner.ManagedSecretService, which this package cannot
// import. A runner test holds the two equal.
const KeyService = "elk-secret"

// GeminiCLIName is what a person reads for Gemini CLI.
const GeminiCLIName = "Gemini CLI"

// NotSetUpLine is the one line a person reads when Gemini CLI is not
// installed, or not set up on a key, on this computer.
const NotSetUpLine = "Set up Gemini CLI on this computer with your own Google AI Studio or Google Cloud key first."

func init() { adapter.MustRegister(New()) }

// Adapter drives Gemini CLI. Safe for concurrent use.
type Adapter struct {
	binary string

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

// Manifest implements [adapter.Adapter].
func (a *Adapter) Manifest() adapter.Manifest { return geminiCLIManifest() }

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
		Notes: "Runs on the person's own Gemini CLI, on their own Google AI Studio or Google " +
			"Cloud key, never a personal Google sign-in (Casey, 2026-11-11). approvals is `no`: headless Gemini CLI cannot ask, so the usable " +
			"modes are read_only (`--approval-mode plan`, measured: write_file denied) and " +
			"full (`--approval-mode yolo`, measured: write_file and run_shell_command ran). " +
			"resume is `no`: each run has its own private Gemini CLI home, which holds the " +
			"person's key setup and the run's MCP servers, and Gemini CLI keeps " +
			"its sessions there. mcp is `yes` for allow-listed servers; the Elk run " +
			"connector is not offered, as for Codex and Grok, because Rein has not verified " +
			"Gemini CLI's excludeTools against its proxy. worktree is `partial`: in full " +
			"mode the agent can reach outside the worktree.",
	}
}

// ---------------------------------------------------------------- errors ---

func notInstalled() error {
	return fmt.Errorf("%w: Gemini CLI (`gemini`) is not installed on this computer. %s (%w)",
		adapter.ErrPreflight, NotSetUpLine, exec.ErrNotFound)
}

// ------------------------------------------------------------- preflight ---

// Preflight implements [adapter.Adapter]: Gemini CLI is installed, answers,
// and is set up on a key (keyAuth). "Set up" is presence-only: Gemini CLI has
// no command that checks a key without sending a prompt, so a key Google
// refuses surfaces as the run's own error.
func (a *Adapter) Preflight(ctx context.Context) error {
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
	if _, err := keyAuth(a.realGeminiDir(), a.storedKey() != ""); err != nil {
		return err
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
	if err := adapter.CheckSupport(geminiCLIManifest(), spec); err != nil {
		return nil, err
	}
	// Hosted runs are Claude Code or Codex only, and no queue's environment
	// may carry a key of any vendor's, this one's included. A stored key
	// arrives in ProviderKey, which is not Env.
	if err := spec.CheckPlanEnv(); err != nil {
		return nil, err
	}
	if len(spec.AllowedTools) > 0 || len(spec.DeniedTools) > 0 {
		return nil, fmt.Errorf("%w: Rein drives no per-run tool list for Gemini CLI, so a run that narrows its tools cannot be honoured",
			adapter.ErrNotSupported)
	}
	// Optional: with none, Gemini CLI runs on its own key setup.
	key := strings.TrimSpace(spec.ProviderKey)
	if len(key) < secretenv.MinValueLen {
		key = ""
	}
	if err := checkArgs(spec); err != nil {
		return nil, err
	}
	// A repository's own Gemini settings outrank the private home's.
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

// argPattern is what a model or effort may look like on a command line. On
// Windows, Gemini CLI is an npm shim run through cmd.exe, which does not
// escape special characters, and the model comes from Elk's settings.
var argPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)

func checkArgs(spec adapter.RunSpec) error {
	for name, v := range map[string]string{"model": spec.Model, "effort": spec.Effort} {
		if v != "" && (len(v) > 128 || !argPattern.MatchString(v)) {
			return fmt.Errorf("%w: the %s %q is not a name Gemini CLI takes", adapter.ErrNotSupported, name, v)
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

// ProgramVersion implements [adapter.ProgramVersioner]: Gemini CLI's version
// with its name, "0.64.0 (Gemini CLI)".
func (a *Adapter) ProgramVersion(ctx context.Context) string {
	v := adapter.ReadProgramVersion(ctx, mustPath(a.resolveBinary()))
	if v == "" {
		return ""
	}
	return v + " (" + GeminiCLIName + ")"
}

func mustPath(p string, err error) string {
	if err != nil {
		return ""
	}
	return p
}
