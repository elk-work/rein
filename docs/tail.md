# `rein tail` — watching a run happen

Rein drives coding agents by watching structured events, and until this landed
it was the only thing that ever saw them. The events went down the adapter's
channel, a throttled sample became `report_progress`, and the rest was dropped.
The daemon's own `rein.log` said

```
20:14:07 mac-claude: claimed run 2e0a3e94 — Fix the flaky due-date test in ElkKit
20:51:22 mac-claude: run 2e0a3e94: submitted ready
```

and nothing at all for the thirty-seven minutes in between. A person who
wanted to know what their agent was doing had one option, which was to wait.

`rein tail` is the other option: the agents' own stream, live, in a terminal.

```
rein tail                    every run in flight, all queues
rein tail mac-claude         every run on one queue
rein tail 2e0a3e94           one run, by id or by a unique prefix
rein tail 2e0a3e94 --raw     the same, as the log's own JSON
rein tail 2e0a3e94 --since 10m
```

It reads files under the Rein home and talks to nothing else — not Elk, not
the daemon, not the agent. There is no way for a person watching a run to
disturb it, which is why it is safe to point at a machine whose service is
mid-run. (Taking a run over deliberately is `rein attach`; see
[`attach.md`](attach.md).)

## The log

`rein run` writes two files per run, under the Rein home's log directory,
beside the daemon's own `rein.log`:

| | |
|---|---|
| `~/.rein/log/runs/<run id>.jsonl` | every record, one JSON object per line |
| `~/.rein/log/runs/<run id>.meta.json` | queue, direction, status, worktree, branch, pid |

**The `.jsonl` holds every `adapter.Event` verbatim** — assistant text, tool
calls with their arguments, permission requests, questions, idle notices, token
usage, the terminal result, and the vendor's own payload in `raw` — plus Rein's
own lifecycle around them: `claimed`, `worktree`, `session`, `report`,
`review`, `submit`, `reaped`, `attach`, `note`. Each record says which half of
the system produced it:

```json
{"run_id":"2e0a3e94-…","seq":4,"at":"2026-08-30T06:17:09Z","source":"agent",
 "kind":"tool_use","tool":{"id":"t2","name":"Bash","input":{"command":"swift test --filter DueDate"}}}
```

`seq` counts the **log**, not the session: a run that stalls and is restarted
with `--resume` holds two sessions, and both of theirs start at 1.

**Per-run meta rather than one shared `index.json`.** Several queue goroutines
write these at once and more than one `rein run` may share a Rein home; a
single index would need a lock and a full rewrite per update, while a file per
run needs neither and is repaired by deleting it. It is rewritten by
write-and-rename, so a reader never catches it half written.

**The log outlives the worktree, deliberately.** Under the default the worktree
is removed the moment the deliverable saves, and from that point the log is the
only account of what happened inside it — which is why `rein tail <run>` on a
finished run is how you read back work you can no longer `cd` into.

**Nothing here can fail a run.** The writer keeps its first error for one line
in the daemon log and is a no-op afterwards, and a nil writer — a runner with
no log store, or one whose Rein home is unwritable — takes exactly the same
code path. The call sites in the run loop are unconditional and have no error
to get wrong.

## Rotation

The oldest logs are pruned where the worktree reaper runs, at the end of every
run, keeping the newest **50**:

```toml
# ~/.rein/config.toml
log_keep_runs = 50   # 0 or absent → 50; a negative number keeps everything
```

A run still being driven is never pruned however old it is: the log of a live
run is the one thing a live view is reading. Active runs do count against the
budget, so a machine with fifty runs in flight prunes nothing, which is the
right answer.

## The view

### All queues

With no argument it is a status block over a scrolling pane:

```
mac-claude  I'll start by reading the failing test.
mac-claude  ▸ Read: ElkKit/Tests/DueDateTests.swift
mac-codex   ▸ Write: docs/tail.md
mac-claude  ▸ Bash: swift test --filter DueDate 2>&1 | tail -40

QUEUE       RUN       DIRECTION                             ELAPSED  LAST            TOKENS
mac-claude  2e0a3e94  Fix the flaky due-date test in ElkK…    4m12s  ▸ Bash           12.4k
mac-codex   9f8e7d6c  Write docs/tail.md and land it            58s  ▸ Write           3.1k
```

The block is erased and reprinted above the scroll every refresh; the stream
scrolls normally under it. Runs appear as they are claimed and drop off once
they settle and their last record is on screen.

A narrow terminal drops columns from the right — `LAST` first, then `TOKENS`.
Queue, run, direction and elapsed are what a person needs in order to decide
which run to look at, and a truncated table that answers none of those is worse
than a narrow one that answers all four.

### One run

`rein tail <run>` drops the per-line run label and pins a footer instead:

```
· claimed on queue mac-claude — Fix the flaky due-date test in ElkKit
· worktree /Users/you/.rein/work/2e0a3e94 on branch rein/run-2e0a3e94 from origin/main (3f59708e)
  I'll start by reading the failing test.
▸ Read: ElkKit/Tests/DueDateTests.swift
▸ Bash: swift test --filter DueDate
  The assertion compares a formatted date to a literal,
  so it fails whenever the machine is not in UTC.
⚠ permission: git push -u origin rein/run-2e0a3e94 — waiting
? Should the fix pin the formatter to UTC, or take it from the fixture?
✓ succeeded — Pinned the formatter to UTC and added a regression test.
── 2e0a3e94 · mac-claude · running 4m12s · 11,400 in / 1,050 out · claude-opus-4
```

The vocabulary is the left column, because a person scanning a fast stream
reads marks, not words:

| | |
|---|---|
| *(none)* | assistant text, **verbatim** — this is the content, everything else is a frame |
| `▸` | a tool call, with the one argument worth showing |
| `⚠` | a permission request — the run is waiting on somebody |
| `?` | a question for a person |
| `·` | Rein's own lifecycle, and the agent's progress and idle notices |
| `✓` `✗` | the session ended |

**Token usage never appears in the stream.** It is an increment per record and
would interrupt the reading; it is summed into the footer, and printed once as
a closing total when the output is a pipe and there is no footer to put it in.

The tool summary is picked without a schema — `adapter.ToolUse.Input` is the
vendor's own JSON, passed through verbatim by contract — by trying the argument
names that carry the meaning, in order: `command`, `file_path`,
`notebook_path`, `pattern`, `url`, `query`, `path`, `description`, `prompt`.
`pattern` comes before `path` because a Grep carries both and the path is
usually `.`. A call matching none of them shows its argument names, which at
least says what shape it was.

## Flags

| | |
|---|---|
| `--raw` | the log's own JSON, one record per line; no rendering, no footer |
| `--since <dur>` | how much history to replay before following |
| `--interval <dur>` | how often the logs are re-read (default 400ms) |
| `--no-color` | never use ANSI colour, even on a terminal |
| `--width <n>` | override the terminal width |

**`--since` defaults differently for a live run and a finished one**, because
the two questions are different. A live run replays the last **2 minutes** and
then follows: pasting an hour of scrollback at somebody who wanted to see what
their agent is doing now is not helpful. A finished run replays **in full**,
because a window measured from now cannot reach a run that ended an hour ago.
Passing `--since` explicitly applies to both.

The totals always count the whole log, including records before the window. A
token count that started halfway through the run would be wrong, and the point
of the number is the total.

## No TUI framework

The in-place drawing is two escape sequences. Before writing anything new, move
the cursor up by the status block's height and erase to the bottom of the
screen; write the new stream lines, which scroll normally; print the block
again. Nothing needs the terminal's height, nothing redraws what has already
scrolled away, and the output is still a stream of lines — so **the degraded
path is not a second renderer, it is this one with the escapes turned off**.

Status lines are truncated to the terminal's width *before* they are counted. A
line that wraps occupies two rows, and the cursor-up count would then be short
by one per wrapped line, which is how an in-place renderer starts eating the
scrollback above it.

Width comes from `TIOCGWINSZ` on macOS and Linux and
`GetConsoleScreenBufferInfo` on Windows (`term_unix.go`, `term_windows.go`),
falling back to 100 columns. `$COLUMNS` is not used: it is a shell variable and
is not exported to child processes. On Windows the console's
`ENABLE_VIRTUAL_TERMINAL_PROCESSING` is switched on best-effort — Windows
Terminal and PowerShell 7 arrive with it on, an older conhost does not, and a
console that refuses gets the same treatment as a pipe.

**Not a terminal ⇒ plain lines.** No status block, no colour, no escapes: the
block could never be erased, so it would be duplicated noise in the file. Every
line is labelled with its run when more than one is being followed, and each
run's totals are printed once when it ends. `rein tail 2e0a3e94 | tee run.txt`
and a CI leg both work.

## Tests

- `internal/runlog` — the writer and reader round-trip, a nil writer, an
  unwritable store, the tailer over a partial line and a truncated log, lookup
  by id, prefix and queue, and the pruner's budget.
- `internal/tail` — golden output for the renderer over a **recorded
  fake-adapter run** (`testdata/run.golden`), the dashboard at two widths
  (`testdata/dashboard.golden`), the screen's exact escape sequences, and the
  driver end to end: a finished run replaying, a live run followed until it
  settles, `--raw`, `--since`, and the all-queues watch.
  Regenerate the goldens with `go test ./internal/tail -update`.
- `internal/runner` — one run driven all the way leaves a log with every
  lifecycle kind and the whole event stream in it, a refused run still leaves
  one, a nil store takes the same path, and the prune runs with the reaper.
