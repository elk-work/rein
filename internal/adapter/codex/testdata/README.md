# Recorded `codex app-server` transcripts

Two real sessions, recorded frame-for-frame from `codex app-server` on
**2026-08-29** (codex-cli **0.150.1**, darwin/arm64, ChatGPT login). Each line
is one frame: `{"dir": "in"|"out", "frame": {…}}`, where `out` is what the
client sent and `in` is what the app server sent back.

| File | The session |
|---|---|
| `pong.jsonl` | `initialize` → `thread/start` (workspace-write, approvalPolicy `never`) → `turn/start` "Reply with exactly the word pong and nothing else." → `turn/completed`. The happy path, and the source of the token counts `replay_test.go` asserts on. |
| `approval.jsonl` | the same handshake with `read-only` + `on-request`, then a turn that asks Codex to write a file. Carries the real `item/commandExecution/requestApproval` round trip, id and all. |

`replay_test.go` derives its script from these rather than from a hand-written
table: an inbound frame is a reply, a notification or a request according to
which members it carries, exactly as the real client decides. So the tests
assert against what Codex actually said, and a protocol change shows up as a
test that no longer replays rather than as a silent divergence.

## What was changed, and only this

Two substitutions, both mechanical:

- **paths** — the recording machine's home directory became `/home/example`
  and the scratch worktree became `/tmp/rein-fixture/worktree`, so the fixture
  is not tied to one laptop;
- **one bearer token** — an MCP server URL in the recording embedded a
  credential in its path. It is `REDACTED`. Nothing else was touched: the
  thread ids, the rollout path, the token counts, the rate-limit payload and
  the frame order are all as recorded.

## One spliced exchange: `account/read`

Both files carry an `account/read` request and its reply right after
`initialized` — the plan-login check (`planauth.go`). It was recorded on
**2026-10-07** from codex-cli **0.159.0** on a ChatGPT Pro login and spliced in
rather than the sessions being re-recorded, so it is the one exchange from a
later build. Scrubbed: the email became `dev@example.com` and the ChatGPT
account id the zero UUID. Its request id, 9001, is chosen not to collide with
the recorded ones.

## Re-recording

Drive `codex app-server` over stdio, log every line in both directions, and
scrub as above. The two things to keep true:

- **the order** — the replay engine walks the script in recorded order and a
  reordering changes what is being tested;
- **the ids** — the server numbers its own requests from 0 while the client
  numbers from 1, and the fixture is what proves the client tells a request
  from a response by `method` rather than by the id.

A re-recording is due after a `codex` upgrade. `go test -tags integration
./internal/adapter/codex/` is what tells you one is due: it drives the real
binary and fails where the recording has stopped being true.

## Tool user input (2026-11-04)

`user-input.jsonl` contains the unmodified request and client reply from a real
codex-cli 0.159.0 stdio app-server session. Only this exchange is retained;
handshake, account notifications and unrelated turn frames are omitted.
The server completed the turn after receiving the recorded reply.

To reproduce: initialize with `capabilities.experimentalApi: true`, start an
 ephemeral, read-only thread with approval policy `never`, then start a turn
with `collaborationMode.mode: "plan"` and settings using the model returned by
thread/start, reasoning effort `low`, and developer instructions
"Ask the user the two questions using request_user_input."

Prompt: "Use request_user_input to ask me two questions in one call: which
color (Blue or Green) and which size (Small or Large) for a fictional button.
Do not decide yourself; invoke the question tool now."

The client answered with the first option label for each question. The fixture
proves both the numeric request ID and the per-question `answers` object;
`TestRecordedUserInputAnswers` checks the events and exact reply shape.
