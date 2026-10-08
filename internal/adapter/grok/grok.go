// Package grok drives xAI's Grok Build through `grok agent stdio`, its
// implementation of the Agent Client Protocol.
//
// ACP rather than `grok -p --output-format streaming-json` because the
// streaming-json surface is one-way: it prints the same session updates and
// has no channel for an approval, a cancel, or a resume. `grok agent stdio` is
// JSON-RPC in both directions, which is what a queue-driven runner needs.
//
// This is Grok Build, xAI's official CLI — not `@vibe-kit/grok-cli`, which is
// a different and unrelated tool (elk `docs/rein.md` §3).
//
// Two of the manifest's declarations are `partial` and both are honest
// admissions rather than gaps in the code; docs/adapter-grok.md gives the
// evidence for each and says exactly what would promote it.
package grok

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// Kind is the agent kind this adapter registers under.
const Kind = "grok"

// Binary is the executable the adapter drives. The Grok installer puts it in
// ~/.grok/bin, which it also adds to PATH; [fallbackBinary] is the belt to
// that braces, for a daemon started from a login shell that never sourced a
// profile.
const Binary = "grok"

func fallbackBinary() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.grok/bin/grok"
}

// sandboxEnv selects a Grok sandbox profile for the child process. It is
// process-wide rather than per-session, which is fine here because Rein spawns
// one agent process per session.
const sandboxEnv = "GROK_SANDBOX"

// sandboxReadOnly is the built-in profile that makes [adapter.PermissionReadOnly]
// mean something: read everywhere, write only to ~/.grok and the temp dirs.
const sandboxReadOnly = "read-only"

// defaultStartupTimeout bounds initialize + session/new. Grok connects the MCP
// servers during session/new, so this is seconds rather than milliseconds.
const defaultStartupTimeout = 90 * time.Second

// deltaHeartbeat coalesces the message and thought chunk streams. One event
// per token would drown the run loop; none at all reads to the watchdog as a
// stalled agent.
const deltaHeartbeat = 5 * time.Second

func init() { adapter.MustRegister(New()) }

// Adapter drives Grok Build. Safe for concurrent use.
type Adapter struct {
	binary    string
	spawn     func(ctx context.Context, spec adapter.RunSpec) (*process, error)
	heartbeat time.Duration
}

// New returns the Grok adapter.
func New() *Adapter {
	a := &Adapter{binary: Binary, heartbeat: deltaHeartbeat}
	a.spawn = a.spawnProcess
	return a
}

// Name implements [adapter.Adapter].
func (a *Adapter) Name() string { return Kind }

// Manifest implements [adapter.Adapter].
//
// Checked against `grok 1.0.13` on darwin/arm64, 2026-08-29. Two declarations
// are `partial`, and `partial` fails closed exactly like `no` — which is the
// point. An adapter that cannot control approvals must not be handed a run
// that requires them, and saying so in the manifest is how that refusal
// happens before a worktree exists rather than in the middle of a session.
func (a *Adapter) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Kind:   Kind,
		Binary: Binary,
		Capabilities: map[adapter.Capability]adapter.Support{
			adapter.CapGit:              adapter.SupportYes,
			adapter.CapWorktree:         adapter.SupportPartial,
			adapter.CapFileEdit:         adapter.SupportYes,
			adapter.CapShell:            adapter.SupportYes,
			adapter.CapMCP:              adapter.SupportYes,
			adapter.CapResume:           adapter.SupportYes,
			adapter.CapStructuredEvents: adapter.SupportYes,
			adapter.CapApprovals:        adapter.SupportPartial,
			adapter.CapSteer:            adapter.SupportNo,
		},
		Platforms: []adapter.Platform{
			adapter.AnyArch("darwin"), adapter.AnyArch("linux"), adapter.AnyArch("windows"),
		},
		Notes: "approvals is `partial` because the round trip is unverified, not " +
			"because it is unimplemented: session/request_permission is handled per " +
			"the ACP spec, but in four probes on 2026-08-29 Grok never sent one — " +
			"every permission gate resolved inside the agent (pending_interaction → " +
			"interaction_resolved, milliseconds apart, no client round trip), " +
			"apparently from the developer's own permission rules. Rein therefore " +
			"cannot claim to control Grok's approvals, so `ask` and `accept_edits` " +
			"fail closed and the usable modes are read_only and full. " +
			"worktree is `partial` for the same kind of reason: read_only is " +
			"enforced by Grok's own OS sandbox (GROK_SANDBOX=read-only), but full " +
			"runs with no sandbox and ACP exposes no per-session filesystem bound, " +
			"so in full the agent can reach outside the directory it was given. " +
			"Promoting it means mapping full onto GROK_SANDBOX=workspace and " +
			"verifying it holds. " +
			"mcp is `yes` — a Rein-supplied server reached session/new and was " +
			"counted in the agent's connection tally — but ACP MCP here is http and " +
			"sse only, no stdio, and Start rejects a server config that is neither " +
			"rather than letting session/new fail mid-handshake. It means exactly " +
			"the packet's servers: every session runs under a private GROK_HOME, so " +
			"the developer's own config.toml servers — the Elk connector that " +
			"submitted a deliverable under his identity — do not load " +
			"(ark:rein#21, home.go).",
	}
}

// Preflight implements [adapter.Adapter]. `grok models` is the login check: it
// asks xAI what models this account has, prints "You are logged in with …",
// and returns in about a second. It reads no credential — that is Grok's job,
// on the developer's own login (elk `docs/rein.md` §3).
func (a *Adapter) Preflight(ctx context.Context) error {
	path, err := a.resolveBinary()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "models").CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("%w: `%s models` failed (%v): %s; run `grok login`, or `grok login --device-auth` on a headless box",
			adapter.ErrPreflight, a.binary, err, firstLine(text))
	}
	if !strings.Contains(strings.ToLower(text), "logged in") {
		return fmt.Errorf("%w: %s reports no login: %s; run `grok login`, or `grok login --device-auth` on a headless box",
			adapter.ErrPreflight, a.binary, firstLine(text))
	}
	return nil
}

func (a *Adapter) resolveBinary() (string, error) {
	if path, err := exec.LookPath(a.binary); err == nil {
		return path, nil
	}
	// The ~/.grok/bin fallback is for the standard install, not for whatever
	// binary the caller named: falling back from a configured `grok-nightly`
	// to the stock `grok` would be running a different agent than the one
	// asked for.
	if a.binary == Binary {
		if fb := fallbackBinary(); fb != "" {
			if st, err := os.Stat(fb); err == nil && !st.IsDir() {
				return fb, nil
			}
		}
	}
	return "", fmt.Errorf("%w: %s is not on PATH and is not at ~/.grok/bin/grok; install Grok Build (curl -fsSL https://grok.com/install.sh | bash)",
		adapter.ErrPreflight, a.binary)
}

// Start implements [adapter.Adapter].
func (a *Adapter) Start(ctx context.Context, spec adapter.RunSpec) (adapter.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := adapter.CheckSupport(a.Manifest(), spec); err != nil {
		return nil, err
	}
	mode, err := permissionSettings(spec.PermissionMode)
	if err != nil {
		return nil, err
	}
	// XAI_API_KEY in the session's environment moves it onto metered xAI API
	// billing; the auth method on initialize is the backstop (planauth.go).
	if err := spec.CheckPlanEnv(); err != nil {
		return nil, err
	}
	if len(spec.AllowedTools) > 0 || len(spec.DeniedTools) > 0 {
		// `--allow` and `--deny` are flags on the interactive `grok` command,
		// not on `grok agent`, and ACP has no per-session tool policy. Refuse
		// rather than drop what the packet asked for.
		return nil, fmt.Errorf("%w: `grok agent` takes no tool allow/deny list; "+
			"narrow the session with PermissionMode instead", adapter.ErrNotSupported)
	}
	servers, err := mcpServers(spec.MCPServers)
	if err != nil {
		return nil, err
	}

	startupCtx, cancelStartup := context.WithTimeout(
		context.WithoutCancel(ctx), orDefault(spec.Timeouts.Startup, defaultStartupTimeout))
	defer cancelStartup()

	proc, err := a.spawn(startupCtx, specWithSandbox(spec, mode))
	if err != nil {
		return nil, fmt.Errorf("grok: starting `%s agent stdio`: %w", a.binary, err)
	}

	s := newSession(a, spec, proc, mode, servers)
	if err := s.handshake(startupCtx); err != nil {
		s.abort()
		return nil, err
	}
	s.run()
	return s, nil
}

// permissionMode is how one Rein mode is expressed to Grok: a sandbox profile
// for the process, and the session `_meta` flags.
type permissionMode struct {
	sandboxProfile string // "" leaves GROK_SANDBOX alone
	yolo           bool
}

// permissionSettings maps Rein's modes onto what `grok agent` actually offers.
//
//	read_only → GROK_SANDBOX=read-only, yoloMode        the OS refuses the writes
//	full      → yoloMode                                everything auto-approved
//	ask, accept_edits → refused
//
// `read_only` sets yoloMode deliberately, and the reasoning matters: with
// `approvals: partial` nobody has promised to answer a permission request, so
// a session that waits on one hangs. The sandbox is what makes the mode
// read-only — kernel-enforced, not agent-enforced — and auto-approving inside
// a jail that permits no writes is safe in a way auto-approving without one is
// not.
//
// `ask` and `accept_edits` are unreachable in practice: `CheckSupport` refuses
// them against a `partial` approvals declaration before Start is called. The
// branch exists so the refusal has a message rather than a panic if that ever
// changes.
func permissionSettings(m adapter.PermissionMode) (permissionMode, error) {
	switch m {
	case adapter.PermissionReadOnly:
		return permissionMode{sandboxProfile: sandboxReadOnly, yolo: true}, nil
	case adapter.PermissionFull:
		return permissionMode{yolo: true}, nil
	case adapter.PermissionAsk, adapter.PermissionAcceptEdits:
		return permissionMode{}, fmt.Errorf(
			"%w: grok declares approvals `partial`, so %q cannot be served; see docs/adapter-grok.md",
			adapter.ErrNotSupported, string(m))
	default:
		return permissionMode{}, fmt.Errorf("grok: permission mode %q is not one this adapter maps", string(m))
	}
}

// specWithSandbox adds the sandbox profile to the child environment without
// disturbing an explicit one the caller set: a run that says GROK_SANDBOX in
// [adapter.RunSpec.Env] means it, and silently overwriting it would be the
// same class of mistake as silently downgrading a permission mode.
func specWithSandbox(spec adapter.RunSpec, mode permissionMode) adapter.RunSpec {
	if mode.sandboxProfile == "" {
		return spec
	}
	env := make(map[string]string, len(spec.Env)+1)
	for k, v := range spec.Env {
		env[k] = v
	}
	if _, set := env[sandboxEnv]; !set {
		env[sandboxEnv] = mode.sandboxProfile
	}
	spec.Env = env
	return spec
}

// mcpServers turns Rein's name-keyed map into the array ACP wants, and rejects
// what Grok cannot take.
//
// Grok's `initialize` reports `mcpCapabilities: {http: true, sse: true}` and
// **no stdio**. A stdio-shaped entry is rejected by the agent with a bare
// "Invalid params" in the middle of the handshake, which is a bad way to find
// out; catching it here means the run is refused before a worktree exists,
// with a message naming the server.
func mcpServers(in map[string]any) ([]any, error) {
	if len(in) == 0 {
		return []any{}, nil
	}
	out := make([]any, 0, len(in))
	for name, cfg := range in {
		m, ok := cfg.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("grok: MCP server %q: want an object, got %T", name, cfg)
		}
		transport, _ := m["type"].(string)
		if transport != "http" && transport != "sse" {
			return nil, fmt.Errorf("%w: MCP server %q has transport %q; `grok agent` takes http and sse only, not stdio",
				adapter.ErrNotSupported, name, transport)
		}
		entry := make(map[string]any, len(m)+1)
		for k, v := range m {
			entry[k] = v
		}
		entry["name"] = name
		if _, ok := entry["headers"]; !ok {
			// The agent's McpServer union requires the member; omitting it is
			// the other way to earn an "Invalid params".
			entry["headers"] = []any{}
		}
		out = append(out, entry)
	}
	return out, nil
}

// process is one `grok agent stdio`, or the in-process stand-in a test uses.
type process struct {
	stdin  io.WriteCloser
	stdout io.Reader

	// home is the run-private GROK_HOME this process was given, and nil in a
	// test that spawns nothing. See home.go for why a session gets one.
	home *privateHome

	stderrTail *tail
	wait       func() error
	kill       func()
	exitCode   func() int
}

// agentArgs is the `grok agent stdio` command line for spec.
func agentArgs(spec adapter.RunSpec) []string {
	args := []string{"agent"}
	if spec.Model != "" {
		// An agent-level option: after `agent`, before the transport name.
		args = append(args, "--model", spec.Model)
	}
	if spec.Effort != "" {
		// Also agent-level; `--effort` is its alias.
		args = append(args, "--reasoning-effort", spec.Effort)
	}
	return append(args, "stdio")
}

func (a *Adapter) spawnProcess(ctx context.Context, spec adapter.RunSpec) (*process, error) {
	path, err := a.resolveBinary()
	if err != nil {
		return nil, err
	}
	args := agentArgs(spec)

	// Every session gets a GROK_HOME of its own, so the developer's
	// config.toml — the Elk MCP connector above all — does not reach the
	// agent. A session that cannot be isolated does not start
	// (ark:rein#21, home.go).
	home, err := newPrivateHome(spec.RunID)
	if err != nil {
		return nil, err
	}

	// Deliberately not a CommandContext: cancelling the ctx passed to Start
	// must not stop the session — only Session.Interrupt does.
	cmd := exec.Command(path, args...)
	cmd.Dir = spec.WorktreeDir
	cmd.Env = home.runEnv(spec.InheritedEnv(), spec.Env)

	stdin, stdout, stderr, err := pipes(cmd)
	if err != nil {
		home.remove()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		home.remove()
		return nil, err
	}

	t := newTail(40)
	go t.drain(stderr)

	var once sync.Once
	var waitErr error
	done := make(chan struct{})
	return &process{
		stdin:      stdin,
		stdout:     stdout,
		home:       home,
		stderrTail: t,
		wait: func() error {
			once.Do(func() { waitErr = cmd.Wait(); close(done) })
			<-done
			return waitErr
		},
		kill: func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		},
		exitCode: func() int {
			if cmd.ProcessState == nil {
				return -1
			}
			return cmd.ProcessState.ExitCode()
		},
	}, nil
}

// tail keeps the last n lines a process wrote to stderr. For Grok it does more
// than help debugging: it is where the sandbox tells you it did not apply.
type tail struct {
	mu    sync.Mutex
	n     int
	lines []string
}

func newTail(n int) *tail { return &tail{n: n} }

func (t *tail) drain(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		t.mu.Lock()
		t.lines = append(t.lines, sc.Text())
		if len(t.lines) > t.n {
			t.lines = t.lines[len(t.lines)-t.n:]
		}
		t.mu.Unlock()
	}
}

func (t *tail) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

// sandboxFailed reports whether the process complained that its sandbox
// profile could not be applied.
//
// Grok's own documentation says a built-in profile that fails to apply
// "warns and continues without enforcement". A read_only session that
// continues without enforcement is a read_only session that can write, which
// is the exact downgrade the contract exists to prevent — so the adapter reads
// the warning and fails instead. (The stricter refusals Grok makes on its own,
// where it exits rather than start unprotected, are caught by the handshake
// failing.)
func (t *tail) sandboxFailed() (string, bool) {
	if t == nil {
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, line := range t.lines {
		l := strings.ToLower(line)
		if strings.Contains(l, "sandbox") &&
			(strings.Contains(l, "could not be applied") ||
				strings.Contains(l, "without enforcement") ||
				strings.Contains(l, "failed to apply")) {
			return line, true
		}
	}
	return "", false
}

// pipes wires the three standard streams, so the caller has one error to
// handle rather than three identical ones.
func pipes(cmd *exec.Cmd) (io.WriteCloser, io.Reader, io.Reader, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	return stdin, stdout, stderr, nil
}

// mergeMaps overlays b on a without touching either. A key the caller set in
// [adapter.RunSpec.Env] wins over the isolation default, deliberately: a run
// that names GROK_HOME means it, the same way an explicit GROK_SANDBOX does.
func mergeMaps(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func mergeEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(extra))
	override := make(map[string]bool, len(extra))
	for k := range extra {
		override[k] = true
	}
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 && override[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "(no output)"
	}
	return s
}

// hostSupported reports whether the manifest covers this machine. Used in
// tests; the run loop goes through [adapter.CheckSupport].
func hostSupported(m adapter.Manifest) bool {
	return m.SupportsPlatform(runtime.GOOS, runtime.GOARCH)
}
