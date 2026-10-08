# The adapter contract

**`ark:rein#3 (01M17TTZJQ6YKPG2PTX6EJM5FE)`. Package `internal/adapter`.**

An adapter is the seam between Rein's run loop and one coding agent. The run
loop is written once against the `Adapter` interface and never learns that
Claude Code speaks stream-json while Codex speaks JSON-RPC and Grok speaks ACP.

This document is what Lane B (the Claude, Codex and Grok adapters —
`ark:rein#4`–`#6`) and the run-loop lane (`ark:rein#7`) build against. Where
the code and this file disagree, the code is right and this file is a bug.

## Ownership

| Path | Owner | Holds |
|---|---|---|
| `internal/adapter/` | this contract | the interface, the capability vocabulary, the registry. **No vendor knowledge at all.** |
| `internal/adapter/<kind>/` | one adapter each | everything about one vendor: its flags, its event shapes, its login check |
| `internal/adapter/fake/` | tests + the run-loop lane | a scriptable test double |

An adapter package owns its own directory and nothing else. Adding a
capability to the vocabulary, changing an event kind, or changing the
interface is a change *here*, and it lands as its own PR — not folded into an
adapter.

A binary decides which agents it can drive by which packages it imports:

```go
import (
    _ "github.com/elk-work/rein/internal/adapter/claude"
    _ "github.com/elk-work/rein/internal/adapter/codex"
)
```

Each package's `init` calls `adapter.MustRegister`. That import is the whole
wiring, and it is why `adapter.Kinds()` can honestly answer "what can this
build drive".

## The lifecycle

```go
a, err := adapter.Get(queue.AgentKind)          // registry
if err := adapter.CheckSupport(a.Manifest(), spec); err != nil {
    // fail closed — report the run stuck. Create NO worktree.
}
if err := a.Preflight(ctx); err != nil { … }    // binary present, logged in
sess, err := a.Start(ctx, spec)                 // one agent process
for ev := range sess.Events() { … }             // watch structured events
res, err := sess.Wait()                         // terminal result
```

The order matters. `CheckSupport` runs **before** `git worktree add`: a run
that cannot be served must produce no partial work, no orphan worktree, and
nothing to reap.

`Events()` must be drained. An adapter that cannot deliver an event blocks,
and a run loop that stops reading stalls the agent it is watching.

## The interface

```go
type Adapter interface {
    Name() string                        // the agent kind; == Manifest().Kind
    Manifest() Manifest                  // must not vary between calls
    Preflight(ctx context.Context) error // local readiness; wraps ErrPreflight
    Start(ctx context.Context, spec RunSpec) (Session, error)
}

type Session interface {
    ID() string                          // the vendor's session id
    Events() <-chan Event                // closed after exactly one terminal event
    Send(ctx context.Context, input string) error
    Respond(ctx context.Context, resp PermissionResponse) error
    Interrupt(ctx context.Context) error
    Wait() (Result, error)               // idempotent
}
```

Obligations an adapter cannot negotiate:

- **Never downgrade silently.** `Start` rejects a spec it cannot honour — an
  unsupported permission mode, a resume it cannot do, a timeout it cannot
  enforce. Quietly weakening a permission mode is the single failure this
  contract exists to prevent.
- **Never touch a credential.** `Preflight` checks that the vendor login
  exists. It does not read, mint, refresh or replay a token. Rein shells out
  to the vendor binary on the developer's own login, and that rule is what
  keeps the whole design defensible (elk `docs/rein.md` §3).
- **Exactly one terminal event**, `EventDone` or `EventError`, then the
  channel closes. The terminal event carries the same `Result` that `Wait`
  returns, so a consumer reading events and a consumer calling `Wait` never
  disagree.
- **A failed session sets both** a non-succeeded `Result.Status` and a non-nil
  error from `Wait`, so a caller inspecting either one learns the truth.
- **Events, never scrollback.** `EventIdle` comes from a real signal — Claude
  Code's `idle_prompt` / `agent_needs_input` notification hooks, an ACP
  update — not from scraping "esc to interrupt" out of a terminal. An adapter
  that cannot do this declares `structured_events: no` and is refused.

## Capabilities

The vocabulary is **closed**. Nine names:

| Capability | Means |
|---|---|
| `git` | the session can run git in its worktree |
| `worktree` | the session stays inside the directory it was given |
| `file_edit` | the session can create, modify and delete files |
| `shell` | the session can run arbitrary shell commands |
| `mcp` | the session can be given MCP servers — how a run reaches Elk from inside the agent |
| `resume` | a session can be resumed by id after the process exits |
| `structured_events` | the adapter reports a machine-readable stream, not scrollback |
| `approvals` | the session raises `permission_request` and honours `Respond` |
| `steer` | the session takes text after it starts, through `Send` — what makes `rein attach` a conversation |

Each is declared `yes`, `partial` or `no`.

### Fail-closed matching

`Manifest.Satisfies(required []string) (missing []string)`. A requirement is
**missing** when it is any of:

- outside the vocabulary — an unknown name cannot be satisfied by anything, so
  a typo in an Elk packet stops the run instead of passing vacuously;
- undeclared in the manifest;
- declared `partial` or `no` — **these fail identically**;
- declared with a value outside the tri-state.

Only a declared `yes` satisfies. That is Loom's rule, borrowed unchanged from
`defaults/docs/runtime-adapters.md` §7 (rjwalters/loom, read 2026-08-29), and
it is the reason a manifest can honestly say `no` without the matcher treating
it as different from "close but gapped". Loom's own worked example is Codex:
`worktreeIsolation: "partial"` is what keeps Codex out of its Builder role,
with no matcher change needed.

Two deliberate differences from Loom:

- **Rein keys on names, not a fixed struct of five fields.** Requirements
  arrive from Elk as `required_capabilities`, a names-only list (elk
  `docs/rein.md` §2), so the matcher has to cope with a name it has never
  seen — and the honest answer to one is a refusal.
- **Rein manifests declare platforms.** Loom is Unix-only by construction;
  Rein's Phase 1 is a Windows box. An empty platform list supports nothing.

`Satisfies` takes `[]string` rather than `[]Capability` for exactly that
reason: the point is to catch a name this build has never heard of.

### Declaring honestly

Declare every capability you have an opinion about, including the ones you
declare `no`. An omission and a `no` fail identically, but only one of them
reads as a decision. Use `partial` when a real mechanism exists but is
incomplete or unverified, and write down what is missing in `Manifest.Notes` —
the manifest is what is enforced, the note is what a human reads when they ask
why.

`Manifest.Validate` rejects a manifest that declares a name outside the
vocabulary, so a misspelling fails at process start, where someone is watching,
rather than at dispatch, where nobody is.

## Permission modes

Rein's four modes; each adapter maps them onto its vendor's own flags. The
zero value `PermissionUnset` is **not valid** — `claude -p` starts in Manual
mode on every plan and hangs on its first edit (elk `docs/rein.md` §3), so a
spec that forgot to say is a bug, not a default.

| Mode | Means | Needs `approvals` |
|---|---|---|
| `read_only` | reads and reasons, mutates nothing | no |
| `ask` | every mutation raises `permission_request` | **yes** |
| `accept_edits` | file edits auto-approved; shell and network still ask | **yes** |
| `full` | everything auto-approved | no |

`full` is only ever for a worktree of a repository we own: `-p` skips the
workspace-trust dialog, so a queue-driven daemon runs a repo's hooks and MCP
servers unprompted.

`CheckSupport` refuses `ask` and `accept_edits` on an adapter that does not
declare `approvals: yes`, and `Session.Respond` returns `ErrNotSupported` on
one — the manifest and the interface agree, and the check happens before the
worktree exists rather than in the middle of a run.

Known mapping targets, from the research in elk `docs/rein.md` §3 — Lane B
owns getting these right:

| | Claude Code | Codex | Grok Build |
|---|---|---|---|
| headless entry | `claude -p --output-format stream-json --verbose` | `codex app-server` (JSON-RPC) | `grok agent … stdio` (ACP) |
| permissions | `--permission-mode` + `--allowedTools` | `-s {read-only,workspace-write,danger-full-access}` — **not** `--full-auto`, which the binary rejects | ACP; `streaming-json` is read-only |
| resume | `--resume <id>` | `codex exec resume` | `--resume` |

## Events

`Event` is a tagged union: read `Kind`, then the field it populates. The set is
closed — an adapter seeing something Rein has no kind for reports
`EventProgress` with the vendor payload in `Event.Raw` rather than inventing a
kind.

| Kind | Field | Notes |
|---|---|---|
| `text` | `Text` | assistant text |
| `tool_use` | `Tool` | the vendor's tool name and arguments, verbatim |
| `permission_request` | `Permission` | answer with `Respond`, or the session waits |
| `question` | `Text` | the agent needs a human → Elk's `ask_elk`; answer with `Send` |
| `idle` | — | the watchdog's input |
| `progress` | `Text` | anything else worth relaying → `report_progress` |
| `usage` | `Usage` | an increment, not a running total |
| `done` | `Result` | terminal |
| `error` | `Err` | terminal |

`Seq` counts from 1 without gaps, so a consumer can tell a dropped event from a
quiet agent. `Raw` carries the vendor's own event for the transcript and is
never interpreted by the run loop.

`Usage` reports counts and never money: Elk prices server-side from
`model-rates.json` (elk `docs/rein.md` §2).

A `question` and a `permission_request` are different things. One is about the
work, the other is about authority, and they go to different places in Elk.

## Telemetry — an optional interface, not a tenth event kind

A session can also describe **its own state**: the model actually in use, how
full the context is, the subscription windows, the MCP inventory. That is not
an event and does not get a kind, for two reasons.

The event set is closed, and these are facts about a session rather than things
that happened in it — a consumer would have to replay a whole run to learn the
current value of one number. And a method on `Session` would be a method every
adapter must implement, which for codex and grok means implementing most of it
badly: their vendors publish little of this.

So it is an optional interface. The run loop type-asserts and asks nothing of a
session without it.

```go
type Telemeter interface{ Telemetry() SessionTelemetry }
```

Implemented by all three since `ark:rein#40`, each answering only what its
vendor says: **claude** nearly everything; **codex** the model and the
subscription window from `account/rateLimits/updated`; **grok** the model and
whether the turn ended on a rate limit. Everything in `SessionTelemetry` is a
pointer or an empty-means-absent string, because **the absence of a measurement
and a measurement of zero are different claims**: a session at 0% context fill
and a session whose adapter never learned the number must not look the same on
a fleet page. `MCPServers` is a pointer to a slice for exactly that reason —
`nil` is "the vendor has not said", `&[]` is "it has, and there are none", and
the second is the true and important answer for a Rein-driven run.

`Telemetry` may be called from any goroutine at any time, including before the
first event and after the session has ended. It returns the best it knows and
never blocks. An implementation that keeps this state in a single-goroutine
decoder must publish a copy under a lock — see `claude/session.go`.

### Subscription exhaustion — the adapter's verdict

`RateLimit` relays the vendor's own words (`Status`, `Type`, the `Windows`),
and then gives the **adapter's reading** of them in one vocabulary for every
vendor: `Exhausted` — the vendor has said this account cannot serve a request
now — and `ExhaustedUntil`, zero when the vendor did not say. The run loop
stops claiming on `Exhausted` and never learns that Claude says `rejected`
where Codex says `rateLimitReachedType` (`internal/runner/subscription.go`).
Set it on the vendor's word only, never on a percentage judged close enough.

An adapter that can read its window **without spending a turn** also
implements

```go
type HeadroomReader interface {
    ReadHeadroom(ctx context.Context) (*RateLimit, error)
}
```

and an idle queue calls it every 30 minutes. Only **codex** has it
(`account/rateLimits/read`). Claude's window is only on the stream of a request
it is already making, and Grok exposes none.

## MCP servers — translated per vendor, allow-listed per queue

`RunSpec.MCPServers` is opaque here, in each vendor's own shape. What fills it
is the queue's allow-list (`internal/runner/mcp.go`, `ark:rein#40`): servers
defined once in config.toml in a neutral `MCPServerSpec` — a `URL` with an
optional `BearerTokenEnvVar`, or a `Command`, `Args` and `EnvVars` — and each
adapter implements

```go
type MCPConfigurer interface {
    MCPServerConfig(s MCPServerSpec) (any, error)
}
```

to express one in its vendor's configuration, or refuse it with
`ErrNotSupported`. Secrets travel as environment variable **names** only; the
vendor binary reads the values. A refused server is neither passed nor
declared, so the queue's `mcp:<name>` capabilities are exactly the servers its
runs get.

| | remote (`url`) | bearer token | local (`command`) |
|---|---|---|---|
| claude | `{"type":"http"}` | header `Bearer ${VAR}`, which Claude Code expands | `{"type":"stdio"}`, `env: {VAR: "${VAR}"}` |
| codex | `{url}` | `bearer_token_env_var` | `{command, args, env_vars}` |
| grok | ACP `{"type":"http"}` | refused — ACP headers are literal | refused — no stdio in `grok agent` |

## Errors

| Sentinel | Means |
|---|---|
| `ErrNotSupported` | the adapter cannot do this at all — a statement about the adapter, not a failed run |
| `ErrCapability` | a fail-closed refusal; the value is a `*CapabilityError` carrying `Missing []string` |
| `ErrPlatform` | the adapter does not run on this host; the value is a `*PlatformError` |
| `ErrPreflight` | the local machine is not ready — wrap it with something a human can act on |
| `ErrSessionClosed` | an operation on a session that already ended |

`CapabilityError.Error()` is written to be pasted straight into a run's `stuck`
reason: it names the adapter, the missing capabilities in the order they were
required, and what asked for them.

## Writing an adapter

1. `internal/adapter/<kind>/<kind>.go`, package `<kind>`.
2. A `Manifest` with `Kind`, `Binary`, every capability you have an opinion
   about, and the platforms you have actually run on. `partial` and `no` are
   fine and honest; `yes` is a claim someone will rely on.
3. `Preflight` — binary on PATH, login present, version understood. Wrap
   `ErrPreflight` and say which of those failed.
4. `Start` — call `spec.Validate()` and `adapter.CheckSupport(m, spec)` first,
   then launch. Re-checking what the run loop already checked is cheap; the run
   loop checking and the adapter assuming is how a mismatch reaches a worktree.
5. `init() { adapter.MustRegister(New()) }`.
6. Tests against `internal/adapter/fake` for the loop, plus your own for the
   vendor's event translation.

`internal/adapter/fake` is the reference implementation of the *contract* —
it spawns no process, but it enforces every rule above, including refusing
`Respond` when its own manifest does not declare `approvals`. A run loop that
passes against the fake is exercising the same rules a real adapter enforces.
Trim its declared capabilities to test that your caller genuinely fails closed.
