# `rein attach` — taking a live run over

Rein drives agents with nobody at the keyboard. That is the whole point, and it
is also why there has to be a hatch: a run that is going wrong, or that has
asked for something only a person can decide, needs somebody to be able to step
in without killing it and starting again.

```
rein attach                  the only run in flight
rein attach mac-claude       the run on one queue
rein attach 2e0a3e94         one run, by id or by a unique prefix
rein attach 2e0a3e94 --read-only
```

While you are attached the daemon **steps back**: its stall watchdog is held,
so silence from the agent is read as you thinking rather than as the agent
dying. It keeps reporting to Elk throughout, so the run does not lose its
fifteen-minute lease while you read. When you detach it resumes, and the run
finishes exactly as it would have — Rein submits the deliverable, with one line
in the footer saying a person took part.

## Why a socket, and not a resume

`ark:rein#26` left two designs open:

- **(a)** the daemon brokers the session over a local socket;
- **(b)** the session is respawned with `--resume` in the person's terminal
  while the daemon pauses.

(b) is cheaper and is what the `resume` capability exists for — but it is not a
takeover. It is a *second* session against the same id, started from whatever
the vendor persisted, while the original process is still running and still
holds the worktree. The two would write over each other, and `--read-only`
would be impossible because there would be nothing to follow.

So (a). The daemon keeps the process and stays the one thing driving it.

## Why not a PTY

The task assumed a PTY on both families, and ConPTY on Windows. It does not
need one.

A vendor CLI in `-p`/stream-json mode **is a pipe, not a terminal**, and every
adapter that takes input at all takes it as structured text through
`Session.Send` — Claude Code over stream-json stdin, Codex over `turn/steer`.
There are no keystrokes for a PTY to carry. So attach sends **lines**: no raw
mode, no `SIGWINCH` propagation, no terminal state to restore on a panic, no
`CreatePseudoConsole`, and no dependency. Your own shell does the line editing,
better than a takeover would reimplement it.

What a PTY would still buy is running the vendor's *interactive* TUI inside a
Rein worktree — a different feature, and one filed separately.

## The control plane

`rein run` listens on a local endpoint under the Rein home:

| | |
|---|---|
| macOS, Linux | a unix socket at `~/.rein/run/control.sock`, mode `0600` in a `0700` directory |
| Windows | a named pipe, `\\.\pipe\rein-control-<hash of the home>` |

The pipe name hashes the home because a pipe name cannot contain a path
separator, and because two Rein homes on one machine must be two endpoints —
what `$REIN_HOME` promises everywhere else. Its security descriptor is the
process token's default DACL: the user who started `rein run`, and LocalSystem.
That is exactly the audience, since the service already runs as the person
whose keychain, vendor logins and repositories the runs use.

The loopback alternative — a TCP port plus a shared-secret file — was rejected:
Go's `0600` on Windows sets the read-only attribute rather than an ACL, so the
secret would be readable by every other user on the box, and the port reachable
by every process on it.

**A socket file on disk proves nothing**, because a killed runner leaves one.
The test for "is somebody already serving this home" is a connection: if
something answers, `rein run` reports the endpoint unavailable and goes on
serving its queues; if nothing does, the file is debris and is replaced.

### The protocol

Newline-delimited JSON, one response per request: `list`, `attach`, `input`,
`permission`, `detach`.

**One connection is one attachment.** When the connection closes — a clean
`:detach`, Ctrl-C, a dropped ssh session — the runner resumes on its own,
because there is no other state to reconcile. That is why the lifetime is the
connection's and not a flag somebody has to remember to clear.

### What does not go over it

The **stream**. `rein attach` is `rein tail <run>` plus a control connection:
the agent's output is read from the run log ([`tail.md`](tail.md)), rendered by
the same renderer, so there is one rendering of a run and it looks the same
whether or not anybody is driving.

## At the prompt

You type lines. Lines beginning with a colon are commands:

| | |
|---|---|
| `:detach` | leave; the daemon resumes driving. Ctrl-D does the same |
| `:allow [why]` | approve the permission request the agent is waiting on |
| `:deny [why]` | refuse it |
| `:status` | what this run is, and what you can do to it |
| `:help` | the list |

Anything else is sent to the agent. To send a line that really does begin with
a colon, put a space in front of it.

### Permission requests

A queue-driven run **denies** every permission request its mode did not already
cover, because there is nobody to ask and granting it would make the mode a
suggestion. While you are attached that premise is gone: the request is offered
to you for **two minutes** (`AttachApprovalGrace`), and falls back to the usual
deny if you do not answer.

The mode is not weakened. Nothing is auto-approved; a person is answering a
question that was always meant for a person. Elk hears about it either way —
the `gate` event records who decided and why, because a decision made outside
the run loop would be one nobody could see afterwards.

## What each agent can do

Attach reads the adapter's `steer` capability, which is new with this and is
what `Session.Send` has always meant:

| | |
|---|---|
| **claude** | `steer: yes` — stream-json stdin |
| **codex** | `steer: yes` — `turn/steer` against the running turn |
| **grok** | `steer: no` — ACP has no mid-turn steer, so attach is **watch-only and says so before you type** |

**The claude window is the turn.** Under `--input-format stream-json` the
adapter treats the first `result` as terminal, closes stdin and reaps the
process — one Rein run is one Claude Code session. So a person can steer the
turn that is running, and once it ends the run submits as it always would;
`Send` then returns `ErrSessionClosed`. See `adapter-claude.md`'s Footguns.

## `--read-only`

`--read-only` is not a weaker attachment; it is **no attachment at all**. It
runs the follow and never opens a control connection, so the daemon keeps
driving and its watchdog keeps running. Following a run without taking it over
is exactly `rein tail <run>`, and the daemon must not step back for somebody
who has no intention of driving.

## What the deliverable says

One line in the run details footer:

```
- A person attached to this session for 6m12s and sent it 3 messages.
```

**And never what they typed.** The lines you send are recorded in this
machine's run log, `~/.rein/log/runs/<run id>.jsonl`, which is the account of
the run and does not leave the machine. A deliverable is read by whoever asked
for the work, and a takeover transcript is not theirs.

## Trade-offs worth knowing

**An attachment held open holds the watchdog open.** There is no cap: a
terminal left attached overnight is a run whose stall detection is off for the
night. The Elk lease keeps being renewed meanwhile, so the run stays alive —
which is what somebody who left it attached asked for — and the deliverable
records how long they were there. Closing the terminal ends it.

**One attachment per run.** A second `rein attach` on the same run is refused
rather than multiplexed; a second person can `--read-only` it, or `rein tail`
it, as many times as they like.

## Tests

- `internal/control` — the server and client over a real endpoint: list,
  attach, input, permission, detach; a refusal keeps the connection; one
  attachment per connection; a **dropped** connection releases the attachment;
  a second `Listen` on one home is `ErrInUse` and a stale socket file is
  replaced.
- `internal/runner` — against the fake adapter, over a real control socket: a
  person attaches to a live run, sends a line, and the agent receives it; the
  meta reads `attached` while they are there; the watchdog is held; an attached
  person answers a permission request and Elk's `gate` event records it; an
  adapter declaring `steer: no` is watch-only and refuses input; and the
  deliverable carries the one-line note and not the transcript.
- `internal/cli` — attach with no runner listening fails with an actionable
  message, and `--read-only` needs no runner at all.
- `internal/runner/attach_integration_test.go`, behind `-tags integration`: the
  same hatch against the **real** `claude` binary, proving that a line typed by
  a person and carried over the socket reaches a session already running and is
  acted on. CI never runs it.

## Still open

- **ConPTY, and a vendor TUI in a Rein worktree** —
  `ark:rein#32 (01M18PHZKCZ2FA3SNTP1G1DC8V)`. Not needed for this hatch (see
  *Why not a PTY*), and the task records why, what a pseudo-terminal would
  actually be for, and the Windows state after this change.
- **Nothing on the Windows side has run on real hardware.** The named pipe, its
  DACL and the `ERROR_ACCESS_DENIED` → `ErrInUse` mapping are gated by CI's
  `windows-latest` leg and by nothing else; Phase 1 is `ark:rein#10`.
- **A cap on how long the watchdog stays held**, for the terminal left attached
  overnight.
