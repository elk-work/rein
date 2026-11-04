// Package gemini drives Google's Antigravity CLI (`agy`) on the person's own
// Gemini API key, through `agy --print` with stream-json in and out.
//
// # Why a key, and why only here
//
// Every other adapter runs on the developer's own plan login and refuses a
// metered API key outright (adapter.RunSpec.CheckPlanEnv, internal/secretenv).
// Gemini is the one exception, by Casey's ruling of 2026-11-04: the person
// brings their own Google key, it stays on their computer, Google bills them,
// and Elk never sees it. There is no plan-login path to honour instead —
// Google's terms forbid driving its agent from another program on a personal
// Google sign-in (antigravity.google/docs/faq: wrapping `agy` in third-party
// applications "is not supported under consumer plans"; for a third-party
// coding agent Google recommends "a Gemini Enterprise or Google AI Studio API
// key"). So this adapter never uses a Google sign-in, and it is built so that
// it cannot: every session runs under a private home whose Antigravity
// settings say `"modelProvider": "gemini"`, and in that mode `agy` refuses to
// start without GEMINI_API_KEY rather than falling back to a sign-in
// (verified against agy 1.3.4, 2026-11-04; home.go).
//
// # Why Antigravity CLI and not Gemini CLI
//
// Google replaced Gemini CLI with Antigravity CLI on 2026-06-18 for
// individuals; Gemini CLI's own README now scopes it to Gemini Code Assist
// organisations "plus legacy paid API / Vertex keys" and calls it "not
// optimized for Gemini 4+". Antigravity CLI's authentication page documents
// a Google AI Studio key for exactly this use (antigravity.google/docs/install,
// "Gemini API key"). docs/adapter-gemini.md has the quotes and the probes.
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
	"strings"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/secretenv"
)

// Kind is the agent kind this adapter registers under.
const Kind = "gemini"

// Binary is the executable the adapter drives: Antigravity CLI.
const Binary = "agy"

// KeyName is the variable Antigravity CLI reads the Gemini API key from, and
// the secret's name in Rein's store. agy reads it from the environment only:
// not GOOGLE_API_KEY and never a .env file (antigravity.google/docs/install).
const KeyName = "GEMINI_API_KEY"

// KeyService is the keychain service the key lives under — the Elk app's
// managed secrets, runner.ManagedSecretService, which this package cannot
// import. A runner test holds the two equal.
const KeyService = "elk-secret"

// InstallCommand is Google's own installer for Antigravity CLI on a Mac or
// Linux (antigravity.google/docs/getting-started). It needs no developer
// tools: it downloads one binary to ~/.local/bin/agy.
const InstallCommand = "curl -fsSL https://antigravity.google/cli/install.sh | bash"

// MissingKeyLine is what a person reads when no key is stored.
const MissingKeyLine = "Add your Google key in Elk to use Gemini."

func init() { adapter.MustRegister(New()) }

// Adapter drives Antigravity CLI. Safe for concurrent use.
type Adapter struct {
	binary string

	// Items reads the key for Preflight. Nil opens this machine's store the
	// way the run loop does (keyring.OpenItems, REIN_KEYRING).
	Items keyring.ItemReader

	// Root is where the adapter keeps its private home and its run records.
	// Empty is <Rein's config dir>/gemini.
	Root string

	// RealHome is the person's own home, mirrored into the private one. Empty
	// is os.UserHomeDir.
	RealHome string

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
//
// Checked against agy 1.3.4 on darwin/arm64, 2026-11-04, against a local
// stand-in for the Gemini API (no real key exists for Rein's tests).
func (a *Adapter) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Kind:   Kind,
		Binary: Binary,
		Capabilities: map[adapter.Capability]adapter.Support{
			adapter.CapGit:              adapter.SupportYes,
			adapter.CapWorktree:         adapter.SupportPartial,
			adapter.CapFileEdit:         adapter.SupportYes,
			adapter.CapShell:            adapter.SupportYes,
			adapter.CapMCP:              adapter.SupportNo,
			adapter.CapResume:           adapter.SupportYes,
			adapter.CapStructuredEvents: adapter.SupportYes,
			adapter.CapApprovals:        adapter.SupportNo,
			adapter.CapSteer:            adapter.SupportNo,
		},
		// Windows is left out until the private home is built from
		// USERPROFILE and measured there.
		Platforms: []adapter.Platform{adapter.AnyArch("darwin"), adapter.AnyArch("linux")},
		Notes: "Runs on the person's own Gemini API key, never a Google sign-in (Casey, " +
			"2026-11-04). approvals is `no`: headless agy cannot prompt and auto-denies " +
			"any write or command it has not been told to allow, so the usable modes are " +
			"read_only (`--mode=plan`, measured: write_file and run_command both denied) and " +
			"full (`--dangerously-skip-permissions`). mcp is `no` for now: agy reads MCP " +
			"servers from its home's config, which Rein has not yet learned to write per run. " +
			"worktree is `partial`: in full mode agy can reach outside the worktree. steer " +
			"is `no`: the prompt is sent and stdin closed, one turn per process.",
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

// notInstalled is the missing-program refusal.
func notInstalled() error {
	return fmt.Errorf("%w: Antigravity CLI (`agy`) is not installed on this computer. Install it with: %s (%w)",
		adapter.ErrPreflight, InstallCommand, exec.ErrNotFound)
}

// ------------------------------------------------------------- preflight ---

// Preflight implements [adapter.Adapter]: the program is installed, the key is
// stored, and `agy models` starts in key mode under the private home with it.
//
// That last call is the sign-in probe, and it is presence-only. `agy models`
// prints a built-in list and makes no network call (measured: a stand-in API
// saw no request), so it proves the program runs and is on the key rather than
// a Google sign-in — with no key it prints "GEMINI_API_KEY environment
// variable is not set" — but it cannot tell a good key from a revoked one.
// Nothing can without spending a task; a bad key surfaces as the run's own
// error ("API key not valid").
func (a *Adapter) Preflight(ctx context.Context) error {
	bin, err := a.resolveBinary()
	if err != nil {
		return err
	}
	key, err := a.readKey()
	if err != nil {
		return err
	}
	h, err := a.prepareHome()
	if err != nil {
		return fmt.Errorf("%w: %v", adapter.ErrPreflight, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "models")
	cmd.Dir = h.dir
	cmd.Env = h.childEnv(secretenv.Filter(os.Environ()), nil, key)
	out, runErr := cmd.CombinedOutput()
	red := secretenv.NewRedactor(map[string]string{KeyName: key})
	text := strings.TrimSpace(red.String(string(out)))
	if runErr != nil {
		return fmt.Errorf("%w: `agy models` failed (%v): %s", adapter.ErrPreflight, runErr, firstLine(text))
	}
	if strings.Contains(text, KeyName) {
		// agy's own words when key mode is on and the key did not arrive.
		return fmt.Errorf("%w: Antigravity CLI did not receive the Google key: %s", adapter.ErrPreflight, firstLine(text))
	}
	if !listsModels(text) {
		return fmt.Errorf("%w: `agy models` did not list any models: %s", adapter.ErrPreflight, firstLine(text))
	}
	return nil
}

// listsModels reports whether `agy models` printed its model table: lines of
// `<id>\t<name>`.
func listsModels(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if id, _, ok := strings.Cut(line, "\t"); ok && strings.HasPrefix(strings.TrimSpace(id), "gemini") {
			return true
		}
	}
	return false
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
	// Google's installer puts it in ~/.local/bin, which a daemon started
	// outside a login shell may not have on PATH.
	if bin == Binary {
		if home, err := a.realHome(); err == nil {
			p := filepath.Join(home, ".local", "bin", Binary)
			if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				return p, nil
			}
		}
	}
	return "", notInstalled()
}

func (a *Adapter) realHome() (string, error) {
	if a.RealHome != "" {
		return a.RealHome, nil
	}
	return os.UserHomeDir()
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

// permissionArgs maps Rein's mode onto agy's flags. Only the two modes that
// decide up front are reachable: CheckSupport refuses ask and accept_edits
// because approvals is `no`.
func permissionArgs(m adapter.PermissionMode) ([]string, error) {
	switch m {
	case adapter.PermissionReadOnly:
		// Measured: in plan mode a write_file and a run_command were both
		// denied and no file appeared.
		return []string{"--mode=plan"}, nil
	case adapter.PermissionFull:
		return []string{"--dangerously-skip-permissions"}, nil
	default:
		return nil, fmt.Errorf("%w: permission mode %q has no Antigravity CLI mapping: headless agy cannot ask, so only read_only and full can be honoured",
			adapter.ErrNotSupported, string(m))
	}
}

// buildArgs is the command line. The prompt is never on it — it goes on stdin
// — and neither is the key.
func buildArgs(spec adapter.RunSpec) ([]string, error) {
	perm, err := permissionArgs(spec.PermissionMode)
	if err != nil {
		return nil, err
	}
	// `--print=` with an empty value: stream-json input supplies the prompt,
	// and a bare `-p` would take the next flag as its prompt.
	args := []string{"--print=", "--output-format=stream-json", "--input-format=stream-json"}
	args = append(args, perm...)
	if spec.Model != "" {
		args = append(args, "--model="+spec.Model)
	}
	if spec.Effort != "" {
		args = append(args, "--effort="+spec.Effort)
	}
	if spec.ResumeID != "" {
		args = append(args, "--conversation="+spec.ResumeID)
	}
	if t := spec.Timeouts.Total; t > 0 {
		// agy's own bound, as well as Rein's timer: belt and braces.
		args = append(args, "--print-timeout="+t.String())
	}
	return args, nil
}

// promptText is the one user message. agy has no system-prompt flag, so the
// standing instructions go first, as the contract asks.
func promptText(spec adapter.RunSpec) string {
	body := spec.Prompt
	if body == "" {
		body = "Continue the work already in progress in this conversation."
	}
	if spec.SystemPrompt == "" {
		return body
	}
	return spec.SystemPrompt + "\n\n---\n\n" + body
}

// Start implements [adapter.Adapter].
func (a *Adapter) Start(ctx context.Context, spec adapter.RunSpec) (adapter.Session, error) {
	spec.AgentKind = Kind
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := adapter.CheckSupport(a.Manifest(), spec); err != nil {
		return nil, err
	}
	// Hosted runs are Claude Code or Codex only, and no queue's environment
	// may carry a key of any vendor's — this one's included. The key arrives
	// in ProviderKey, which is not Env.
	if err := spec.CheckPlanEnv(); err != nil {
		return nil, err
	}
	if len(spec.AllowedTools) > 0 || len(spec.DeniedTools) > 0 {
		return nil, fmt.Errorf("%w: Antigravity CLI has no per-run tool list, so a run that narrows its tools cannot be honoured",
			adapter.ErrNotSupported)
	}
	key := strings.TrimSpace(spec.ProviderKey)
	if len(key) < secretenv.MinValueLen {
		return nil, ErrNoKey
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
	h, err := a.prepareHome()
	if err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	return a.startProcess(spec, bin, args, h, key)
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
