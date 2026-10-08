// Package codex drives OpenAI's Codex CLI through `codex app-server`, its
// JSON-RPC 2.0 control surface.
//
// `codex app-server` rather than `codex exec --json` because the app server is
// the interface OpenAI built for "a task queue or orchestration layer" — it is
// bidirectional, so approvals come back as requests Rein answers instead of
// being decided up front, and a thread survives the turn that created it. The
// `exec` surface is one-shot and its approval flags do not exist:
// `--ask-for-approval` and `--full-auto` are rejected as unexpected arguments
// however many docs still list them (elk `docs/rein.md` §3).
//
// Everything this package knows about the protocol was generated from the
// binary — `codex app-server generate-json-schema --out` — and verified by
// driving the real thing. See schema/README.md for the command and
// docs/adapter-codex.md for the event mapping.
package codex

import (
	"bufio"
	"context"

	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// Kind is the agent kind this adapter registers under; it is the value of
// `agent_kind` in config.toml and of Elk's `agent_kind` on a queue.
const Kind = "codex"

// Binary is the executable the adapter drives.
const Binary = "codex"

// defaultStartupTimeout bounds the handshake — spawn, initialize, thread,
// first turn. Codex starts the packet's MCP servers during `thread/start`, and
// a slow one is the usual reason this takes seconds rather than milliseconds.
// Before `initialize` Codex indexes the history in its home, which is why that
// history is Rein's own and small (home.go, ark:rein#38).
const defaultStartupTimeout = 90 * time.Second

// deltaHeartbeat is how long a stream of agent-message deltas may go without
// the adapter emitting anything, before it emits one [adapter.EventProgress]
// to say the agent is alive.
//
// The deltas themselves are one event per token, which would drown the run
// loop and inflate Seq past any use. But a long turn can otherwise be minutes
// between structured events, and the watchdog reads silence as a stall
// (elk `docs/rein.md` §4, the Loom ladder). So: coalesce, do not drop.
const deltaHeartbeat = 5 * time.Second

func init() { adapter.MustRegister(New()) }

// Adapter drives Codex. Safe for concurrent use; one adapter starts many
// sessions.
type Adapter struct {
	// ResolveWranglerConnector overrides the owner keychain resolver for tests.
	ResolveWranglerConnector func(context.Context, string) (string, error)

	// binary is the executable to run. Overridden in tests.
	binary string

	// spawn produces the process a session talks to. The real one execs
	// `codex app-server`; a test substitutes an in-process peer replaying
	// recorded frames, which is the only way to test the translation without
	// spending a model call on every assertion.
	spawn func(ctx context.Context, spec adapter.RunSpec) (*process, error)

	// spawnProbe produces the process a headroom read talks to — see
	// headroom.go. Overridden in tests for the same reason as spawn.
	spawnProbe func(ctx context.Context) (*process, error)

	// probeExitGrace overrides how long a headroom read waits for the app
	// server to exit after stdin closes. Zero is the default; tests shorten
	// it, because their stand-in process only ever exits when killed.
	probeExitGrace time.Duration

	// heartbeat is [deltaHeartbeat], overridden in tests so an event sequence
	// is deterministic rather than wall-clock dependent.
	heartbeat time.Duration

	// clock stamps a rate-limit reading. Nil is the wall clock.
	clock func() time.Time
}

// New returns the Codex adapter.
func New() *Adapter {
	a := &Adapter{binary: Binary, heartbeat: deltaHeartbeat}
	a.spawn = a.spawnProcess
	a.spawnProbe = a.spawnProbeProcess
	return a
}

func (a *Adapter) now() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}

// Name implements [adapter.Adapter].
func (a *Adapter) Name() string { return Kind }

// Manifest implements [adapter.Adapter].
//
// Every declaration here was checked against the running binary on
// 2026-08-29 (codex-cli 0.150.1, darwin/arm64), not inferred from the docs.
// The one that is a judgement rather than an observation is `worktree`, and
// the note says why.
func (a *Adapter) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Kind:   Kind,
		Binary: Binary,
		Capabilities: map[adapter.Capability]adapter.Support{
			adapter.CapGit:              adapter.SupportYes,
			adapter.CapWorktree:         adapter.SupportYes,
			adapter.CapFileEdit:         adapter.SupportYes,
			adapter.CapShell:            adapter.SupportYes,
			adapter.CapMCP:              adapter.SupportYes,
			adapter.CapResume:           adapter.SupportYes,
			adapter.CapStructuredEvents: adapter.SupportYes,
			adapter.CapApprovals:        adapter.SupportYes,
			adapter.CapSteer:            adapter.SupportYes,
		},
		Platforms: []adapter.Platform{
			adapter.AnyArch("darwin"), adapter.AnyArch("linux"), adapter.AnyArch("windows"),
		},
		Notes: "worktree is `yes` on the strength of the OS sandbox, not of the " +
			"process boundary: every mode but `full` starts the thread under " +
			"Codex's `read-only` or `workspace-write` sandbox, whose writable root " +
			"is the cwd — the worktree. Loom declares Codex worktree isolation " +
			"`partial` because its adapter does not set the sandbox; this one " +
			"always does, and `full` (danger-full-access) is by contract only ever " +
			"pointed at a worktree of a repository we own. " +
			"mcp is `yes` via thread/start params.config.mcp_servers, verified with " +
			"a probe server — and it means exactly the packet's servers: every " +
			"session runs under a private CODEX_HOME, so the developer's own " +
			"config.toml servers do not load (ark:rein#21, home.go). " +
			"AllowedTools/DeniedTools are refused rather than ignored: the app " +
			"server has no per-tool allow list this adapter has verified, and " +
			"silently dropping one would be exactly the downgrade the contract " +
			"forbids.",
	}
}

// Preflight implements [adapter.Adapter]. It checks the binary is on PATH and
// that Codex reports a login — it never reads, refreshes or replays the
// credential itself, which is the rule the whole design rests on
// (elk `docs/rein.md` §3).
func (a *Adapter) Preflight(ctx context.Context) error {
	path, err := exec.LookPath(a.binary)
	if err != nil {
		return fmt.Errorf("%w: %s is not on PATH; install the Codex CLI (npm i -g @openai/codex, or brew install codex)",
			adapter.ErrPreflight, a.binary)
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "login", "status").CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("%w: `%s login status` failed (%v): %s; run `codex login`, or `codex login --device-auth` on a headless box",
			adapter.ErrPreflight, a.binary, err, firstLine(text))
	}
	if !strings.Contains(text, "Logged in") {
		return fmt.Errorf("%w: %s reports no login: %s; run `codex login`, or `codex login --device-auth` on a headless box",
			adapter.ErrPreflight, a.binary, firstLine(text))
	}
	return nil
}

// Start implements [adapter.Adapter].
func (a *Adapter) Start(ctx context.Context, spec adapter.RunSpec) (adapter.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := adapter.CheckSupport(a.Manifest(), spec); err != nil {
		return nil, err
	}
	sandbox, approval, err := permissionFlags(spec.PermissionMode)
	if err != nil {
		return nil, err
	}
	// OPENAI_API_KEY or CODEX_API_KEY in the session's environment moves it
	// onto metered billing; the account check after initialize is the
	// backstop for everything else (planauth.go).
	if err := spec.CheckPlanEnv(); err != nil {
		return nil, err
	}
	if len(spec.AllowedTools) > 0 || len(spec.DeniedTools) > 0 {
		return nil, fmt.Errorf("%w: codex app-server has no verified per-tool allow/deny list; "+
			"narrow the session with PermissionMode and the sandbox instead", adapter.ErrNotSupported)
	}

	connectorURL := ""
	if spec.WranglerConnectorAccount != "" {
		resolve := a.ResolveWranglerConnector
		if resolve == nil {
			resolve = resolveWranglerConnector
		}
		u, err := resolve(ctx, spec.WranglerConnectorAccount)
		if err != nil {
			return nil, fmt.Errorf("codex: owner Elk connector is unavailable")
		}
		if !validConnectorURL(u) {
			return nil, fmt.Errorf("codex: owner Elk connector is not a valid HTTPS URL")
		}
		connectorURL = u
		spec.MCPServers = map[string]any{"elk": map[string]any{"url": u}}
	}
	startupCtx, cancelStartup := context.WithTimeout(
		context.WithoutCancel(ctx), orDefault(spec.Timeouts.Startup, defaultStartupTimeout))
	defer cancelStartup()

	proc, err := a.spawn(startupCtx, spec)
	if err != nil {
		return nil, fmt.Errorf("codex: starting `%s app-server`: %w", a.binary, err)
	}

	s := newSession(a, spec, proc, sandbox, approval)
	s.connectorURL = connectorURL
	if err := s.handshake(startupCtx); err != nil {
		s.abort()
		return nil, s.redactError(err)
	}
	s.run()
	return s, nil
}

// permissionFlags maps Rein's four modes onto Codex's two knobs. The table is
// repeated in docs/adapter-codex.md; this is the copy that runs.
//
//	read_only    → read-only          + never       nothing can be authorised
//	accept_edits → workspace-write    + on-request  writes in the worktree are free,
//	                                                escalations come back as requests
//	ask          → workspace-write    + untrusted   everything not already trusted asks
//	full         → danger-full-access + never       everything is authorised up front
//
// `never` is what makes read_only and full quiet: with it Codex raises no
// approval at all, so neither mode can hang waiting for an answer nobody
// promised to give.
func permissionFlags(m adapter.PermissionMode) (sandboxMode, askForApproval, error) {
	switch m {
	case adapter.PermissionReadOnly:
		return sandboxReadOnly, approvalNever, nil
	case adapter.PermissionAcceptEdits:
		return sandboxWorkspaceWrite, approvalOnRequest, nil
	case adapter.PermissionAsk:
		return sandboxWorkspaceWrite, approvalUntrusted, nil
	case adapter.PermissionFull:
		return sandboxFullAccess, approvalNever, nil
	default:
		return "", "", fmt.Errorf("codex: permission mode %q is not one this adapter maps", string(m))
	}
}

// process is one `codex app-server`, or the in-process stand-in a test uses.
type process struct {
	stdin  io.WriteCloser
	stdout io.Reader

	// home is the run-private CODEX_HOME this process was given, and nil in a
	// test that spawns nothing. It owns the transcript-path rewrite and its own
	// removal; see home.go for why a session gets one at all.
	home *privateHome

	// stderr is drained into a ring the session quotes in a failure. The app
	// server puts real diagnostics there — a bad `-c` override, a config
	// error — and a session that dies without them is a session nobody can
	// debug.
	stderrTail *tail

	// wait blocks until the process exits and reports its status.
	wait func() error

	// kill stops it without waiting.
	kill func()

	// exitCode is valid after wait returns.
	exitCode func() int
}

func (a *Adapter) spawnProcess(ctx context.Context, spec adapter.RunSpec) (*process, error) {
	// The command is deliberately built without a cancellable context: the
	// contract says cancelling the ctx passed to Start must not stop the
	// session, only Session.Interrupt does.
	// Every session gets a CODEX_HOME of its own, so the developer's
	// config.toml — MCP servers above all — does not reach the agent. A
	// session that cannot be isolated does not start (ark:rein#21, home.go).
	home, err := newPrivateHome(spec.RunID)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(a.binary, "app-server")
	cmd.Dir = spec.WorktreeDir
	cmd.Env = mergeEnv(spec.InheritedEnv(), mergeMaps(home.env(), spec.Env))

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

// tail keeps the last n lines a process wrote to stderr.
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
// that names CODEX_HOME means it, and silently overriding it would be the same
// class of mistake as silently downgrading a permission mode.
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

// hostSupported reports whether the manifest covers this machine. Only used in
// tests; the run loop goes through [adapter.CheckSupport].
func hostSupported(m adapter.Manifest) bool {
	return m.SupportsPlatform(runtime.GOOS, runtime.GOARCH)
}
