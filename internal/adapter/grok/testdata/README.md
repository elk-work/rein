# Recorded `grok agent stdio` transcripts

Two real ACP sessions, recorded frame-for-frame from `grok agent stdio` on
**2026-08-29** (Grok Build **1.0.13**, darwin/arm64, cached grok.com login).
Each line is one frame: `{"dir": "in"|"out", "frame": {…}}`, where `out` is
what the client sent and `in` is what the agent sent back.

| File | The session |
|---|---|
| `pong.jsonl` | `initialize` → `session/new` (yoloMode, one Rein-supplied http MCP server) → `session/prompt` "Reply with exactly the word pong and nothing else." Also the evidence for `mcp: yes` — the agent's `_x.ai/mcp/init_progress` counts three servers where the developer's own config has two. |
| `toolcall.jsonl` | a turn that runs a shell command. Carries a `tool_call`, its `tool_call_update`s, **two** `response_completed` payloads (so two usage increments), and the `pending_interaction` → `interaction_resolved` pair that is the whole evidence behind `approvals: partial`. |

## The ordering rule these fixtures need

Grok's terminal signal is the **reply to `session/prompt`**, and every session
update arrives between that request and its answer. So `replay_test.go` records,
for each frame, which client request was in flight when the agent sent it, and
the peer will not push a frame until the client has actually sent that request.
Replaying by recorded position alone would deliver the whole turn before the
adapter had asked for anything.

The script is derived from the recording rather than hand-written, so a
protocol change surfaces as a replay that no longer matches instead of as a
table someone forgot to update.

## What was changed, and only this

- **paths** — the recording machine's home directory became `/home/example`
  and the scratch worktree `/tmp/rein-fixture/worktree`;
- **one bearer token** — the developer's Elk MCP server URL embeds a
  credential in its path (`.../elk-mcp/<token>`). It is `REDACTED`.

Nothing else: session ids, token counts, the model metadata, the hook-execution
noise and the frame order are all as recorded.

## Re-recording

Drive `grok agent stdio` over stdio, log every line in both directions, scrub
as above. Two things must stay true:

- **the interleaving** — which updates arrive during which request is exactly
  what the ordering rule replays;
- **the two `response_completed` payloads in `toolcall.jsonl`** — they are what
  proves the usage normalisation. Grok's per-response `input_tokens` *excludes*
  the cached read while the turn total includes it, and the test asserts that
  the increments sum to the total. A one-model-call recording cannot catch that
  bug.

A re-recording is due after a `grok` upgrade. `go test -tags integration
./internal/adapter/grok/` is what tells you: it drives the real binary and
fails where the recording has stopped being true.
