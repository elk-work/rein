# The Codex adapter

**`ark:rein#5 (01M17TTZKZCBCVJZXFRDZKZ65R)`. Package `internal/adapter/codex`.**

Drives OpenAI's Codex CLI through **`codex app-server`**, its JSON-RPC 2.0
control surface. Read [`docs/adapters.md`](./adapters.md) first — this file
only records what is true of Codex.

Everything below was checked against the running binary on **2026-08-29**,
`codex-cli 0.150.1`, darwin/arm64, ChatGPT login. Where a claim is inference
rather than observation it says so.

**Re-verified 2026-09-29 against `codex-cli 0.159.0`**: the integration suite
passes, and the regenerated schema differs from `schema/` only additively — no
new required field on any request this adapter sends (`ThreadResumeParams`
gained `excludeTurns`, `TurnStartParams` four optional fields). The runs that
failed in between were not a protocol change; see **Isolation** and
`ark:rein#38`.

## Why the app server, not `codex exec`

`codex exec --json` is one-shot: it decides approvals up front and exits. The
app server is bidirectional and long-lived — OpenAI's own framing is that it is
for "a task queue or orchestration layer" — which is what buys Rein three
things `exec` cannot give it: approvals arriving as requests the run loop
answers, a thread that outlives the turn (so `resume` is real), and a
structured notification stream instead of a JSONL dump.

The `exec` flags people expect **do not exist**: `--ask-for-approval` and
`--full-auto` are rejected as unexpected arguments however many published docs
still list them (elk `docs/rein.md` §3). The real knobs are `-s` /
`sandbox` and `approvalPolicy`, and they are what this adapter sets.

## The wire

```
codex app-server        # cwd = RunSpec.WorktreeDir, --listen stdio:// is the default
```

**Newline-delimited JSON, one object per line. Not `Content-Length`.**

Three things the schema does not tell you and the binary does:

1. **Codex omits `"jsonrpc"` on the frames it sends.** A decoder that validates
   the member rejects every message. `jsonrpc.go` writes it and ignores it.
2. **A server request and a response to one of ours are told apart by
   `method`, not by the id.** The server numbers its own requests from `0`
   while the client is numbering from `1`, so ids collide by construction. A
   frame with `method` is a request or a notification; one without is a
   response, and only then does the id mean *our* id.
3. **Frames get large** — a `turn/completed` carries the whole turn — so the
   reader is a `json.Decoder` over the stream rather than a `bufio.Scanner`
   with a token ceiling.

The handshake, in order: `initialize` → the `initialized` notification →
`thread/start` (or `thread/resume`) → `turn/start`. `Start` returns once the
turn is running; everything after that arrives as notifications.

## Permission modes

| Rein mode | `sandbox` | `approvalPolicy` | What it means here |
|---|---|---|---|
| `read_only` | `read-only` | `never` | reads and reasons; the OS refuses writes and nothing can be authorised |
| `accept_edits` | `workspace-write` | `on-request` | writes inside the worktree are free; an escalation comes back as a request |
| `ask` | `workspace-write` | `untrusted` | anything not already trusted raises a request |
| `full` | `danger-full-access` | `never` | everything authorised up front |

`never` is what makes `read_only` and `full` quiet: under it Codex raises no
approval at all, so neither mode can wait on an answer nobody promised.

**`AllowedTools` and `DeniedTools` are refused, not ignored.** The app server
has no per-tool allow list this adapter has verified, and quietly dropping the
narrowing a packet asked for is exactly the downgrade `docs/adapters.md`
forbids. `Start` returns `ErrNotSupported`.

## Capabilities

| Capability | Declared | Because |
|---|---|---|
| `git` | yes | shell in the worktree; verified |
| `worktree` | yes | see below |
| `file_edit` | yes | `fileChange` items, verified |
| `shell` | yes | `commandExecution` items, verified |
| `mcp` | yes | `thread/start` `params.config.mcp_servers`, verified with a probe server |
| `resume` | yes | `thread/resume` by thread id |
| `structured_events` | yes | the notification stream below |
| `approvals` | yes | a full `requestApproval` round trip, verified |
| `steer` | yes | `turn/steer` against the running turn — what `rein attach` sends on |

**`worktree: yes` is a judgement, and here is the reasoning.** Loom declares
Codex worktree isolation `partial` because its adapter does not set the
sandbox, and a Codex that can reach outside its cwd is exactly what `partial`
is for. This adapter always sets it: every mode but `full` starts the thread
under `read-only` or `workspace-write`, whose writable root is the cwd — the
worktree — enforced by the OS rather than by the agent's good intentions. And
`full` is by contract only ever pointed at a worktree of a repository we own.
So the isolation Loom could not claim, this adapter can. The integration test
`TestIntegrationReadOnlySandboxHolds` is what keeps that honest: it asks for a
write in `read_only` and fails if one lands.

**`mcp: yes` means exactly the packet's servers.** It did not always: until
`ark:rein#21` the developer's own `~/.codex/config.toml` servers loaded for
every thread as well. They no longer do — see **Isolation** below, which is the
most important thing in this file.

## Isolation: a private `$CODEX_HOME` per session

**A session sees only the packet's MCP servers.** It gets a `CODEX_HOME` of its
own, built at `Start` and removed when the session ends.

This is not tidiness. On 2026-08-29 a dogfood run submitted its own
deliverable: the agent found the Elk MCP connector in the developer's personal
config, called `submit_deliverable` **under his identity** mid-run, Elk reviewed
that submission, Rein submitted again, and the run ended in a state the review
sweep treats as already reviewed (`ark:rein#21`). Codex had the same exposure,
verified rather than assumed — a probe passing one MCP server through
`thread/start` watched four connect: the one Rein asked for, plus `elk`,
`node_repl` and `codex_apps` from `~/.codex/config.toml`.

An inherited server is not extra capability. It is capability *as somebody
else*.

**Why a private home and not an override.** `codex app-server -c mcp_servers={}`
does nothing — config overrides deep-merge, so an empty table changes nothing
and all three servers still loaded. Disabling them one at a time would mean
reading the developer's `config.toml` for names and hoping none is added
mid-run. `CODEX_HOME` is the switch that exists: Codex reads its whole
configuration from there, so a home without a `config.toml` has no MCP servers,
no hooks, and no personal model pin.

**What is linked in**, by symlink, and nothing else:

| Linked | From | Why |
|---|---|---|
| `auth.json` | the developer's `~/.codex` | the login. Rein never reads, mints, refreshes or replays a credential — a link means the *vendor binary* opens its own credential file at another path, which is a different act from Rein handling the token |
| `sessions/` | **Rein's own** `$REIN_HOME/codex/sessions` | the rollout transcripts, so `Result.TranscriptPath` still resolves and `thread/resume` can still find a thread a previous session created |

Whatever Codex writes during the session — its state DB, the thread-history
projection, memories, logs — is thrown away with the directory.

**Why Rein's history and not the developer's (`ark:rein#38`).** Through
v0.2.1 `sessions/`, `session_index.jsonl` and `thread_history_*.sqlite*` were
linked from `~/.codex`, and that is what stopped mac-codex starting. A home
that has rollouts and no state DB makes `codex app-server` build one — an index
over **every rollout under `sessions/`** — *before it answers `initialize`*.
The developer's history was 1.9 GB / 64,066 rollouts:

| Probe: `codex app-server`, private home, empty cwd, no turn | `initialize` answered after |
|---|---|
| 0.159.0, `auth.json` only | 0.05 s |
| 0.159.0, the v0.2.1 links | **61.75 s** |
| 0.154.0, the v0.2.1 links | **39.2 s** |
| either, the v0.2.1 links plus the developer's `state_5.sqlite*` | 0.06–1.3 s |

Under launchd it was slower still: 49 s on 2026-08-31, and past the 90 s
startup timeout by 2026-09-29, when a run submitted `stuck — The agent would not
start` having never run. The pong integration test kept passing at 47 s, which
is why a Codex upgrade looked like the suspect; the protocol had not changed.

Linking the developer's state DB as well is the fix that looks obvious, and it
is wrong: Codex records each thread's rollout path as `$CODEX_HOME/sessions/…`
without resolving the link, so a shared index fills with paths into private
homes that have since been deleted. `TestIntegrationResume` failed with "no
rollout found for thread id", and the dead rows land in the developer's own
index. A fresh index per session is right; building it over somebody else's
two gigabytes is not. Rein's own history holds only Rein's sessions, so the
index costs what Rein has run — `initialize` in under a second today.
`TestIntegrationPongSession` fails if `Start` takes more than 20 s, so the next
slow start is caught while it is still only slow.

Rein's history grows by one rollout per session and nothing prunes it yet. At
the rate measured above (about 1 ms per rollout) that is years away from
mattering; if it ever does, a startup timeout says so (see **Timeouts**).

Three consequences worth expecting:

- **The model default changes.** Without the developer's `config.toml`, an
  empty `RunSpec.Model` gets Codex's own built-in default rather than his pin.
  That is the intent — the queue's `model` and `effort` in Rein's config.toml
  decide, passed as the thread's and turn's `model` and the turn's `effort` —
  but it is a visible difference for a queue that sets neither.
- **Plan login only.** Right after `initialized`, before any thread, the
  adapter calls `account/read` and stops the session unless `account.type` is
  `chatgpt` — an `apiKey` or `amazonBedrock` account, or none, fails with
  `adapter.ErrNotPlanAuth` (`planauth.go`). `Start` also refuses a spec whose
  `Env` carries `OPENAI_API_KEY` or `CODEX_API_KEY`.
- **The developer's hooks no longer fire** in a run. Same inheritance, same
  reasoning.
- **Rein's threads are not in the developer's Codex history** and his are not
  in Rein's. A run resumes only threads Rein created, so nothing needs his.

`Result.TranscriptPath` is rewritten from the private home to Rein's history
before the session reports it: Codex names the rollout file relative to its own
`$CODEX_HOME`, the file itself lives in `$REIN_HOME/codex/sessions` through the
`sessions` link, and the private directory is about to be deleted.

**If Codex rotates its login mid-session** — replacing the symlink with a real
file holding a newer credential — the private home is *not* deleted, and the
session emits a progress event naming the path. Deleting it would log the
developer out; copying it back would mean this package handling a token. It
does neither.

**One residual.** `codex_apps` is a built-in of the binary rather than user
configuration, and survives an otherwise empty home. It carries no Elk tool and
no identity; `ark:rein#22` tracks closing it.
`TestIntegrationSessionSeesOnlyTheSpecsMCPServers` asserts the set of servers a
real session starts, and fails on anything else.

## Events

| Codex | Rein event | Notes |
|---|---|---|
| `item/completed` `agentMessage` | `text` | the whole message; deltas are not events |
| `item/agentMessage/delta` | `progress`, coalesced | one per token would drown the loop; at most one per 5 s says the agent is alive |
| `item/started` `commandExecution` | `tool_use` `shell` | |
| `item/started` `fileChange` | `tool_use` `apply_patch` | |
| `item/started` `mcpToolCall` | `tool_use` `<server>/<tool>` | |
| `item/started` `dynamicToolCall` / `webSearch` | `tool_use` | |
| `item/started` `reasoning` | `progress` | |
| `item/completed` (others) | `progress` | exit code for a command |
| `item/*/requestApproval` | `permission_request` | in `ask` / `accept_edits`; decided by the mode otherwise |
| `thread/tokenUsage/updated` | `usage` | **an increment**, see below |
| `thread/status/changed` `idle` | `idle` | a real signal, not a scraped spinner |
| `thread/status/changed` `active` | `progress` | carries flags like `waitingOnApproval` |
| `error` (`willRetry: false`) | `progress` | |
| `error` (`willRetry: true`) | `progress` | Codex retries these itself; treating one as terminal ends sessions that were about to succeed |
| `turn/completed` | `done` | terminal |
| process exit before the turn ends | `error` | terminal, quoting the app server's stderr |
| anything else | `progress` with `Raw` | never a new kind |

**Usage is differenced, not forwarded.** The notification carries a running
`total` for the thread and a `last` for the most recent model call; Rein's
usage events are increments and a turn makes several model calls, so neither
field can be passed through. The adapter subtracts the total it last reported.
`Result.Usage` is the accumulated total, and the model name comes from the
`thread/start` response — the usage payload does not carry one.

**`Result.TranscriptPath`** is Codex's own rollout JSONL under `$CODEX_HOME`,
which `thread/started` hands over. The run loop decides whether to upload it.

## Subscription headroom (`ark:rein#40`)

Codex is the one adapter that can read its window **without a turn**:
`account/rateLimits/read` (params `{excludeResetCreditDetails: true}`, the
background-poll flag) answers from the backend's usage endpoint. `ReadHeadroom`
starts `codex app-server` under a private home holding only the `auth.json`
link — no history, so there is nothing to index before `initialize` —
initializes, reads, and stops. On a ChatGPT Pro login on 2026-09-29 (0.159.0)
that took 0.7 s and returned one `primary` window of 10080 minutes at 12%, no
`secondary`, `ordinaryUsageAllowed: true`. During a run the same shape arrives
as `account/rateLimits/updated`, and the session reports it through `Telemetry`.

A window is named by its stated length — 300 minutes is `five_hour`, 10080 is
`seven_day`, which Elk already renders — and keeps its slot name (`primary`,
`secondary`) otherwise. `usedPercent` becomes utilization 0..1.

**Exhausted** is `ordinaryUsageAllowed: false`, any `rateLimitReachedType`, or
a window at 100% with no credits behind it; `ExhaustedUntil` is the latest reset
among the full windows. The schema says a client "must not infer recovery from
percentages or reset times", so the run loop re-reads before it claims again
after a hold rather than trusting the clock.

## One turn per session

Rein's unit of work is an Elk run: one packet in, one deliverable out. Codex
threads outlive turns, so the thread id is what `Result.SessionID` reports and
what a later `ResumeID` takes — but the session this adapter hands back ends
when its turn does. A follow-up is a new session against the same thread.

`Send` during the turn is `turn/steer`, Codex's own word for it: the running
turn takes the input into account rather than a second turn queueing behind it.
After the turn ends, `Send` returns `ErrSessionClosed`.

## What this adapter declines, and why

Every server request gets an answer, including the refusals — an unanswered
request wedges the turn.

| Request | Answer | Why |
|---|---|---|
| `account/chatgptAuthTokens/refresh` | error | **Rein never handles a vendor credential.** It shells out to the binary on the developer's own login and does not read, mint, refresh or replay a token (elk `docs/rein.md` §3). This request asks the client to do exactly that. |
| `item/tool/requestUserInput`, `mcpServer/elicitation/request` | error, plus a `progress` event | These are questions about the *work*, and Rein has a place for one — `EventQuestion`, answered with `Send`, routed to Elk's `ask_elk`. Wiring the answer back needs a per-question response shape this adapter has not verified, so v0 declines and says so rather than pretending. **Open: `ark:rein#13 (01M17YXMR467FX7537AE7NM0CD)`.** |
| `execCommandApproval`, `applyPatchApproval` | error | the v1 spellings, with a different decision vocabulary. 0.150.1 sends the v2 methods; answering in an unverified shape would be worse than declining. |
| anything else | error `-32601` | |

`item/permissions/requestApproval` is answered rather than declined, and the
shape is worth recording: the response *grants a profile* and has no "decline".
Allowing echoes the requested profile back verbatim — `RequestPermissionProfile`
and `GrantedPermissionProfile` are the same schema — and denying grants an
empty one, which is what "you may do nothing extra" means here.

## Timeouts

`Timeouts.Startup` bounds the handshake (default 90 s — `thread/start` boots the
packet's MCP servers, and a slow one is the usual reason it takes seconds).

**A start that fails says why on its own**, because its error is the whole of
the `stuck` deliverable and of the local record. It names the step that stalled
and the timeout, what the app server had said by then (those notifications are
otherwise dropped with the session — "nothing" before `initialize` means Codex
was busy before it would talk), the history it was handed to index when the
stall was `initialize`, and stderr, including when stderr was empty. The run
loop writes the same text to `rein.log` and the run's event log. Before
`ark:rein#38` it said `initialize: context deadline exceeded` to Elk and nothing
at all locally.
`Timeouts.Total` is enforced: the adapter interrupts the turn and reports
`timed_out`. `Timeouts.Idle` is **not** the adapter's to enforce — the contract
defines it as the run loop's watchdog input, and the `idle` event above is what
feeds it.

## Tests

`replay_test.go` drives the adapter against `testdata/*.jsonl`, verbatim
recordings of two real sessions; the script is *derived* from the recording
rather than hand-written, so a protocol change surfaces as a replay that no
longer matches. See `testdata/README.md`.

`integration_test.go` is behind `//go:build integration` and drives the real
binary: `go test -tags integration ./internal/adapter/codex/`. It is not in the
PR gate and must not be — it needs a login and spends the developer's own
five-hour quota, shared with the human at the keyboard. Run it after a `codex`
upgrade; it is what tells you the recordings need redoing.

`schema/` holds the protocol subset the Go types model, generated with
`codex app-server generate-json-schema --out`. See `schema/README.md`.
