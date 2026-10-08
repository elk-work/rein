# The Grok Build adapter

**`ark:rein#6 (01M17TTZMNZC4VKZKWBZSCP2GF)`. Package `internal/adapter/grok`.**

Drives xAI's **Grok Build** through `grok agent stdio`, its implementation of
the [Agent Client Protocol](https://agentclientprotocol.com). Read
[`docs/adapters.md`](./adapters.md) first — this file only records what is true
of Grok.

This is Grok Build, xAI's official CLI, **not** `@vibe-kit/grok-cli`, which is
an unrelated tool with a confusingly similar name (elk `docs/rein.md` §3).

Everything below was checked against `grok 1.0.13` on darwin/arm64 with a
cached grok.com login, on **2026-08-29**. Where a claim is specification rather
than observation it says so — and two of them are, which is why two
capabilities are declared `partial`.

## Why ACP and not `grok -p --output-format streaming-json`

`streaming-json` prints the same ACP session updates and nothing else: it is
one-way. There is no channel for an approval, a cancel, or a resume, and a
runner that cannot cancel a run is not a runner. `grok agent stdio` is the same
event stream with a JSON-RPC connection around it.

## The wire

```
grok agent [--model <id>] [--reasoning-effort <level>] stdio     # cwd = RunSpec.WorktreeDir
```

Newline-delimited JSON, one object per line, JSON-RPC 2.0 with `"jsonrpc"`
present on every frame — unlike Codex, which omits it. This client ignores the
member anyway; validating it buys nothing and breaks against a vendor that
leaves it out.

A request and a response are told apart by `method`, not by the id: ACP lets
both sides originate requests, so the id spaces are independent.

Agent-level flags go **after `agent` and before the transport name** —
`grok agent --model grok-4.6 stdio` — which is the vendor's own documented rule
and easy to get backwards.

The handshake: `initialize` (protocolVersion 1) → `session/new` or
`session/load` → `session/prompt`. `Start` returns once the session exists;
the prompt runs on its own goroutine.

**Plan login only.** `initialize`'s `_meta.defaultAuthMethodId` must be
`cached_token` (the grok.com sign-in the private `GROK_HOME` links in) or
`grok.com`; anything else, or nothing, stops the session before
`session/new` with `adapter.ErrNotPlanAuth` (`planauth.go`). `Start` also
refuses a spec whose `Env` carries `XAI_API_KEY`. A queue's `model` and
`effort` become `--model` and `--reasoning-effort`.

**The terminal signal is a reply, not a notification.** `session/prompt` is a
request that does not return until the turn is over, and its result carries
both the stop reason and the turn's token totals. Every session update arrives
between that request and its answer — which is also why the recorded-frame
tests have to replay in request order rather than in file order; see
`testdata/README.md`.

## Permission modes

| Rein mode | How | Usable |
|---|---|---|
| `read_only` | `GROK_SANDBOX=read-only` on the child process, plus `_meta.yoloMode` | yes |
| `full` | `_meta.yoloMode: true` | yes |
| `ask` | — | **no**: fails closed on `approvals: partial` |
| `accept_edits` | — | **no**: same |

`read_only` auto-approves *inside a sandbox*, and the reasoning matters. With
`approvals: partial` nobody has promised to answer a permission request, so a
session that waits on one hangs. The kernel-enforced sandbox is what makes the
mode read-only; auto-approving inside a jail that permits no writes is safe in
a way auto-approving without one is not.

**The silent-downgrade guard.** Grok's own documentation says a built-in
profile that fails to apply "warns and continues without enforcement". A
`read_only` session that continues without enforcement is a `read_only` session
that can write. The adapter reads the agent's stderr, and `Start` fails rather
than running unprotected.

`AllowedTools`/`DeniedTools` are refused with `ErrNotSupported`: `--allow` and
`--deny` are flags on the interactive `grok` command, not on `grok agent`, and
ACP has no per-session tool policy.

## Capabilities

| Capability | Declared | Because |
|---|---|---|
| `git` | yes | `run_terminal_command` ran `git status`; verified |
| `worktree` | **partial** | see below |
| `file_edit` | yes | the `write` tool created a file; verified |
| `shell` | yes | verified |
| `mcp` | yes | a Rein-supplied server reached `session/new`; verified |
| `resume` | yes | `session/load` across two processes; verified |
| `structured_events` | yes | the update stream below |
| `approvals` | **partial** | see below |
| `steer` | **no** | ACP has no mid-turn steer; a follow-up is a new session against the same id |

### `steer: no` — and what that costs

`Session.Send` returns `ErrNotSupported`, because ACP has no mid-turn steer:
a follow-up is `session/prompt` on a new turn, which is a different thing from
correcting the turn that is running. The declaration is what `rein attach`
reads, so attaching to a grok run is **watch-only and says so before the person
types**, rather than accepting a line and dropping it.

Lifting it means either finding a real steer in ACP or deciding that "queue
the next turn" is close enough — and it is not, because a person attaches
precisely when the current turn is going wrong.

### `approvals: partial` — the finding, not a shortcut

The handler is written, to the ACP specification:
`session/request_permission` → `EventPermissionRequest` → `Respond` →
`{"outcome": {"outcome": "selected", "optionId": …}}`. What is missing is
evidence that it ever runs.

In four probes — a file write, a shell command, a network command, and a write
under a sandbox — **Grok never sent one**. Every permission gate resolved
*inside the agent*: an `_x.ai/session_notification` with
`sessionUpdate: "pending_interaction"`, `kind: "permission"`, followed
milliseconds later by `interaction_resolved` for the same tool call, with no
client round trip. The likely cause is that Grok loads the developer's own
permission rules — `grok inspect` reported 64 of them from a
`.claude/settings.json` — and had already allowed everything asked of it.

So Rein cannot honestly claim to *control* Grok's approvals. `partial` fails
closed exactly like `no`, which makes `ask` and `accept_edits` unusable on
grok, which is the correct outcome of that fact rather than a limitation of the
code.

**`ark:rein#14 (01M17ZQJ5Z7X41P9ADXR5T1QQR)`** tracks promoting it.
`TestIntegrationApprovalIsNeverAsked` fails loudly if a request ever arrives —
deliberately, because that day is good news and should not pass unnoticed.

### `worktree: partial` — enforced in one mode, not the other

`read_only` is enforced by Grok's own OS sandbox. `full` runs with no sandbox,
and ACP exposes no per-session filesystem bound, so in `full` the agent can
reach outside the directory it was given.

The fix is small — map `full` onto `GROK_SANDBOX=workspace`, which is precisely
"write to CWD and the temp dirs" — and it is **`ark:rein#15
(01M17ZQJ6DFAFGMEE3JVQYS1KS)`**, blocked on **`ark:rein#16
(01M17ZQJ6RVRMTFXCFXFKQQ9GD)`**: no `GROK_SANDBOX` profile currently applies on
the maintainer's Mac, because a hook source path under `~/.grok/hooks` is a symlink and
Grok refuses to start with its protections missing. Making `full` depend on a
sandbox before that is fixed would make grok unusable on the only machine we
have. (That refusal is Grok failing closed, and it is the right behaviour;
`ark:rein#16` has the one-line local fix.)

Note the consequence for `read_only` today: it does not work on that machine
either, for the same reason. `full` is unaffected.

### `mcp: yes`, with a shape constraint

A Rein-supplied server reached `session/new` and was counted in the agent's
own tally — `_x.ai/mcp/init_progress` reported three servers where the
developer's config has two.

But `initialize` reports `mcpCapabilities: {http: true, sse: true}` and **no
stdio**. `RunSpec.MCPServers` is name-keyed and opaque; the adapter converts it
to the array ACP wants and **rejects an entry that is neither http nor sse**,
naming the server. A stdio entry otherwise earns a bare `Invalid params` in the
middle of the handshake, which is a bad way to find out.

The developer's own `~/.grok/config.toml` servers used to load as well. They no
longer do — see **Isolation** below, which is the most important thing in this
file.

## Isolation: a private `$GROK_HOME` per session

**A session sees only the packet's MCP servers.** It gets a `GROK_HOME` of its
own, built at `Start` and removed when the session ends.

This adapter is the one the incident happened to. On 2026-08-29 a dogfood run
(`arun-18298891`) **submitted its own deliverable**: Grok found the Elk MCP
connector in `~/.grok/config.toml` and called `submit_deliverable` under the
developer's identity, mid-run. Elk reviewed that submission, Rein submitted
again, and the run ended in a state the review sweep treats as already
reviewed, so it was never re-reviewed and could not be accepted
(`ark:rein#21`).

**There is no flag for this.** What was checked, against `grok 1.0.13`:

- `grok agent --help` has no MCP switch at all;
- ACP's `session/new` `mcpServers` is **additive** here — a probe passing one
  server watched the agent's tally go from two to three, not to one;
- `GROK_CONFIG` is allowlisted to `models`, `features`, a narrowed `toolset`
  and `shell_environment_policy`, and the documentation says outright that it
  cannot add a discovery source; `mcp_servers` is not on the list either way;
- `disabled_mcp_servers` and `[mcp_servers.<name>].enabled` both need the
  names, which means reading the developer's config and hoping nothing changes
  mid-run.

`GROK_HOME` is the switch that exists, and `_x.ai/mcp/servers_updated` now
arrives empty.

**What is carried across**, by symlink: `auth.json` (the login — verified, a
private home with only this link still reports the account logged in, and
without it reports "You are not authenticated") and `sessions/` (session state,
so `session/load` still resumes).

**Foreign configuration imports are disabled.** `GROK_HOME` isolates native
configuration, while the adapter sets all ten confirmed Claude/Cursor switches
to `0`: `GROK_{CLAUDE,CURSOR}_{MCPS,HOOKS,AGENTS,RULES,SKILLS}_ENABLED`.
These are individually documented in Grok's own
[configuration reference](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/26-config-reference.md#compat).
They suppress foreign MCP, hook, instruction, rule, and skill scanners without
disabling Grok's native hooks. Rein's private StopFailure hook remains installed.
Isolation variables override both inherited and caller-supplied environment
values. Unit tests verify the assembled environment and execute Rein's hook on
synthetic failure payloads; this does not prove a live CLI turn's behavior.
Codex hook/skill compatibility settings appear in the reference, but equivalent
environment switches are not documented there and are not set.

**A side effect that helps.** The private home gets its own real, empty `hooks/`
directory and `hooks-paths` file. Grok's sandbox refuses to start when a hook
source path has a symlink component, and the developer's `~/.grok/hooks` has
one on this machine — which was one of the two causes behind `ark:rein#16`. A
run no longer trips it. (The other cause, a symlinked `/var/run/docker.sock`,
is unaffected and still blocks `read_only` here.)

**If Grok rotates its login mid-session**, the private home is left in place and
the session emits a progress event naming the path, for the same reason as the
Codex adapter: deleting it would log the developer out, and copying it back
would mean this package handling a token.

**One residual.** `context7` still reaches the session — a plugin-contributed
server discovered from the Claude plugin directory under the real `$HOME`, which
`GROK_HOME` does not move. It is a public documentation lookup with two
read-only tools, no credential in its URL and no identity; `ark:rein#22` tracks
closing it, and records the options (moving `HOME` itself, or writing
`[plugins] disabled` into the private home).
`TestIntegrationSessionSeesOnlyTheSpecsMCPServers` asserts that no
config-derived server reaches a real session, and that the agent does not report
having an Elk tool.

## Rate limits: the `StopFailure` hook (`ark:rein#40`)

Grok exposes **no subscription window** — nothing on the ACP stream, no usage
read, nothing in `_meta`. The one signal is after the fact: a turn that ends on
an API error fires the `StopFailure` hook, and its payload's `error` is the
runtime's own classification — `rate_limit` (capacity errors classify as it
too), `authentication_failed`, `invalid_request`, `server_error`,
`max_output_tokens`, `unknown`.

So every private home gets exactly one hook of Rein's own,
`hooks/rein-stop-failure.json`, a `StopFailure` command hook that appends the
payload (`cat >>`) to `rein-stop-failure.jsonl` in the same home. When the turn
ends the session closes stdin, gives Grok up to 3 s to exit — it flushes queued
turn-end hooks at teardown — reads the log, and reports
`RateLimit{Exhausted: true}` through `Telemetry` if any report says
`rate_limit`. With no report at all it falls back to the error text (`429`,
`rate limit`, `too many requests`, `usage limit`); a report of another kind wins
over the text. `ExhaustedUntil` stays zero — Grok never says — so the run loop
holds the queue until the person's `weekly_reset`, or 24 hours.

**Verified under ACP**, grok 1.0.13, 2026-09-29:
`TestIntegrationTurnEndHookFiresUnderACP` subscribes the same hook to `Stop`
(which fires on every completed turn; a real rate limit cannot be produced on
demand, and Grok falls back to its default model rather than failing on an
unknown one) and sees `global/rein-stop-failure:stop[0].hooks[0]` run and its
payload land in the log. Hooks under `$GROK_HOME/hooks/` are the global scope,
which Grok always trusts.

The home's path is resolved (`/var` → `/private/var` on macOS) before it is
used, because a hook source path with a symlink component is what Grok's
sandbox refuses to start with (`ark:rein#16`). **No hook on Windows**: the
command is POSIX shell, so a Windows runner has only the text fallback.

## Events

| Grok | Rein event | Notes |
|---|---|---|
| `agent_message_chunk` | accumulated → `text` on `response_completed` | one event per token would be useless; the whole message is the unit |
| `agent_message_chunk` / `agent_thought_chunk` | `progress`, coalesced | at most one per 5 s, so a long turn is not silence to the watchdog |
| `user_message_chunk` | — | Rein's own prompt echoed back |
| `tool_call` | `tool_use` | `toolCallId`, title, `rawInput` |
| `tool_call_update` (with a status) | `progress` | |
| `response_completed` | `text` + `usage` | one model call finished |
| `turn_completed` | `progress` | the reply to `session/prompt` is the real terminal |
| `last_turn_summary` | `progress` | Grok's own one-line account; the fallback `Result.Summary` |
| `pending_interaction` | `progress` | **the gate that never became a request** — see above |
| `interaction_resolved` | `progress` | |
| `_x.ai/sessions/changed` `activity: idle` | `idle` | the agent's own account of what it is doing |
| `session/request_permission` | `permission_request` | unverified; decided by the mode in `read_only`/`full` |
| anything else | `progress` with `Raw` | never a new kind |
| the `session/prompt` reply | `done` / `error` | terminal |

### Usage, and the normalisation that makes it add up

Grok reports tokens in two shapes and they do not agree by accident.

- Per model call, on `response_completed`, **snake_case**: `input_tokens`,
  `output_tokens`, `cache_read_input_tokens`, …
- Per turn, on the `session/prompt` reply, **camelCase**: `inputTokens`,
  `cachedReadTokens`, …

The per-response `input_tokens` **excludes** the cached read; the turn total
**includes** it. Summing the raw field would under-report a cached turn by an
order of magnitude — in the recorded fixture, 5129 against a true 33417. So the
adapter adds the cached read back into each increment. That makes the
increments sum to the turn total exactly, and matches how the Codex adapter
reports input tokens (Codex's `inputTokens` includes its `cachedInputTokens`
too), so a consumer comparing two agents is comparing the same quantity.

`Result.Usage` is the vendor's own turn total where the reply carries one,
because that figure is authoritative and the increments are reconstructed.

## Resume, and the trap in it

`session/load` works across processes: a second `grok agent stdio` can load a
session id the first one created and the model remembers what it was told.
Verified.

**It replays the entire prior conversation as `session/update` notifications
before it returns.** That is ACP working as specified and it is a trap for a
translator: replayed history is not news. Emitting it would re-report every
message the run loop already saw, and would leave the replayed text in the
buffer that becomes this turn's summary. The adapter mutes the translator for
the duration of the call and emits one progress event saying how much it
skipped.

## `Send` is refused

ACP has no mid-turn steer. A second `session/prompt` is *queued* as another
turn — Grok says so on `_x.ai/queue/changed` — which is a different thing from
answering the turn in flight, and would give one session two terminal signals.
`Send` returns `ErrNotSupported`, which is the contract's own provision for an
adapter that cannot take input after start. A follow-up is a new session
against the same session id.

`Interrupt` is ACP's `session/cancel`, a notification; the acknowledgement is
the prompt call returning with `stopReason: "cancelled"`. Killing the process
is the backstop.

## Preflight

`grok models` — it asks xAI what models the account has, prints "You are
logged in with …", and returns in about a second. It reads no credential;
that is Grok's job, on the developer's own login.

The binary is looked up on `PATH`, falling back to `~/.grok/bin/grok` for a
daemon started from a shell that never sourced a profile. The fallback applies
only to the default binary name, so a configured `grok-nightly` that is missing
fails instead of quietly running stock `grok`.

One oddity worth knowing: on the recording machine `grok --version` printed
`1.0.5` while the running agent reported `agentVersion 1.0.13` — the launcher
had auto-updated underneath the symlink. **Trust the agent's own
`initialize._meta.agentVersion`, not `--version`.**

## Tests

`replay_test.go` drives the adapter against `testdata/*.jsonl`, verbatim
recordings of two real sessions, with the script derived from the recording.
See `testdata/README.md` for the ordering rule and what may not change in a
re-recording.

`integration_test.go` is behind `//go:build integration`:
`go test -tags integration ./internal/adapter/grok/`. Not in the PR gate — it
needs a login and spends the developer's own quota. Run it after a `grok`
upgrade.
