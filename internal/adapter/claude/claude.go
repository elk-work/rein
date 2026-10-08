// Package claude drives Claude Code as a Rein agent session.
//
// It shells out to the `claude` binary in headless mode and reads its
// stream-json NDJSON output:
//
//	claude -p --output-format stream-json --verbose --input-format stream-json
//	       --permission-mode <mapped> --settings <hooks> [--model …] [--resume …]
//
// The prompt is delivered over stdin as a stream-json message rather than as an
// argument, which is what makes [Session.Send] possible at all. Everything this
// package knows about Claude Code lives here; nothing outside
// internal/adapter/claude may learn that Claude Code speaks stream-json.
//
// # Auth
//
// [Adapter.Preflight] runs `claude auth status` and requires a login. It never
// reads, mints, refreshes or replays a token: Rein runs on the developer's own
// Claude Code login and that is the whole basis of the design (elk
// docs/rein.md §3). For the same reason this package never passes `--bare`,
// which disables subscription auth and forces ANTHROPIC_API_KEY.
//
// # What was verified, and when
//
// Everything below was measured against `claude` 2.1.251 on darwin/arm64 on
// 2026-08-29, and the recordings are in testdata/. Two measurements contradict
// what was assumed when the contract was written, and both matter:
//
//   - `claude -p` does NOT hang on an unauthorised edit. It emits
//     `system/permission_denied` and carries on, so a headless run auto-denies
//     rather than blocking. That is why [adapter.CapApprovals] is declared
//     partial and not no: a real signal exists, but nothing can answer it.
//   - Under `--input-format stream-json` a `result` line is a *turn* boundary,
//     not the end of the session — the process stays alive waiting for more
//     stdin. This adapter therefore treats the FIRST result as terminal and
//     closes stdin, which is what keeps one Rein run equal to one session.
//
// See docs/adapter-claude.md for the full command line and the footguns.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// Kind is the agent kind this adapter registers under.
const Kind = "claude"

// DefaultBinary is the executable looked up on PATH when [Adapter.Binary] is
// empty.
const DefaultBinary = "claude"

func init() { adapter.MustRegister(New()) }

// Default timeouts, used where [adapter.Timeouts] leaves a zero.
const (
	// defaultStartup bounds the wait for the `system/init` line. A cold start
	// pays for plugin sync and MCP server spawning, so this is generous.
	defaultStartup = 2 * time.Minute

	// defaultInterruptGrace is how long [Session.Interrupt] lets the process
	// settle after SIGINT before it is killed.
	defaultInterruptGrace = 10 * time.Second

	// defaultExitGrace is how long the session waits for the process to exit
	// after stdin is closed at the end of a turn.
	defaultExitGrace = 30 * time.Second
)

// Adapter drives Claude Code. The zero value is not usable; call [New].
type Adapter struct {
	// Binary overrides the executable name or path. Empty means
	// [DefaultBinary] resolved on PATH. This is the config override a
	// developer needs when `claude` is not on the daemon's PATH.
	Binary string

	// InterruptGrace is how long [Session.Interrupt] waits after SIGINT before
	// killing. Zero means [defaultInterruptGrace].
	InterruptGrace time.Duration

	// DisableWatchdog turns off the hook listener. The session still runs; it
	// simply has no out-of-band idle signal. Tests use it to avoid binding a
	// port.
	DisableWatchdog bool

	// ResolveWranglerConnector overrides the named OS keychain resolver for tests.
	ResolveWranglerConnector func(context.Context, string) (string, error)

	// SettingSources is the value passed to --setting-sources. Empty means
	// [DefaultSettingSources], which keeps the repository's own checked-in
	// settings and drops the developer's.
	//
	// Set it to "" via the sentinel [NoSettingSources] to load nothing at all,
	// for a deployment that wants a run to depend on nothing but its packet.
	SettingSources string
}

// Setting-source policies for [Adapter.SettingSources].
const (
	// DefaultSettingSources keeps settings checked into the repository under
	// work and drops the two personal sources: `user` (~/.claude — the
	// developer's hooks, plugins and skills) and `local`
	// (.claude/settings.local.json, gitignored).
	DefaultSettingSources = "project"

	// NoSettingSources loads no settings files at all, the repository's
	// included. Pass it to [Adapter.SettingSources] for a run that depends on
	// nothing but its packet.
	NoSettingSources = "none"
)

// settingSources resolves the configured policy to the flag's value. The
// "none" sentinel exists because the flag wants an empty string for "nothing",
// and an empty Go field has to keep meaning "unset" so the default applies.
func (a *Adapter) settingSources() string {
	switch a.SettingSources {
	case "":
		return DefaultSettingSources
	case NoSettingSources:
		return ""
	default:
		return a.SettingSources
	}
}

// New returns a Claude Code adapter with the default binary and grace period.
func New() *Adapter { return &Adapter{} }

// Name implements [adapter.Adapter].
func (a *Adapter) Name() string { return Kind }

// binary is the executable to run.
func (a *Adapter) binary() string {
	if a.Binary != "" {
		return a.Binary
	}
	return DefaultBinary
}

// Manifest implements [adapter.Adapter].
//
// Every capability in the vocabulary is declared, including the two declared
// partial, because an omission and a refusal fail identically but only one of
// them reads as a decision.
func (a *Adapter) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Kind:   Kind,
		Binary: a.binary(),
		Capabilities: map[adapter.Capability]adapter.Support{
			adapter.CapGit:              adapter.SupportYes,
			adapter.CapWorktree:         adapter.SupportPartial,
			adapter.CapFileEdit:         adapter.SupportYes,
			adapter.CapShell:            adapter.SupportYes,
			adapter.CapMCP:              adapter.SupportYes,
			adapter.CapResume:           adapter.SupportYes,
			adapter.CapStructuredEvents: adapter.SupportYes,
			adapter.CapApprovals:        adapter.SupportPartial,
			adapter.CapSteer:            adapter.SupportYes,
		},
		Platforms: []adapter.Platform{
			adapter.AnyArch("darwin"), adapter.AnyArch("linux"), adapter.AnyArch("windows"),
		},
		Notes: "approvals is partial, and the consequence is concrete: " +
			"CheckSupport refuses both `ask` and `accept_edits`, so v0 serves " +
			"`read_only` and `full` only. Claude Code 2.1.251 exposes no way for a " +
			"headless caller to answer a permission prompt — the SDK has canUseTool, " +
			"the CLI has no equivalent flag. What `claude -p` does instead is " +
			"auto-DENY: it emits system/permission_denied and keeps going, so the " +
			"denial is visible (relayed as a progress event, and collected in the " +
			"result) but not answerable. Session.Respond returns ErrNotSupported. " +
			"Lift this to yes only when the binary grows a permission-prompt tool. " +
			"\n\nworktree is partial for the same kind of reason as grok's: " +
			"isolation holds in one mode and not the other. The adapter arranges " +
			"what it can — cwd is the worktree and no --add-dir is passed — and " +
			"under read_only a write outside the worktree is auto-denied. But " +
			"under `full` the agent is not sandboxed: bypassPermissions approves a " +
			"write outside the worktree just as readily as one inside it. Claude " +
			"Code also always writes its own state to ~/.claude (the session " +
			"transcript under projects/, and in plan mode a plan file under " +
			"plans/), which is product state rather than the run's work product. " +
			"partial fails closed, so a packet that requires `worktree` by name is " +
			"refused here; none did when this was ruled. Ruled partial on " +
			"Elk Scout #706, 2026-10-01 (ark:rein#12 (01M17YVY2NCFNHATGYQ1TNGSV0)).",
	}
}

// authStatus is the JSON `claude auth status` prints. Only the fields Rein
// reads are named; nothing here is a credential.
type authStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	APIProvider      string `json:"apiProvider"`
	SubscriptionType string `json:"subscriptionType"`
}

// Preflight implements [adapter.Adapter]: the binary is on PATH, it reports a
// version, and a login exists.
//
// It shells out and reads stdout. It does not open a credential file, and the
// only thing it keeps from the auth check is whether someone is logged in and
// by what method.
func (a *Adapter) Preflight(ctx context.Context) error {
	bin := a.binary()
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("%w: %q is not on PATH: %w", adapter.ErrPreflight, bin, err)
	}

	verOut, err := runTool(ctx, path, "--version")
	if err != nil {
		return fmt.Errorf("%w: %s --version failed: %w", adapter.ErrPreflight, bin, err)
	}
	version := strings.TrimSpace(verOut)
	if version == "" {
		return fmt.Errorf("%w: %s --version printed nothing", adapter.ErrPreflight, bin)
	}

	authOut, err := runTool(ctx, path, "auth", "status")
	if err != nil {
		return fmt.Errorf("%w: %s auth status failed (run `%s auth login`): %w",
			adapter.ErrPreflight, bin, bin, err)
	}

	var st authStatus
	if jsonErr := json.Unmarshal([]byte(authOut), &st); jsonErr != nil {
		// Older or newer builds may print prose. Fall back to something a human
		// can still act on rather than failing a working machine on a format
		// change.
		if strings.Contains(strings.ToLower(authOut), "not logged in") {
			return fmt.Errorf("%w: %s reports no login; run `%s auth login`",
				adapter.ErrPreflight, bin, bin)
		}
		return nil
	}
	if !st.LoggedIn {
		return fmt.Errorf("%w: %s is not logged in; run `%s auth login`",
			adapter.ErrPreflight, bin, bin)
	}
	return nil
}

// runTool runs a short, read-only vendor command and returns its stdout.
func runTool(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// permissionFlag maps a Rein permission mode onto the vendor's
// `--permission-mode` value.
//
// The vendor's choices at 2.1.251 are acceptEdits, auto, bypassPermissions,
// manual, dontAsk and plan. Three of them are traps for a runner and none of
// them is used:
//
//   - `manual` is what an unconfigured `-p` run lands in, and it auto-denies.
//   - `dontAsk` also auto-denies anything not pre-allowed — measured, not
//     assumed: it refused both Write and Bash in testdata's sibling probe.
//   - `auto` hands the decision to a classifier, which is not a policy Rein
//     can report to Elk.
//
// `ask` and `accept_edits` have mappings here so that lifting CapApprovals to
// yes is a one-line change rather than a rewrite — but [adapter.CheckSupport]
// refuses both while approvals is partial, so neither is reachable today.
func permissionFlag(m adapter.PermissionMode) (string, error) {
	switch m {
	case adapter.PermissionReadOnly:
		// Plan mode is Claude Code's own read-only stance: it reads and
		// reasons and its edits do not land. Verified — a Write in plan mode
		// left no file behind.
		return "plan", nil
	case adapter.PermissionAcceptEdits:
		return "acceptEdits", nil
	case adapter.PermissionAsk:
		return "manual", nil
	case adapter.PermissionFull:
		// Verified to need no companion flag: --permission-mode
		// bypassPermissions alone wrote the file.
		return "bypassPermissions", nil
	default:
		return "", fmt.Errorf("%w: permission mode %q has no Claude Code mapping",
			adapter.ErrNotSupported, string(m))
	}
}

// Start implements [adapter.Adapter].
func (a *Adapter) Start(ctx context.Context, spec adapter.RunSpec) (adapter.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := adapter.CheckSupport(a.Manifest(), spec); err != nil {
		return nil, err
	}
	mode, err := permissionFlag(spec.PermissionMode)
	if err != nil {
		return nil, err
	}
	// A metered key switches the session off the developer's subscription
	// and onto API billing — under -p, Claude Code always uses
	// ANTHROPIC_API_KEY when it is present. That is a change of auth model,
	// exactly the kind of silent downgrade the contract forbids. The daemon's
	// own environment cannot carry one in (RunSpec.InheritedEnv drops them);
	// the spec and the repository's settings can, so both are checked here,
	// and the start-up line is checked again once the session reports it.
	if err := spec.CheckPlanEnv(); err != nil {
		return nil, err
	}
	if err := checkSettingsAuth(spec.WorktreeDir, a.settingSources()); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	bin, err := exec.LookPath(a.binary())
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not on PATH: %w", adapter.ErrPreflight, a.binary(), err)
	}

	tmpDir, err := os.MkdirTemp("", "rein-claude-")
	if err != nil {
		return nil, fmt.Errorf("claude: creating the session's temp directory: %w", err)
	}

	if rel, err := filepath.Rel(spec.WorktreeDir, tmpDir); spec.WranglerConnectorAccount != "" && err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("claude: temporary directory must be outside the worktree; change TMPDIR")
	}

	s := &Session{
		spec:       spec,
		startCtx:   ctx,
		tmpDir:     tmpDir,
		events:     make(chan adapter.Event),
		done:       make(chan struct{}),
		started:    time.Now(),
		grace:      a.InterruptGrace,
		dec:        &decoder{},
		hookEvents: make(chan hookPayload, 16),
	}
	if s.grace <= 0 {
		s.grace = defaultInterruptGrace
	}

	args, err := a.buildArgs(spec, mode, s)
	if err != nil {
		s.cleanup()
		return nil, err
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = spec.WorktreeDir
	cmd.Env = childEnv(spec.InheritedEnv(), spec.Env)
	setProcAttr(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.cleanup()
		return nil, fmt.Errorf("claude: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.cleanup()
		return nil, fmt.Errorf("claude: stdout pipe: %w", err)
	}
	stderr := &boundedBuffer{limit: 64 << 10}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		s.cleanup()
		return nil, fmt.Errorf("claude: starting %s: %w", bin, err)
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stderr = stderr
	s.argv = append([]string{bin}, args...)

	// The prompt is the first stdin message. A resume with no prompt is legal
	// — the contract allows a spec with only a ResumeID — and simply carries
	// on where the session left off. The system prompt does not go here: it
	// rides on --append-system-prompt, which this build has.
	//
	// A failure here is recorded rather than returned. The overwhelmingly
	// likely cause is that the process is already gone — a rejected flag, an
	// instant crash — which surfaces as EPIPE on the write and races the exit,
	// so returning it would make Start fail intermittently with "broken pipe"
	// while the process's own stderr, which says what actually went wrong, was
	// thrown away. Letting the session start means the normal path reports it:
	// the stream ends, no result arrives, and the terminal error carries the
	// stderr. A write that failed for any other reason is caught by the
	// startup timeout.
	if spec.Prompt != "" {
		if err := s.writeUserMessage(spec.Prompt); err != nil {
			s.promptErr = err
		}
	}

	go s.run(stdout)
	go s.watch()
	return s, nil
}

// buildArgs assembles the command line. It is separated from [Adapter.Start] so
// a unit test can assert the exact argv without spawning anything.
func (a *Adapter) buildArgs(spec adapter.RunSpec, mode string, s *Session) ([]string, error) {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--permission-mode", mode,
	}

	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Effort != "" {
		args = append(args, "--effort", spec.Effort)
	}
	if spec.SystemPrompt != "" {
		// --append-system-prompt takes the prompt itself, not a path: there is
		// no --append-system-prompt-file at 2.1.251.
		args = append(args, "--append-system-prompt", spec.SystemPrompt)
	}
	if len(spec.AllowedTools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, spec.AllowedTools...)
	}
	if len(spec.DeniedTools) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, spec.DeniedTools...)
	}
	if spec.ResumeID != "" {
		args = append(args, "--resume", spec.ResumeID)
	}

	// The MCP config is written and passed on EVERY run, including one whose
	// packet names no servers at all — in which case it is an explicit empty
	// set, `{"mcpServers":{}}`.
	//
	// Making this conditional was a real incident, not a hypothetical. Until
	// ark:rein#21 the flags were passed only when the packet named a server, so
	// an ordinary run inherited the developer's own ~/.claude.json servers:
	// measured on this Mac, a bare `claude -p` session came up with 287 MCP
	// tools, 43 of them the Elk connector's, logged in as the developer. A grok
	// run used exactly that to post its own deliverable to Elk under the developer's
	// identity before Rein reported anything; the claude run of the same dogfood
	// failed instead, on a connector whose URL happened to be invalid. An agent
	// holding the credentials of the person who queued it is the failure this
	// closes, and "the packet named no servers" is precisely when it bit.
	mcpServers := spec.MCPServers
	if spec.WranglerConnectorAccount != "" {
		resolve := a.ResolveWranglerConnector
		if resolve == nil {
			resolve = resolveWranglerConnector
		}
		ctx := s.startCtx
		if ctx == nil {
			ctx = context.Background()
		}
		u, err := resolve(ctx, spec.WranglerConnectorAccount)
		if err != nil {
			return nil, fmt.Errorf("claude: owner Elk connector unavailable in keychain service elk-connector-url")
		}
		if !validConnectorURL(u) {
			return nil, fmt.Errorf("claude: owner Elk connector must be an HTTPS URL")
		}
		s.connectorURL = u
		mcpServers = map[string]any{"elk": map[string]any{"type": "http", "url": u}}
	}
	if mcpServers == nil {
		mcpServers = map[string]any{}
	}
	mcpPath := filepath.Join(s.tmpDir, "mcp.json")
	blob, err := json.Marshal(map[string]any{"mcpServers": mcpServers})
	if err != nil {
		return nil, fmt.Errorf("claude: encoding MCP servers: %w", err)
	}
	if err := os.WriteFile(mcpPath, blob, 0o600); err != nil {
		return nil, fmt.Errorf("claude: writing the MCP config: %w", err)
	}
	args = append(args, "--mcp-config", mcpPath, "--strict-mcp-config")

	// --setting-sources cuts the rest of the developer's machine state. The
	// default keeps `project` — the settings checked into the repository being
	// worked on, which are reviewed and shared — and drops `user` (~/.claude,
	// the developer's own hooks, plugins and skills) and `local`
	// (.claude/settings.local.json, personal and gitignored).
	//
	// Verified on 2.1.251: with it, a session reports no plugins and the
	// developer's SessionStart hook does not fire, while the repository's own
	// checked-in hooks still do — and this adapter's own --settings file is
	// unaffected either way, because --settings is not one of the sources.
	// Subscription login also survives, because auth is not a setting: all four
	// probe combinations exited success on a subscription login.
	args = append(args, "--setting-sources", a.settingSources())

	if !a.DisableWatchdog {
		if wd, err := newWatchdog(s.hookEvents); err == nil {
			s.watchdogSrv = wd
			if path, err := wd.writeSettings(s.tmpDir); err == nil {
				args = append(args, "--settings", path)
			}
		}
		// A watchdog that will not start is not fatal. The stream is the
		// primary signal; the hooks are corroboration.
	}

	// Deliberately never `--bare`: it disables subscription auth.
	return args, nil
}

// childEnv is the inherited environment (RunSpec.InheritedEnv) with the spec's overrides applied.
func childEnv(base []string, over map[string]string) []string {
	if len(over) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 {
			if _, shadowed := over[kv[:i]]; shadowed {
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range over {
		out = append(out, k+"="+v)
	}
	return out
}
