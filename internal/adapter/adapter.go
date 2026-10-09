// Package adapter is the contract between Rein's run loop and the coding
// agents it drives.
//
// One adapter per agent kind, each living in its own package under
// internal/adapter/<kind>/ — internal/adapter/claude, internal/adapter/codex,
// internal/adapter/grok. This package owns the interface, the capability
// vocabulary and the registry; it owns no vendor knowledge at all, and an
// adapter package owns nothing but its own vendor. That split is the whole
// point: the run loop is written once against [Adapter] and never learns that
// Claude Code speaks stream-json while Codex speaks JSON-RPC.
//
// The shape of the capability manifest is borrowed from Loom's
// defaults/docs/runtime-adapters.md §7 (rjwalters/loom, read 2026-08-29),
// including the rule that matters: matching **fails closed**, so only a
// declared "yes" satisfies a requirement. See [Manifest.Satisfies] and
// docs/adapters.md.
//
// The lifecycle:
//
//	a, ok := adapter.Lookup(queue.AgentKind)   // registry
//	missing := a.Manifest().Satisfies(packet.RequiredCapabilities)
//	// non-empty → report the run stuck; create no worktree, do no partial work
//	a.Preflight(ctx)                           // binary present, logged in
//	sess, err := a.Start(ctx, spec)            // one agent process
//	for ev := range sess.Events() { … }        // watch structured events
//	res, err := sess.Wait()                    // terminal result
//
// [Session.Events] must be drained: an adapter that cannot deliver an event
// blocks, and a run loop that stops reading stalls the agent it is watching.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/hosted"
	"github.com/elk-work/rein/internal/secretenv"
)

// Adapter drives one kind of coding agent. Implementations are safe for
// concurrent use: the run loop may hold a single adapter and start several
// sessions from it.
type Adapter interface {
	// Name is the agent kind — "claude", "codex", "grok". It is the registry
	// key and must equal Manifest().Kind.
	Name() string

	// Manifest is what this adapter declares about itself. It must not vary
	// between calls: the run loop caches it, and Elk is told about it at
	// enrolment as `declared_capabilities`.
	Manifest() Manifest

	// Preflight checks that this adapter can actually run here and now — the
	// binary is on PATH, the vendor login is present and not expired, the
	// version is one the adapter understands. It talks to the local machine
	// only; it never mints, reads or replays a credential (elk docs/rein.md:
	// shell out to the vendor binary, never extract a token).
	//
	// The error must be actionable: say which binary or which login, so a
	// human reading a stuck run knows what to do. Wrap [ErrPreflight].
	Preflight(ctx context.Context) error

	// Start launches one agent session against spec and returns as soon as the
	// process is running — it does not wait for the agent to finish. The
	// returned [Session] owns the process until [Session.Wait] returns.
	//
	// Start must reject a spec it cannot honour rather than silently degrading
	// it: an unsupported [RunSpec.PermissionMode], a resume it cannot do, a
	// timeout it cannot enforce. Downgrading a permission mode is the one
	// failure mode this contract exists to prevent.
	//
	// Cancelling ctx after Start returns does not stop the session; use
	// [Session.Interrupt].
	Start(ctx context.Context, spec RunSpec) (Session, error)
}

// Session is one running agent process.
//
// Exactly one goroutine should read [Session.Events]; the others may call
// [Session.Send], [Session.Interrupt] and [Session.Respond] concurrently.
type Session interface {
	// ID is the vendor's own session identifier — a Claude Code session id, a
	// Codex thread id, a Grok session id. It is what [RunSpec.ResumeID] takes
	// and what `rein attach` needs, so a session that has one must expose it
	// before it finishes, not only in its [Result].
	ID() string

	// Events is the structured event stream, closed when the session ends. It
	// is the only sanctioned way to watch a session: Rein reads events, never
	// scrollback.
	//
	// The channel must be drained. The final event before close is
	// [EventDone] or [EventError].
	Events() <-chan Event

	// Send delivers follow-up text to a running session — the answer to an
	// [EventQuestion], or a mid-run steer.
	//
	// Adapters that cannot take input after start return [ErrNotSupported];
	// they are the ones that do not declare a multi-turn channel.
	Send(ctx context.Context, input string) error

	// Respond answers an [EventPermissionRequest]. Adapters declaring
	// [CapApprovals] as anything but [SupportYes] return [ErrNotSupported] —
	// which is why [PermissionAsk] is unusable on them, and why the run loop
	// must check the manifest before choosing a mode rather than discovering
	// it here.
	Respond(ctx context.Context, resp PermissionResponse) error

	// Interrupt asks the session to stop: the agent's own interrupt where it
	// has one, escalating to signalling the process. It returns once the
	// request is delivered; [Session.Wait] still reports the outcome.
	Interrupt(ctx context.Context) error

	// Wait blocks until the session ends and returns its terminal result. It
	// is safe to call more than once and returns the same answer each time.
	//
	// A session that failed returns a [Result] with a non-succeeded status
	// AND a non-nil error; a caller that only inspects the error still learns
	// something went wrong, and one that only inspects the status does too.
	Wait() (Result, error)
}

// RunSpec is everything an adapter needs to start one session. The run loop
// builds it from the Elk packet plus the worktree it just created.
type RunSpec struct {
	// Hosted is fixed by the runner config, never by the packet.
	Hosted    bool
	AgentKind string
	// RunID is Elk's action_run id. Adapters use it for logging and for
	// naming anything they persist; it is not the vendor session id.
	RunID string

	// WorktreeDir is the isolated git worktree the session runs in — created,
	// and reaped, by the run loop. It is the process's working directory and
	// the boundary [CapWorktree] is about.
	WorktreeDir string

	// Prompt is the packet: what the agent is being asked to do.
	Prompt string

	// SystemPrompt is the standing instruction set — the vendored elk-inbox
	// contract (elk docs/rein.md §5a step 4). Adapters that have no system
	// prompt channel must prepend it to Prompt rather than dropping it.
	SystemPrompt string

	// Model is a vendor model id, passed through verbatim. Empty leaves the
	// vendor's own default alone: Rein does not keep a logical-tier table,
	// because tier names are one vendor's vocabulary and mapping them across
	// vendors is a judgement Elk should make, not an adapter. The run loop
	// fills it from the queue's `model` in config.toml.
	Model string

	// Effort is a vendor reasoning-effort level, passed through verbatim —
	// `--effort` for Claude Code, the turn's `effort` for Codex,
	// `--reasoning-effort` for Grok. Empty leaves the vendor's default. The
	// run loop fills it from the queue's `effort` in config.toml.
	Effort string

	// PermissionMode is how mutations are authorised. See [PermissionMode] —
	// an adapter that cannot honour the mode asked for must fail Start.
	PermissionMode PermissionMode

	// AllowedTools and DeniedTools narrow what the session may call, in the
	// vendor's own tool names. Empty AllowedTools means the vendor's default
	// set; DeniedTools always wins over AllowedTools.
	AllowedTools []string
	DeniedTools  []string

	// RequiredCapabilities is the packet's `required_capabilities`, carried
	// here so an adapter can re-check what the run loop already checked.
	// Checking twice is cheap; the run loop checking and an adapter assuming
	// is how a mismatch reaches a worktree.
	RequiredCapabilities []string

	// ResumeID continues a prior session instead of starting a new one.
	// Requires [CapResume].
	ResumeID string

	// MCPServers are the MCP servers to expose to the session, by the vendor's
	// own configuration shape, opaque here. Requires [CapMCP].
	MCPServers map[string]any

	// WranglerConnectorAccount is a secret NAME resolved at session start,
	// never a URL: the keychain account holding a Wrangler queue's owner Elk
	// connector. Empty on every other queue.
	WranglerConnectorAccount string

	// Env is added to the inherited environment for the child process; a key
	// present here overrides the inherited value. Adapters must not put
	// credentials of their own in it — the vendor binary uses the developer's
	// own login. The run loop puts exactly one kind of credential here: a
	// scoped queue's secrets map, resolved from the keychain for this run
	// (ark:rein#48). An adapter passes Env to the agent process and nowhere
	// else — never on a command line, never in a log line, never in a file.
	Env map[string]string

	// PassEnv names daemon variables the child inherits on top of the system
	// variables in internal/secretenv. Every run is scoped: nothing else the
	// daemon holds reaches the agent. The run loop fills it for a queue with
	// no secrets map (its inherit_env, credential-shaped capabilities and MCP
	// server keys) and leaves it empty for a queue with one, whose Env is its
	// only source of credentials. A metered-billing variable is never passed
	// whatever this says ([secretenv.Metered]).
	PassEnv []string

	// Timeouts bound the session. Zero values mean the adapter's default.
	Timeouts Timeouts
}

// InheritedEnv is the base environment for the child process, before Env and
// an adapter's own isolation variables are laid over it: the daemon's system
// variables plus those [RunSpec.PassEnv] names, never a metered-billing one.
func (s RunSpec) InheritedEnv() []string {
	return secretenv.Filter(os.Environ(), s.PassEnv...)
}

// CheckPlanEnv refuses a spec whose Env would move the agent off the
// developer's plan login onto metered API billing. Every adapter calls it at
// the top of Start. InheritedEnv already drops such variables from what the
// daemon passes down; Env is the one door left, so it is checked here.
func (s RunSpec) CheckPlanEnv() error {
	if s.Hosted {
		if s.AgentKind != "claude" && s.AgentKind != "codex" {
			return &APIAuthError{Because: "hosted mode requires Claude Code or Codex"}
		}
		for name := range s.Env {
			if hosted.Forbidden(name, true) {
				return &APIAuthError{Because: "the run environment sets forbidden " + name}
			}
		}
		key := "ANTHROPIC_API_KEY"
		if s.AgentKind == "codex" {
			if s.Env["OPENAI_API_KEY"] != "" || s.Env["CODEX_API_KEY"] != "" {
				return nil
			}
			key = "OPENAI_API_KEY or CODEX_API_KEY"
		} else if s.Env[key] != "" {
			return nil
		}
		return &APIAuthError{Because: key + " is required"}
	}

	if bad := secretenv.MeteredIn(s.Env); len(bad) > 0 {
		return &PlanAuthError{Because: "the run's environment sets " + strings.Join(bad, ", ") +
			", which would switch the agent from the plan login to metered API billing"}
	}
	return nil
}

// PlanAuthError reports a session refused because it would not run on the
// developer's plan login — a metered key in its environment, a repository
// setting that supplies one, or a start-up line naming any auth but the plan.
// Its message is written to be pasted into a run's `stuck` reason.
type PlanAuthError struct {
	Because string
}

func (e *PlanAuthError) Error() string {
	return "not on the plan login: " + e.Because +
		". Rein runs agents on the developer's own subscription only; nothing further was run"
}

// Is makes [errors.Is] match [ErrNotPlanAuth].
func (e *PlanAuthError) Is(target error) bool { return target == ErrNotPlanAuth }

// Timeouts bound a session. Every duration is zero-means-default so a partly
// filled RunSpec is still usable.
type Timeouts struct {
	// Startup is how long Start waits for the process to be usable.
	Startup time.Duration

	// Idle is how long the session may emit no event before the run loop
	// treats it as stalled. This is the input to the watchdog ladder Rein
	// borrows from Loom — no progress, one bounded restart, then quarantine.
	Idle time.Duration

	// Total bounds the whole session regardless of progress.
	Total time.Duration
}

// PermissionMode is how a session authorises mutations. The four values are
// Rein's; each adapter maps them onto its vendor's own flags, and the mapping
// table is in docs/adapters.md.
//
// The mode is explicit and has no zero-value default on purpose. `claude -p`
// starts in Manual mode on every plan and hangs on its first edit
// (elk docs/rein.md §3), so a spec that forgets to say must be rejected, not
// guessed at.
type PermissionMode string

const (
	// PermissionUnset is the zero value. Adapters reject it.
	PermissionUnset PermissionMode = ""

	// PermissionReadOnly — the session may read and reason, and mutate
	// nothing. For review and triage runs.
	PermissionReadOnly PermissionMode = "read_only"

	// PermissionAsk — every mutation raises an [EventPermissionRequest] the
	// run loop must answer. Requires [CapApprovals].
	PermissionAsk PermissionMode = "ask"

	// PermissionAcceptEdits — file edits inside the worktree are approved
	// automatically; shell and network still ask. Requires [CapApprovals] for
	// the half that still asks.
	PermissionAcceptEdits PermissionMode = "accept_edits"

	// PermissionFull — everything is approved automatically. Only ever for a
	// worktree of a repository we own: `-p` skips the workspace-trust dialog,
	// so a queue-driven daemon runs a repo's hooks and MCP servers unprompted
	// (elk docs/rein.md §3).
	PermissionFull PermissionMode = "full"
)

// Valid reports whether m is one of the four defined modes. [PermissionUnset]
// is not valid — see [PermissionMode].
func (m PermissionMode) Valid() bool {
	switch m {
	case PermissionReadOnly, PermissionAsk, PermissionAcceptEdits, PermissionFull:
		return true
	}
	return false
}

// NeedsApprovals reports whether the mode requires [CapApprovals] to be
// serviceable.
func (m PermissionMode) NeedsApprovals() bool {
	return m == PermissionAsk || m == PermissionAcceptEdits
}

// Validate reports the first thing wrong with the spec that an adapter should
// refuse. Adapters call it at the top of Start; it checks what is
// vendor-independent and leaves the rest to them.
func (s RunSpec) Validate() error {
	if s.WorktreeDir == "" {
		return errors.New("adapter: RunSpec has no worktree directory")
	}
	if s.Prompt == "" && s.ResumeID == "" {
		return errors.New("adapter: RunSpec has neither a prompt nor a resume id")
	}
	if !s.PermissionMode.Valid() {
		return fmt.Errorf("adapter: permission mode %q: want read_only, ask, accept_edits or full",
			string(s.PermissionMode))
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{{"startup", s.Timeouts.Startup}, {"idle", s.Timeouts.Idle}, {"total", s.Timeouts.Total}} {
		if d.v < 0 {
			return fmt.Errorf("adapter: %s timeout is negative", d.name)
		}
	}
	return nil
}

// CheckSupport is the single fail-closed gate a run must pass before anything
// is created on disk. It checks, in order, that the adapter runs on this host,
// that the permission mode is serviceable, and that every required capability
// is declared [SupportYes].
//
// The run loop calls this before `git worktree add`. Returning a
// [*CapabilityError] rather than a bare error lets the caller put the missing
// names straight into the `stuck` reason Elk shows a human.
func CheckSupport(m Manifest, spec RunSpec) error {
	if !m.SupportsHost() {
		return &PlatformError{Kind: m.Kind, Platforms: m.Platforms}
	}
	if spec.PermissionMode.NeedsApprovals() && !m.Has(CapApprovals) {
		return &CapabilityError{Kind: m.Kind, Missing: []string{string(CapApprovals)},
			Because: fmt.Sprintf("permission mode %q", spec.PermissionMode)}
	}
	if spec.ResumeID != "" && !m.Has(CapResume) {
		return &CapabilityError{Kind: m.Kind, Missing: []string{string(CapResume)},
			Because: "a resume id was given"}
	}
	if (len(spec.MCPServers) > 0 || spec.WranglerConnectorAccount != "") && !m.Has(CapMCP) {
		return &CapabilityError{Kind: m.Kind, Missing: []string{string(CapMCP)},
			Because: "MCP servers were requested"}
	}
	if missing := m.Satisfies(spec.RequiredCapabilities); len(missing) > 0 {
		return &CapabilityError{Kind: m.Kind, Missing: missing,
			Because: "the packet's required_capabilities"}
	}
	return nil
}

// CapabilityError reports a fail-closed capability refusal. Its message is
// written to be pasted into a run's `stuck` reason.
type CapabilityError struct {
	Kind    string   // the adapter that refused
	Missing []string // capability names, in the order they were required
	Because string   // what asked for them
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("adapter %s does not declare %s (required by %s)",
		e.Kind, strings.Join(e.Missing, ", "), e.Because)
}

// Is makes [errors.Is] match [ErrCapability].
func (e *CapabilityError) Is(target error) bool { return target == ErrCapability }

// PlatformError reports that an adapter does not run on this machine.
type PlatformError struct {
	Kind      string
	Platforms []Platform
}

func (e *PlatformError) Error() string {
	names := make([]string, len(e.Platforms))
	for i, p := range e.Platforms {
		names[i] = p.String()
	}
	return fmt.Sprintf("adapter %s does not run on this host; supported: %s",
		e.Kind, strings.Join(names, ", "))
}

// Is makes [errors.Is] match [ErrPlatform].
func (e *PlatformError) Is(target error) bool { return target == ErrPlatform }

// Sentinel errors. Adapters wrap these so the run loop can route an outcome
// without knowing which vendor produced it.
var (
	// ErrNotSupported — the adapter cannot do this at all. Not a failure of
	// the run; a statement about the adapter.
	ErrNotSupported = errors.New("adapter: not supported")

	// ErrCapability — a fail-closed capability refusal. Match with
	// [errors.Is]; the value is a [*CapabilityError].
	ErrCapability = errors.New("adapter: required capability not declared")

	// ErrPlatform — the adapter does not run on this host.
	ErrPlatform = errors.New("adapter: unsupported platform")

	// ErrPreflight — the local machine is not ready: the binary is missing,
	// the login has expired, the version is wrong. Wrap it with something a
	// human can act on.
	ErrPreflight = errors.New("adapter: preflight failed")

	// ErrNotPlanAuth — the session would not run, or did not start, on the
	// developer's plan login. Match with [errors.Is]; the value is a
	// [*PlanAuthError].
	ErrNotPlanAuth = errors.New("adapter: not on the plan login")

	// ErrSessionClosed — an operation was attempted on a session that has
	// already ended.
	ErrSessionClosed = errors.New("adapter: session closed")
)

// APIAuthError is a refusal of hosted API-key billing.
type APIAuthError struct{ Because string }

func (e *APIAuthError) Error() string        { return "not on an API key: " + e.Because }
func (e *APIAuthError) Is(target error) bool { return target == ErrNotAPIAuth }

var ErrNotAPIAuth = errors.New("adapter: not on an API key")
