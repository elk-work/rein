# The Claude Code adapter

**`ark:rein#4 (01M17TTZKBGWXMCZT74AWHNC4K)`. Package `internal/adapter/claude`.**

Drives Claude Code headlessly on the developer's own login. Everything Rein
knows about `claude` lives in this one package; the run loop is written against
[`docs/adapters.md`](./adapters.md) and never learns that Claude Code speaks
stream-json.

Everything below was **measured against `claude` 2.1.251 on darwin/arm64 on
2026-08-29**, not taken from documentation. The recordings are in
`internal/adapter/claude/testdata/` and the unit tests replay them. Where a
claim here disagrees with the binary, the binary is right and this file is a
bug — re-record and fix it.

## The command line

```
claude -p
  --output-format stream-json
  --input-format  stream-json
  --verbose
  --permission-mode <plan|acceptEdits|manual|bypassPermissions>
  --settings <tmp>/settings.json          # the watchdog hooks
  [--model <RunSpec.Model>]
  [--append-system-prompt <RunSpec.SystemPrompt>]
  [--allowedTools <RunSpec.AllowedTools…>]
  [--disallowedTools <RunSpec.DeniedTools…>]
  [--resume <RunSpec.ResumeID>]
  --mcp-config <tmp>/mcp.json --strict-mcp-config   # ALWAYS, even when empty
  --setting-sources project                         # ALWAYS
```

with `cwd` = `RunSpec.WorktreeDir`, and the environment being the daemon's own
with `RunSpec.Env` layered over it.

**The prompt is not on the command line.** It goes to stdin as a stream-json
message:

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"…"}]}}
```

That is the whole reason `--input-format stream-json` is always passed: an
argument prompt would work, but it would close the door on `Session.Send`, and
the contract wants a mid-run steer to be possible. That channel is what the
manifest declares as `steer: yes`, and it is what `rein attach` carries a
person's typing on — see the footgun below for the window it is open in.

**`--bare` is never passed, and never should be.** It disables subscription auth
outright — Anthropic credentials become "strictly `ANTHROPIC_API_KEY` or
apiKeyHelper", OAuth and keychain are never read. Passing it would silently move
the fleet onto metered API billing. For the same reason `Start` refuses a spec
whose `Env` sets any metered key (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`,
…), and a repository whose loaded settings (`--setting-sources`, `project` by
default) set `apiKeyHelper` or put such a key in their `env` block.

**The init line is the guarantee.** Under `-p`, Claude Code uses
`ANTHROPIC_API_KEY` whenever it is present, and a managed settings file can
supply a helper Rein never sees. So the first `system/init` line's
`apiKeySource` must be `"none"` — the subscription login — or the session is
killed (not interrupted: a graceful stop would let the turn keep spending)
before its first tool call, and fails with `adapter.ErrNotPlanAuth`
(`planauth.go`). An init line without the field is refused too.

`RunSpec.Model` and `RunSpec.Effort` — a queue's `model` and `effort` in
config.toml — become `--model` and `--effort`; empty passes neither.

### Permission modes

The vendor's `--permission-mode` accepts `acceptEdits`, `auto`,
`bypassPermissions`, `manual`, `dontAsk` and `plan`. Rein's four map like this:

| Rein | `claude` | Serviceable in v0? |
|---|---|---|
| `read_only` | `plan` | **yes** |
| `ask` | `manual` | no — needs `approvals` |
| `accept_edits` | `acceptEdits` | no — needs `approvals` |
| `full` | `bypassPermissions` | **yes** |

**v0 serves `read_only` and `full` only.** That is not a choice this package
made, it is what the shared gate does: `CheckSupport` refuses any mode whose
`NeedsApprovals()` is true when the manifest does not declare `approvals: yes`,
and this adapter declares it `partial`. The `ask` and `accept_edits` mappings
exist so that lifting the capability later is a one-line change, not a rewrite.

Three vendor modes are deliberately unused:

- **`manual`** is what an unconfigured `-p` run lands in, and it auto-denies.
- **`dontAsk`** *also* auto-denies anything not pre-allowed — measured, not
  assumed: a probe run refused both `Write` and `Bash` and created no file.
  The name suggests "proceed without asking"; it means "never ask, so deny".
- **`auto`** hands the decision to a classifier. That is not a policy Rein can
  report to Elk as the authority under which a run mutated something.

`bypassPermissions` needs **no** companion flag — not
`--allow-dangerously-skip-permissions`, not `--dangerously-skip-permissions`.
Verified: `--permission-mode bypassPermissions` alone wrote the file.

### Isolation from the developer's own configuration

**A run gets what its packet asked for and nothing that happens to be on the
laptop.** Two flags, both passed on *every* run:

```
--mcp-config <tmp>/mcp.json --strict-mcp-config   # MCP servers
--setting-sources project                         # hooks, plugins, skills
```

`RunSpec.MCPServers` is written to `<tmp>/mcp.json` as `{"mcpServers": {…}}` —
**and when the packet names no servers it is written as `{"mcpServers": {}}`,
not omitted.** `--strict-mcp-config` on its own is not what excludes the
developer's servers; it is strict about the config it is given, so it needs one.

This was an incident, `ark:rein#21`. The flags used to be conditional on the
packet naming a server, so an ordinary run inherited `~/.claude.json`. Measured
on this Mac, a bare `claude -p` session came up with **287 MCP tools, 43 of them
the Elk connector's**, authenticated as the developer. A grok run used exactly
that to post its own deliverable to Elk *under the developer's identity* before Rein
reported anything; the claude run of the same dogfood failed instead, on a
connector whose URL happened to be invalid. The dangerous half was never the
error — it was the run that worked.

`--setting-sources` covers what `--strict-mcp-config` does not. The default
keeps **`project`**, the settings checked into the repository being worked on,
and drops the two personal sources: `user` (`~/.claude` — the developer's hooks,
plugins and skills) and `local` (`.claude/settings.local.json`, gitignored). The
line is *the developer's machine state* versus *the repository's reviewed,
shared configuration*: a worktree of a repo we own legitimately carries hooks and
skills the run should honour. `Adapter.SettingSources` overrides it, and
`NoSettingSources` drops the repository's settings too.

Measured, all on 2.1.251:

| | baseline | `--strict-mcp-config` + empty | + `--setting-sources project` |
|---|---|---|---|
| MCP servers | 9 | **0** | 0 |
| `mcp__*` tools | 287 | **0** | 0 |
| Elk tools | 43 | **0** | 0 |
| plugins | 4 | 4 | **0** |
| developer's `SessionStart` hook | fires | fires | **does not fire** |

Four things were checked before settling on this, because each could have made
it the wrong mechanism:

- **This adapter's own `--settings` still applies.** `--settings` is not one of
  the sources, so the watchdog hooks survive — verified by the `Stop` hook still
  posting under both `project` and `""`.
- **The repository's own hooks still fire** under `project`, and stop under `""`.
  That difference is the whole reason `project` is the default.
- **`CLAUDE.md` still loads.** It is not a settings file, so the worktree's
  project instructions reach the agent either way — verified by asking a session
  for a magic word that only its `CLAUDE.md` knew.
- **Subscription login survives.** Auth is not a setting; every probe exited
  successfully on a subscription login.

`CLAUDE_CONFIG_DIR` was considered for this and rejected. It would isolate more,
but credentials live under the config directory on Linux
(`~/.claude/.credentials.json`), so redirecting it risks breaking the login the
whole design rests on — and it would fail differently on each platform, which is
the worst property a security control can have.

A side benefit worth knowing: a session that spawns no inherited MCP servers
starts markedly faster — the integration tests dropped from ~8s to ~3s.

## The watchdog

A loopback HTTP listener on `127.0.0.1:0`, and a generated `--settings` file
registering two hooks that POST their stdin to it:

```json
{
  "hooks": {
    "Notification": [{"hooks": [{"type": "command",
      "command": "curl -sS -m 5 -X POST --data-binary @- http://127.0.0.1:<port>/<token>"}]}],
    "Stop":         [{"hooks": [{"type": "command", "command": "…same…"}]}]
  }
}
```

**TCP on all three platforms, not a unix socket.** The brief allowed a socket on
Unix and TCP on Windows; one transport turned out to be the better trade. A unix
socket needs a helper on the far end — `nc -U` is not reliably present — and
macOS caps `sun_path` at 104 bytes, which a temp directory can exhaust on its
own. `curl` is on macOS, on Windows 10+ (`curl.exe` since 1803) and on all but
the most minimal Linux images. The URL carries a 128-bit random token the
handler checks, which closes the one thing a loopback port has that a socket
file does not: another local process posting fake events.

**`--settings` is additive, but the sources it adds to are now narrowed.**
`--settings` is not one of the setting *sources*, so this file applies whatever
`--setting-sources` says — which is what lets the watchdog survive the isolation
above. The repository's own hooks still run alongside it; the developer's no
longer do.

The recorded fixtures predate that change and still show a `SessionStart` hook
firing at the top of the stream — that is the developer's own hook, and a run
today would not fire it. The fixtures are kept as they were because they are
recordings of the vendor's *event shapes*, which is all the decoder tests read
them for.

### What actually fires, headless

| Hook | Fires under `-p`? | What Rein does with it |
|---|---|---|
| `Stop` | **yes** | recorded, not reported — see below |
| `Notification` | **no** | mapped anyway, for the day it does |

`Stop` arrives just before the `result` line with a genuinely useful payload:
`session_id`, `cwd`, `permission_mode`, `last_assistant_message`, and
`transcript_path` — the last of which the event stream never carries and which
becomes `Result.TranscriptPath`. It is **not** reported as an event: it fires
while the session is finishing normally, so an `idle` event there would tell the
run loop's watchdog that a completing session had stalled.

`Notification` never fired in any probe, and on reflection it cannot: every
notification it reports — permission prompts, idle prompts — is an *interactive*
state that a `-p` run never reaches. It is registered regardless, because it
costs nothing, it is the contract's named source of `EventIdle`, and it starts
working the day Rein drives an attached or `--bg` session.

**So the headless watchdog contributes the transcript path and nothing else, and
that is a supported outcome.** The stream is the primary signal; a session whose
hooks never fire runs normally. If `curl` is missing or the listener cannot
bind, `Start` carries on without `--settings` rather than failing.

## Event mapping

The vendor's NDJSON, line by line:

| `claude` line | Rein event | Notes |
|---|---|---|
| `system` / `init` | `progress` | captures the session id, model and version |
| `system` / `hook_started` | *dropped* | the developer's own hooks fire every session |
| `system` / `hook_response` | *dropped* when `outcome: success`, else `progress` | |
| `system` / `permission_denied` | `progress` | **not** `permission_request` — see below |
| `system` / anything else | `progress` | e.g. `thinking_tokens` |
| `assistant` → `text` block | `text` | |
| `assistant` → `tool_use` block | `tool_use` | name, id and arguments verbatim |
| `assistant` → `thinking` block | *dropped* | reasoning is not a work record |
| `assistant` → `message.usage` | `usage` | an increment, with the model |
| `user` → `tool_result` block | `progress` | Rein has no `tool_result` kind; the correlation id is in the text and in `Raw` |
| `rate_limit_event` | `progress` | window utilisation — a fleet starving the developer is the named risk in elk `docs/rein.md` §3 |
| `result` | `done` / `error` | **terminal**, and only the first one |
| anything unrecognised | `progress` | with the payload in `Raw` |

A trivial session reads:

```
#1 progress  claude session <id> started (model claude-fable-5, permission mode plan, version 2.1.251)
#2 text      REIN OK
#3 usage     in=2 out=1 claude-fable-5
#4 progress  rate limit allowed, five_hour 58%, seven_day 70%
#5 done      succeeded
```

**Usage totals do not come from summing the increments.** `EventUsage` reports
each message's own counts, but the per-message numbers repeat cache reads across
iterations, so `Result.Usage` is taken from the `result` line's own `usage`
object — the number the vendor would bill. Counts only, never money: Elk prices
server-side from `model-rates.json`.

**There is no `EventQuestion` in v0.** Headless Claude Code has no structured way
to say "I need a human" — there is no `AskUserQuestion` tool in this build's tool
list, and a turn that ends in a question is indistinguishable from one that ends
in an answer without reading the prose. Inferring one from the summary text
would be scraping, which the contract forbids. A question therefore arrives as an
ordinary `done` whose summary happens to ask something, and it is the run loop's
call what to do with it.

## Subscription windows (`ark:rein#40`)

`rate_limit_event` carries `status` (`allowed`, `allowed_warning`, `rejected`),
`resetsAt` for the binding window, `rateLimitType`, the overage state and every
window in `unifiedWindows`. The adapter reads **`rejected` as exhausted** —
unless extra usage is serving the request (`isUsingOverage`, `overageInUse`, or
an overage status that still allows it) — until `resetsAt`, or, without one,
the latest reset among the windows at 100%. `allowed_warning` is not exhausted:
close is the router's problem. A failed result reading `Claude AI usage limit
reached|<unix seconds>`, which older builds sent instead of a rejected event,
counts too.

**There is no idle read.** The window is only on the stream of a request Claude
Code is already making. `/usage` is a separate endpoint that is itself
rate-limited, and the cheapest call that would produce a `rate_limit_event` is
still a real turn billed to the window it measures. So a Claude queue's reading
is carried forward from its last run, with `sampled_at`, and nothing is spent
to refresh it; a window whose own reset has passed is left out of the idle
reading rather than shown full.

## Footguns

**`result` is a turn boundary, not the end of the session.** This is the one that
will bite anyone who edits this package. Under `--input-format stream-json` the
process stays alive after emitting `result`, waiting for another stdin message —
a two-turn recording with two `result` lines is in `testdata/multiturn.jsonl`.
The adapter therefore treats the **first** `result` as terminal, closes stdin,
and reaps the process. That is what keeps one Rein run equal to one Claude Code
session. The consequence for callers: `Send` is a *mid-run steer* only, and
returns `ErrSessionClosed` afterwards. It is also the window `rein attach` has:
a person can steer the turn that is running, and once it ends the run submits
as it always would. Attaching to a claude run that has already finished its
turn can watch and cannot type — `docs/attach.md`. A genuine follow-up is a new session
started with `ResumeID`, which is what `resume: yes` is for and which the
integration test verifies end to end.

**`claude -p` auto-denies; it does not hang.** elk `docs/rein.md` §3 says an
unconfigured `-p` run "hangs on its first edit". It does not, at 2.1.251: it
emits `system/permission_denied`, carries on, and **exits 0 reporting
`success`** with the denial in `result.permission_denials[]`. The dangerous
failure is therefore not a wedged session but a run that quietly did nothing and
called it a win. Filed against the doc as
`elk:17 (01M17YWG2F8RG8PKSFXDHJDS7Q)`; the recording is
`testdata/permission_denied.jsonl`.

**A denial is reported as `progress`, never as `permission_request`.** That kind
obliges the run loop to answer with `Session.Respond`, and this adapter declares
`approvals: partial` precisely because nothing can: Claude Code 2.1.251 gives a
headless caller no way to answer a prompt. The SDK has `canUseTool`; the CLI has
no equivalent flag. `Respond` returns `ErrNotSupported`.

**`message` changes type between line types.** It is a JSON *object* on
`assistant` and `user` lines and a plain *string* on
`system/permission_denied`. A struct with `Message string` silently fails to
decode every assistant line; a struct with `Message vendorMessage` fails on
denials. It is `json.RawMessage`, decoded per line type, and there is a test
pinning both.

**The `init` line is big.** Tens of kilobytes on a machine with plugins and MCP
servers — it enumerates every tool, skill, plugin and slash command. `bufio`'s
default 64 KiB scanner limit is not enough; the reader is raised to 16 MiB,
because a tool result can be far larger still.

**Plan mode is read-only about the *worktree*, not about the disk.** A plan-mode
run writes its plan to `~/.claude/plans/`, and every session writes a transcript
to `~/.claude/projects/`. Neither is the run's work product, but both mean
"mutates nothing" is not literally true. Under `full` the agent is not sandboxed
at all.

**`worktree: partial` — enforced in one mode, not the other.** The adapter
arranges what it can — cwd is the worktree, no `--add-dir` — and under
`read_only` a write outside the worktree is auto-denied. Under `full`,
`bypassPermissions` approves a write outside the worktree as readily as one
inside it. That is the same situation the Grok adapter declares `partial` for
(`docs/adapter-grok.md`), so this one does too: ruled on Elk Scout
#706, 2026-10-01, `ark:rein#12 (01M17YVY2NCFNHATGYQ1TNGSV0)`. `partial` fails
closed, so a packet that names `worktree` (or `runtime:worktree`) in its
requirements is refused on claude before a worktree is cut; no packet did when
this was ruled. The way to promote it is `--restricted`, which confines the file
tools to the working directories but also strips Bash and ignores settings, so
it would cost `shell`.

**Timeouts are enforced here, not just accepted.** The contract says an adapter
must refuse a timeout it cannot honour, so all three are real: `Startup` bounds
the wait for the `init` line (default 2 minutes), `Idle` the gap between events,
`Total` the whole session. Zero means unbounded for `Idle` and `Total` — the run
loop's own watchdog ladder owns those — and the default for `Startup`.

**Interrupt takes the process group, not the process.** The session is started
with `Setpgid`, and `Interrupt` sends `SIGINT` to the whole group before
escalating to `SIGKILL` after a grace period. A `claude -p` session spawns MCP
servers and hook commands; signalling only the parent leaves them running. This
is Loom's `sweep.md` lesson (elk `docs/rein.md` §4) in the one place Rein can act
on it. Windows has no `SIGINT` to deliver to a child in a new process group, so
`Interrupt` goes straight to `taskkill /F /T`.

**An interruption carries no error.** `Wait` returns `StatusInterrupted` with a
nil error, matching `internal/adapter/fake`: stopping a run is a decision, not a
fault. Failures and timeouts set both a non-succeeded status *and* an error.

**`Start` does not fail when the process dies instantly.** A binary that rejects
a flag or crashes on startup can exit before the opening prompt reaches its
stdin, and the write then gets `EPIPE`. Returning that would make `Start` fail
intermittently with "broken pipe" while discarding the process's own stderr,
which is the part that says what went wrong — and it raced, so it failed only
sometimes. (It broke `main` once, on 2026-08-29.) The write error is recorded
instead and the session starts normally: the stream ends, no result arrives, and
the terminal error carries the stderr. `TestSessionWithoutAResultFails` pins it.

## Tests

```
go test ./internal/adapter/claude/                      # unit — what CI runs
go test -race ./internal/adapter/claude/                # the session is concurrent
go test -tags integration ./internal/adapter/claude/    # drives the real binary
```

The unit tests need no `claude`, no login and no network. They come in two
layers: the decoder replayed against the recorded fixtures, and the whole
`Session` — process, reader goroutine, terminal event, timeout ladder — driven
against a stub binary that replays a fixture and then drains stdin, which is
exactly how the real thing behaves.

The fixtures are genuine recordings, redacted in two ways only: the operator's
tool, skill, plugin and MCP inventory in the `system/init` line is replaced with
a short placeholder list, and the home directory is rewritten. Every event shape
is the vendor's own.

The integration tests are behind a build tag because they spend subscription
tokens on the developer's own login. Every one of them runs `read_only`, so a
failure cannot leave a file behind. They cover preflight, a trivial session,
`--resume` across two sessions, and the auto-deny behaviour.

**Re-record the fixtures when Claude Code's stream format moves.** The probe
scripts are not checked in; the recording command is just

```
claude -p "say hi" --output-format stream-json --verbose > hello.jsonl
```

run in a scratch directory, then redacted as above.

## Preflight

Three checks, in order, each wrapping `ErrPreflight` with something a human can
act on:

1. the binary resolves on `PATH` (or wherever `Adapter.Binary` points);
2. `claude --version` prints something — `2.1.251 (Claude Code)`;
3. `claude auth status` reports a login.

`auth status` prints JSON when its output is not a terminal:

```json
{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty",
 "subscriptionType": "max", "email": "…", "orgId": "…", "orgName": "…"}
```

Only `loggedIn` gates the run. Preflight **never reads, mints, refreshes or
replays a credential** — it shells out and reads stdout, and keeps nothing but
whether someone is logged in. That rule is what makes the whole design
defensible (elk `docs/rein.md` §3), and it is why the email and org fields above
are read past rather than stored. If the output stops being JSON, preflight
degrades to a substring check rather than failing a working machine on a format
change.

### Wrangler queue connector

`wrangler = true` (the Wrangler is Elk's PM agent; `pm = true` is the old
spelling and still accepted) is supported only for `agent_kind = "claude"` and
defaults to false. It advertises the capability `pm` — still the name on the
wire — and supplies a secret *name* to the adapter, not a URL. At session start
the adapter reads OS keychain service `elk-connector-url`, with account
`wrangler_connector_account` (or the old `pm_connector_account`) or the queue's
`<workspace>/<queue>` default. The operator must bind that account to the queue
owner's connector. There is no fallback to personal Claude settings or another
owner's binding.

The run loop sets that name only on a **Wrangler cycle**: a run whose packet
requires `pm`, on a queue with the opt-in (`ark:rein#50`). Every other run on
the queue reaches the adapter with an empty `WranglerConnectorAccount` and no
MCP servers, so it gets the ordinary strict, empty MCP config, as on any queue
without the opt-in.

A Wrangler queue ignores inherited servers and permits no configured
`mcp_servers` list. A Wrangler cycle's strict MCP config contains exactly one
HTTP server named `elk`. Its URL lives
only in memory and the 0600 `mcp.json` in the session's 0700 temporary directory;
argv contains its path. The adapter refuses a temporary directory inside the
worktree. Session cleanup removes it on success, failure and cancellation;
process crashes that bypass cleanup may leave a temporary file.

Resolver errors are replaced with a fixed diagnostic without their secret
payload. Claude stream output is scrubbed before decoding, including JSON-escaped
URL characters, and stderr is scrubbed before errors reach Rein. This protects
Rein's event log and results when a server or tool echoes the URL. Claude's own
vendor-managed transcript storage remains subject to Claude's behavior.

Wrangler cycles use the Wrangler production rule: scout migration README rule 5
with object verification, scout's deploy script, Signal's deploy workflow between
refresh passes, `-- HOLD:` headers respected, no website merges, and elk's
`docs/wrangler.md` playbook. The rule is spliced into the prompt per run, for a
Wrangler cycle only; every other run on a Wrangler queue, and every run on any
other queue, keeps the ordinary prompt and its rule against touching production.
