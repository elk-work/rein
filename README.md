# Rein

**Drive.** The runner that serves [Elk](https://elk.work)'s agent queues from
your own machine.

Elk hands work to coding agents through queues. Rein is the program on a
developer's machine that takes that work and does it there. It is a single Go
binary installed as a user service (launchd, Windows service,
`systemd --user`). It enrols the machine into an Elk workspace as one named
queue per agent kind, claims the runs Elk dispatches to those queues, and
drives a coding-agent session against each one in an isolated git worktree —
Claude Code, Codex or Grok Build, shelled out as the vendor's own binary on the
developer's own login. It watches each session through structured events,
reports progress and the deliverable back to Elk, and recycles the worktree.

The agents run on your machine, under your logins, against your checkouts.
That is why Rein's source is open: a program that holds your tokens and drives
agents in your code should be one you can read.

## Install

Download the archive for your platform from
[Releases](https://github.com/elk-work/rein/releases), check it against the
`.sha256` published beside it, and put `rein` on your `PATH`. Builds exist for
macOS (`darwin_arm64`, `darwin_amd64`), Linux (`linux_amd64`, `linux_arm64`)
and Windows (`windows_amd64`, a `.zip` holding `rein.exe`).

```sh
v=$(curl -fsSL https://api.github.com/repos/elk-work/rein/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
p=darwin_arm64
curl -fsSLO "https://github.com/elk-work/rein/releases/download/$v/rein_${v}_${p}.tar.gz"
curl -fsSL "https://github.com/elk-work/rein/releases/download/$v/rein_${v}_${p}.tar.gz.sha256" | shasum -a 256 -c
tar -xzf "rein_${v}_${p}.tar.gz"
install "rein_${v}_${p}/rein" ~/.local/bin/rein
```

(Linux: `sha256sum -c` in place of `shasum -a 256 -c`.) Then connect a queue
with a claim code from Elk → **Connected agents** → **Connect another agent**,
and run it as a service:

```sh
rein enrol --workspace "<your workspace>" --agent-kind claude --claim-code <code>
rein service install
rein service start
```

[Enrolling](#enrolling) and [Serving](#serving--the-user-service) below cover
the details. Building from source is under [Building](#building).

## Status

**v0 complete.** `enrol`, `run`, `tail`, `attach`, `service`, `status` and
`version` all work — `attach` was the last stub and `ark:rein#26
(01M1895B4M85C301MB5N90AT96)` implemented it. Exit code **2** is still
reserved for a command that lands ahead of its implementation.

## Usage

```
rein enrol --workspace "Elk Scout" --agent-kind claude --claim-code <code>
rein enrol --workspace "Elk Scout" --agent-kind claude --token <connector-token> [--shared]
rein enrol --revoke --queue mac-claude
rein run   [--queue mac-claude] [--once] [--dry-run] [--keep-worktrees]
           [--review-timeout 20m] [--max-review-rounds 3]
           [--max-concurrent 4] [--log-file PATH] [--service]
rein tail   [run|queue] [--raw] [--since 10m]
rein status [--online]
rein config check [--json]
rein version
rein attach [run|queue] [--read-only]
rein service install | uninstall | start | stop | status
```

Exit codes: `0` success, `1` an error, `2` not implemented in v0.

### Enrolling

`rein enrol` binds this machine to an Elk workspace as one named queue for one
agent kind, and puts that queue's Elk token in the OS keychain.

Two ways in, and the first is the one to prefer:

| | |
|---|---|
| `--claim-code <code>` | a single-use invite (Elk → **Connected agents** → **Connect another agent**). Rein exchanges it for a durable **run-scoped** token and stores that. The claim carries its own workspace and queue name. |
| `--token <token>` | your own connector token, from Elk → Tweaks → **Elk Connector**. Broader — it is your personal, `interactive`-tier credential — but it needs no invite. |

Either can come from the environment instead of the command line, which keeps
it out of `ps`: `REIN_ELK_CLAIM_CODE` and `REIN_ELK_TOKEN`.

Rein asserts Elk's `self_configure` on the claim exchange, which is what makes
Elk hand back the durable credential in-band. It qualifies: the token goes
straight into the keychain and is never printed, logged or repeated — that is
the whole condition Elk attaches to the flag.

**Who may send work here — `--shared`.** A queue is **private** by default:
only the person whose credential connected it can queue work to it. Pass
`--shared` and any member of the workspace can. Neither is permanent — the
queue's owner changes it later in Elk → Settings → **Connected agents** — and
`rein enrol` prints which one it got, because Elk's own reply does not say.

`--shared` applies to the `--token` path only. On `--claim-code` the inviting
person already chose private or shared on the invite sheet, so Elk ignores the
argument and says so in the reply Rein prints. Rein sends it anyway rather than
dropping it silently: a flag that vanishes without a word is how someone comes
to believe a private queue is open to their team.

The key is absent from the request unless `--shared` is set, so an Elk older
than scout migration 0201 (2026-08-31, `ark:scout#379`) sees no argument it
does not recognise.

`rein enrol --revoke --queue <name>` forgets a queue **locally**. It deletes
the keychain entry *first* and drops the config row second: the OS keychain
APIs Rein uses cannot be enumerated, so the config is the only index of which
tokens exist, and a config row removed first would strand its secret with
nothing left that knows to ask for it. Enrolment does the reverse for the same
reason — config first, token second, so a half-finished enrolment shows up in
`rein status` as `missing — run rein enrol` rather than as an invisible orphan.
Revoking the credential *in Elk* is an admin action in Settings → Connected
agents.

### Running

`rein run` is the loop: heartbeat each configured queue, claim a run when Elk
says one is waiting, cut a worktree, drive the queue's agent against the
packet, report while it works, submit the deliverable, reap the worktree.

```
rein run --queue mac-claude --once     # one run per queue, then exit
rein run --dry-run                     # claim nothing; say what is waiting
rein run --keep-worktrees              # leave each run's checkout in place
```

Ctrl-C interrupts an in-flight session and submits nothing for it: the claim
lapses and Elk hands the run to whoever asks next, which is better than
reporting an outcome that did not happen.

By default the agent **lands its own work**: it pushes the run branch, opens a
non-draft PR, waits for CI and squash-merges it, and reports the PR URL and
merge sha. Rein fills the branch name into the prompt per run. An environment
that refuses the push or the merge means the PR is left open and the
deliverable says so. A queue serving a repository somebody else owns can stop
short of that with its **landing policy** — see below.

The PR body carries **no Elk-closing sigil**: closing the item from a PR that
merges mid-run makes Elk cancel the run and refuse its deliverable, which lands
the work and loses the record. Rein closes the item afterwards instead.

A run flagged **"Review with Elk"** is not finished at the submit: Elk's review
pass may reopen it with a revision request and expect the agent to still be
there. Rein stays, polls, works the revisions (resuming the session where the
adapter can), and gives up into `stuck` after `--max-review-rounds`. Silence for
`--review-timeout` counts as approval, because an approved run looks exactly
like one whose review has not run yet. No agent runs while Elk reviews, so the
run gives its concurrency slot back for the wait and takes one again only to
work revisions.

Each run gets a git worktree of its own, branched from the **remote's default
branch** after a fetch — not from whatever the local checkout is sitting on,
which is routinely a stale release branch. The repository's `.ark` is symlinked
in, so `ark` works in the worktree and the run's records land in the project's
own history; Rein never runs `ark init`, which would mint a new repository id
and orphan that history. Both are reported in the run's first progress note and
in the deliverable's footer.

**Elk's work order names no repository**, so Rein resolves one from four places
in order — a `repo:` line in the work order, a github.com URL in it, the
queue's `repo`, then `default_repo` — and submits `stuck` rather than guessing
when none of them answers. The state machine, the exact MCP calls with their
JSON, the stall ladder and the repository rules are in
[`docs/run-loop.md`](docs/run-loop.md).

```toml
default_repo = "/Users/you/dev/elk"
capabilities = ["SUPABASE_SERVICE_ROLE_KEY"]   # names only, never values

[repos]
scout = "/Users/you/dev/elk/scout"

[[queues]]
name            = "mac-claude"
agent_kind      = "claude"
repo            = "/Users/you/dev/elk"
permission_mode = "full"        # read_only | ask | accept_edits | full
land            = "merge"       # merge | pr | branch; unset is merge
capabilities    = ["xcode"]     # added to the top-level list
base_ref        = "origin/main" # rarely needed: the remote default is resolved
```

On machines serving multiple workspaces, add `repos = ["owner/repository"]`
to each workspace's queue entries, selecting keys from `[repos]`. Queues in a
workspace share the union of those lists and advertise only those repositories.
An undeclared repository ends the run stuck; it never uses `default_repo`.
Configs with no list on any queue keep the old rules: every queue sees the
top-level map and `default_repo` applies. On a multi-workspace machine that is
**compatibility mode** — the shape every such config had before v0.8.4, which
v0.8.4 read as "no repositories anywhere" and stranded every run
(`ark:rein#67`). Rein logs a warning naming the fix at every start until the
lists are added. The first list on any queue scopes the whole config, so add
one to every queue in a single edit, give a Wrangler queue a `repo`, and run
`rein config check` before restarting: it prints each queue's repositories and
default and exits 1 when a queue would strand its runs.

Queues are keyed by workspace and name. A second workspace may enrol the same
name without replacing the first; re-enrol keeps local settings. Use
`--queue 'Workspace/mac-claude'` when names collide, and use `--workspace` for
revoke. Claim enrol takes the workspace from Elk's reply and refuses a conflicting
flag. See [workspace configuration](docs/workspace-repositories.md) for a complete
example and compatibility details.

**Each queue has a landing policy** — how far a run takes its change
(`ark:rein#47`):

| `land` | the agent… | for |
|---|---|---|
| `merge` *(default)* | pushes, opens a non-draft PR, waits for CI, squash-merges it | elk-work's own repositories — the house rule |
| `pr` | pushes, opens a non-draft PR, waits for CI, and **stops** | a repository whose owners review and merge, such as a customer's |
| `branch` | pushes the run branch and **stops** — no pull request | owners who want to open the PR themselves, or none at all |

```toml
[[queues]]
name       = "mac-codex"
agent_kind = "codex"
workspace  = "Acme"
repo       = "/Users/you/dev/acme/app"
land       = "pr"               # the agent opens a PR and stops; Acme's team merges
```

Unset is `merge`, so a config written before the setting existed behaves
exactly as it did — a merge queue's prompt is byte-for-byte the vendored one.
Any other value is refused at load with the line to fix; a typo of `pr` is
never read as `merge`. The policy reaches the run three ways: the system
prompt's *Landing the work* section is that policy's own text; the per-run
*This run* block states it again beside the branch name; and after the session
Rein looks at the run branch on GitHub (`git ls-remote`, `gh pr list --head`)
and, if a `pr` run merged or a `branch` run opened a pull request, puts
**Landing policy breached** at the top of the deliverable and its title. The
policy also rides every heartbeat as `session.land`, so the executor row can
show it before anything is dispatched, and `rein status` prints it per queue.

**None of that is a control.** An agent holding a shell can merge whatever it
is told not to, and the check only looks at the run's own branch; Rein reports
a breach, it never reverts one. What actually prevents a merge is the
repository's branch protection — put a `pr` queue on a repository that
requires a review.

**A packet's `required_capabilities` span two namespaces**, and Rein checks each
name against the list that can answer it: the adapter contract's nine closed
runtime names against the agent's manifest, and every other name —
`elk-connector`, `github-cli`, `xcode` — against what this machine holds. That
host list is the built-ins Rein satisfies by construction, plus cheap
auto-detection at startup, plus `capabilities` above.
Rein also declares Elk's namespaced vocabulary alongside the legacy names:
`agent:<queue kind>`, `os:<host OS>`, `tool:<detected tool>`, and
`runtime:<manifest name>`. Existing `[repos]` directories add `repo:owner/name`:
slash keys supply the identity directly; bare aliases require a GitHub HTTPS or
SSH `origin`. Missing directories and origins that cannot be derived add nothing.
Duplicate identities are declared once.

Preflight checks namespaced runtime requirements against the manifest, tools
against built-ins and successful probes, agent and OS against the queue and host,
repositories against configured checkouts, and MCP against the resolved queue
allow-list. Config capability names cannot override these checks. `does:` and
`tier:` are Elk's responsibility; Rein accepts them without declaring them.
Unknown namespaces and unknown bare names still fail closed. The namespace list
mirrors Elk's `public.capability_vocabulary` by hand.

`REIN_HOST_CAPABILITIES="github-cli,docker"` replaces the probing for a sandbox
or CI box. `rein status` prints the list with how each entry was established.

**MCP servers are allow-listed per queue.** A run sees no MCP server by
default — every adapter cuts it off from the developer's own
(`ark:rein#21`, `#22`), because an inherited server acts *as somebody else*. A
queue opts in by name, from definitions in the same file (`ark:rein#40`):

```toml
[mcp_servers.posthog]
url                  = "https://mcp.posthog.com/mcp"
bearer_token_env_var = "POSTHOG_API_KEY"   # a NAME; the agent reads the value

[mcp_servers.docs]
command  = "npx"
args     = ["-y", "docs-mcp"]
env_vars = ["DOCS_KEY"]                    # NAMES passed through to the server

[[queues]]
name        = "mac-claude"
agent_kind  = "claude"
mcp_servers = ["posthog", "docs"]          # every run on this queue gets these, and only these
```

Each adapter translates a definition into its own vendor's shape — a
`--mcp-config` entry with `${VAR}` for Claude, a `mcp_servers` table with
`bearer_token_env_var`/`env_vars` for Codex, an ACP entry for Grok — and the
queue declares each server it can serve as **`mcp:<name>`**, so a packet
requiring `mcp:posthog` is routed to, and passes preflight on, a queue that has
it. A server is left out, and the startup log says why, when the adapter cannot
express it (Grok takes no stdio server and no bearer token, since ACP headers
are literal values Rein would have to read) or when this process lacks an
environment variable it names — a service keeps the environment it was
installed with. For an ordinary queue, secrets never pass through Rein: only
their names do. A scoped queue (next) is the one place Rein reads a value, and
it hands it only to that queue's agent process.

**A queue can be scoped to its own secrets** (`ark:rein#48`). An ordinary queue
reaches the vault broker on PATH (`vault-run`, declared `supabase-vault`) and
the daemon variables it names (see "Plan login only" under Configuration) — which
is fine while every queue is elk-work's own, and wrong once one Mac also serves
a customer's workspace. A queue that declares a secrets map gets exactly those
variables and nothing else:

```toml
[[queues]]
name       = "acme-claude"
agent_kind = "claude"
workspace  = "Acme"
land       = "pr"
mcp_servers = ["posthog"]       # its key comes from the map below

[queues.secrets]
POSTHOG_API_KEY       = "acme-posthog"          # keychain service; account "rein"
AWS_ACCESS_KEY_ID     = "acme-aws/access-key"   # service/account
AWS_SECRET_ACCESS_KEY = "acme-aws/secret-key"
```

The config names keychain items; it never holds a value. Create each item as
the user Rein runs as — `-w` with nothing after it prompts, so the value never
reaches your shell history or a process list:

```sh
security add-generic-password -s acme-posthog -a rein -w
```

(Linux: `secret-tool store --label=acme-posthog service acme-posthog username rein`.
Windows: a generic credential with target `acme-posthog:rein`.)

For a run on a scoped queue, Rein reads the items at the start of the run —
before the worktree, so a missing one leaves nothing behind and the run goes
`stuck` naming the variable and the item — and sets them in the agent
process's environment, where the queue's allowed MCP servers find them too.
The process inherits only system variables (`PATH`, `HOME`, locale, temp,
certificates, proxies, `SSH_AUTH_SOCK`) from the daemon besides. The queue
stops declaring `supabase-vault` and any credential name its map does not
supply, and declares each map name instead; the prompt lists the names the run
holds and tells it nothing else is its to use. `scoped_secrets = true` scopes a
queue that needs no credentials at all. A queue with no map is unchanged.

**No value is logged, prompted or delivered.** Rein moves a value from the
keychain to the agent's environment and nowhere else, and everything it writes
down — the run log, the service log, progress notes, the deliverable — passes a
redactor that replaces any value the agent echoes with `[redacted:<NAME>]`. The
match is literal; a value the agent encodes is not caught.

**It is not a sandbox.** The agent runs as your user, and your user can read
your keychain and use your logged-in tools. The map scopes what Rein hands a
run; isolation that holds against a run trying to reach further is a separate
OS user, or a separate machine, per customer. The detail is in
[`docs/run-loop.md`](docs/run-loop.md#scoped-secrets-a-queue-that-gets-only-its-own-credentials).

**Tools that read credentials themselves: wrap them.** Under the service, a
run's environment is closed — a LaunchAgent does not read your shell profile,
so a key exported in `~/.zshrc` is not there. A tool that must find its own
credential resolves it locally, from the keychain, at the moment it starts;
Elk never brokers one, and Rein only ever passes names or a scoped map:

- **An MCP server** — point the definition at a wrapper that reads the item and
  `exec`s the server, so the key exists only in the server's process:

  ```sh
  #!/bin/sh
  # ~/bin/posthog-mcp — referenced as command = "/Users/you/bin/posthog-mcp"
  POSTHOG_API_KEY="$(security find-generic-password -s posthog -a rein -w)" || exit 1
  export POSTHOG_API_KEY
  exec your-posthog-mcp-server "$@"
  ```

- **The aws CLI** — a profile whose `credential_process` prints the keychain
  item as the JSON the CLI expects, so no key is written to `~/.aws`:

  ```ini
  # ~/.aws/config
  [profile acme]
  credential_process = /Users/you/bin/aws-creds-acme
  ```

  where the script prints
  `{"Version": 1, "AccessKeyId": "…", "SecretAccessKey": "…"}` built from
  `security find-generic-password -w` reads.

On a scoped queue, prefer the map: it is per queue, it is declared, and it is
redacted. A wrapper or `credential_process` is per machine — any run that can
reach the script can use it.

### Watching — `rein tail`

The run loop reports a throttled sample to Elk and drops the rest, so for a
long time the only account of a thirty-seven-minute run was two lines in
`rein.log`. It now writes every event to `~/.rein/log/runs/<run id>.jsonl` —
assistant text, tool calls with their arguments, permission requests,
questions, usage, the result, and its own lifecycle around them — with the
facts a reader needs in a `.meta.json` beside it.

```
rein tail                    every run in flight, all queues
rein tail mac-claude         every run on one queue
rein tail 2e0a3e94           one run, by id or by a unique prefix
rein tail 2e0a3e94 --raw     the same, as the log's own JSON
```

With no argument it is a status block over a scrolling pane, one line per run
— queue, run, direction, elapsed, last event, tokens — refreshed in place.
Assistant text is verbatim, a tool call is one line (`▸ Bash: swift test …`),
and a permission request or a question is highlighted, because those are the
two records that mean the run is waiting on somebody. Plain ANSI, no TUI
framework; not a terminal means plain lines, so `rein tail | tee` works.

A **finished** run replays from its log and exits, which is how to read back
work in a worktree that no longer exists — the log outlives the worktree
deliberately. Logs are pruned with the worktree reaper, newest 50 kept
(`log_keep_runs`), and a live run is never pruned.

`rein tail` reads files and talks to nothing else — not Elk, not the daemon —
so it is safe to point at a machine whose service is mid-run. Taking a run
*over* is `rein attach`. Full detail in [`docs/tail.md`](docs/tail.md).

### Taking over — `rein attach`

A run that is going wrong, or that has asked for something only a person can
decide, needs somebody to be able to step in without killing it.

```
rein attach                  the only run in flight
rein attach mac-claude       the run on one queue
rein attach 2e0a3e94         one run, by id or by a unique prefix
```

The daemon **steps back**: its stall watchdog is held for as long as you are
there, because silence from the agent is now you thinking. It keeps reporting
to Elk throughout, so the run does not lose its fifteen-minute lease while you
read. You type lines; `:allow` and `:deny` answer a permission request the run
would otherwise have refused; `:detach` or Ctrl-D leaves and the daemon takes
over again. The run then finishes as it always would, with one line in the
deliverable saying a person took part — never what they typed, which stays in
this machine's run log.

**No PTY.** A vendor CLI in `-p`/stream-json mode is a pipe, not a terminal,
and every adapter that takes input takes it as text through `Send` — so attach
sends lines, and there is no raw mode, no `SIGWINCH`, and no ConPTY. `rein run`
listens on a unix socket at `~/.rein/run/control.sock` (a named pipe on
Windows); the stream you read is the run log, rendered exactly as `rein tail`
renders it.

Adapters declare whether they can be steered at all. **grok declares `steer:
no`** — ACP has no mid-turn steer — so attaching to a grok run is watch-only
and says so before you type. Full detail, including why a socket beat a
`--resume`, in [`docs/attach.md`](docs/attach.md).

### Serving — the user service

`rein run` in a terminal is for watching it work. The way Rein is meant to be
there is as a **user service**: a LaunchAgent on macOS, `systemd --user` on
Linux, a service that logs on as you on Windows.

```
rein service install    # write the definition, enable it at login
rein service start
rein service status
rein service stop
rein service uninstall
```

It runs `rein run` — every configured queue, daemon mode, no `--once` — as
**you**, with your config and your keychain, because everything it needs
belongs to one person: the queue tokens are in that person's keychain, the
coding agents are logged in as them, `gh` is authenticated as them, and the
repositories are under their home. A LaunchDaemon would be a different user and
would lose all four.

Two things bite, and both are in [`docs/service.md`](docs/service.md) at
length. **PATH is captured at install time**, because a LaunchAgent otherwise
inherits `/usr/bin:/bin:/usr/sbin:/sbin` and can see none of the agent CLIs —
so install a tool somewhere new and reinstall the service. And **the macOS
keychain works from a LaunchAgent** — verified, because go-keyring reads
through `/usr/bin/security` inside the user's login session — but only while
launchd's `SessionCreate` stays false, which is what puts a job in a *new*
security session with no login keychain.

**Concurrency is a ceiling, not a number.** `--max-concurrent` defaults to 4 —
elk's standing cap for a Mac, from the 2026-08-20 freeze — and every claim
takes the smaller of that and what the machine can currently afford: free RAM
over 4 GiB a run, free disk under `work_dir` over 10 GiB a run, floor of one.
The count is logged whenever it changes, with the reason, because a throttled
runner and an idle one look identical from outside. Claims waiting for a slot
stand in line: a Wrangler queue's claim goes first, then a run Elk's review has
reopened, then everything else in arrival order; a run under review gives its
slot back while Elk thinks. A queue whose work is not starting says why on its
heartbeat, as `session.waiting_on`. [`docs/run-loop.md`](docs/run-loop.md)
has the rules.

**A self-upgrade checks the config first.** When a new binary is installed the
service asks it `rein config check --json` against the live config before
draining into it, and stays on the running version — logging why and saying so
in `session.waiting_on` — if the new version would take a repository or a
default away from any queue. Fixing `config.toml` releases the upgrade on the
next check; restarting the service takes the new version as it is. A binary
too old to have the check is restarted into unchecked.
[`docs/service.md`](docs/service.md#upgrading) has the detail.

### Telling Elk what the machine looks like

Every heartbeat carries a **fleet reading**: two optional objects Elk stores
verbatim for 24 hours, one describing the machine and one describing the
session on that queue.

```
host    macOS 26.1, Apple M2 Max, 12 cpu, load 4.1, 14.2 GiB of 32 free,
        288 GiB of 994 free under work_dir, rein v0.1.1, 3 of 4 slots, held
        down by memory
session running, 8 minutes in, claude-opus-5, 125k of 1M context,
        five_hour 43% (resets 02:40Z), seven_day 67%, no MCP servers,
        1.28M cache-read tokens spent, land pr
```

`session.land` is the queue's landing policy, on every beat whether or not a
run is in flight: configuration rather than a measurement, so it is always
known.

Two rules shape all of it.

**A key that could not be measured is omitted, never sent as zero.** Elk
renders "could not measure" and "zero" differently and they mean opposite
things: Windows publishes no load average at all, while a load average of 0.0
is a machine doing nothing. Every measurement travels behind a known-flag and
every number on the wire is a pointer, so a reported zero survives the encoding
and an unmeasured one does not appear. The same reasoning makes an **empty** MCP
list different from a missing one — and empty is the true answer for a
Rein-driven Claude run, which starts with `--strict-mcp-config` and an explicit
empty server set.

**The reading rides the beat, and one goroutine beats for a queue that has gone
quiet.** A queue's own loop stops beating for the whole of a run — `serve`
blocks inside `claimAndDrive` — so the queue with the most interesting session
reading in the fleet was exactly the one that could not send it, and had been
showing offline in `list_executors` the whole time it was working. The
telemetry tick is a backstop rather than a duplicate: a queue that beat inside
the interval is left alone, so an idle machine sends exactly the beats it
always did.

```toml
[telemetry]
interval = "60s"   # how often the machine is measured, and how long a queue
                   # may go unheard from before the tick beats for it
disabled = false   # true stops Rein DESCRIBING the machine, not being one
```

`rein status` prints the same `host` half, measured now, so you can see what
Elk will be told without going to look at a fleet page. What each platform can
answer:

| | macOS | Linux | Windows |
|---|---|---|---|
| OS name and version | `kern.osproductversion` | `/etc/os-release` | `RtlGetVersion` |
| CPU model | `machdep.cpu.brand_string` | `/proc/cpuinfo` | registry `ProcessorNameString` |
| 1-minute load | `vm.loadavg` | `/proc/loadavg` | **none — omitted** |
| total / free RAM | `hw.memsize` / `vm_stat` | `/proc/meminfo` | `GlobalMemoryStatusEx` |
| total / free disk | `statfs` | `statfs` | `GetDiskFreeSpaceEx` |

No new dependency for any of it: `release.yml` disables cgo so the darwin
binaries cross-build, so every measurement is a `golang.org/x/sys` syscall, a
file read, or the one subprocess macOS free memory has always needed.

### Subscription headroom, and backing off

Every queue runs on a person's own subscription, and a subscription has a
window. Until `ark:rein#40 (01M3QMT4XQZV6RW4XFK3NYT1C2)` Rein did not notice
when one ran out: it kept claiming runs, and every run failed on its first
request until the reset. Now each queue keeps its last subscription reading,
reports it on every beat — idle ones too — and **stops claiming** while the
agent has said the window is spent.

| Agent | What it can say | How Rein reads it |
|---|---|---|
| Claude | `rate_limit_event`: status, `resetsAt`, each window's utilization | only on a live run's stream. There is no turn-free read — `/usage` is itself rate-limited, and a "cheap" idle call would still be a real turn billed to the window it is measuring — so the reading is **carried forward** between runs, with `sampled_at` saying how old it is. |
| Codex | `account/rateLimits/read` and `account/rateLimits/updated`: used percent, window length, reset time, `ordinaryUsageAllowed` | over `codex app-server`, **with no turn** (0.7 s on a Pro login). An idle Codex queue re-reads every 30 minutes, and before it claims again after a hold. |
| Grok | nothing readable | a turn that ends on the `StopFailure` hook with `error: rate_limit` — Rein installs that hook in the run's private `GROK_HOME` and reads what it records. |

A queue is **exhausted** only on the agent's own word — Claude's `rejected`
(and not while extra usage is paying for the request), Codex's
`ordinaryUsageAllowed: false`, a `rateLimitReachedType` or a full window with no
credits behind it, Grok's classified `rate_limit` — never on a percentage Rein
judged close enough. Being close is the router's business, not the runner's.

The hold lasts until the reset the agent named. Where it named none: a Codex
queue re-reads at the next refresh; any other queue waits for the person's
stated weekly reset,

```toml
[[queues]]
name         = "mac-grok"
agent_kind   = "grok"
weekly_reset = "fri 17:00 America/Los_Angeles"   # weekday, HH:MM, optional IANA zone
```

and without one, 24 hours. The run that discovers the window is spent is
submitted `stuck` with that said first — it failed for want of a subscription,
not for anything in its work order, so it can be sent again. The hold survives
a restart (`~/.rein/state/headroom/<queue>.json`), so a service restarted while
a subscription is out does not claim straight back into it.

On the wire, in the `session` half of every beat:

```
exhausted_until   RFC3339, only while the queue is holding
exhausted_reason  why, and on what basis the time was chosen
limits.<window>   utilization 0..1, resets_at, window_minutes (Codex)
rate_limit        status, type, resets_at, source, sampled_at, exhausted
```

Elk stores the `session` object verbatim, so these reach it with no server
change; `list_executors` renders `limits.five_hour` and `limits.seven_day`
today, and reading `exhausted_until` is the router's half (`ark:scout#2003`).
A Codex window whose stated length is five hours or seven days is reported
under those names, so Elk shows it without learning Codex's slot names.

### What `rein status` tells you

Everything local, and nothing over the network unless you ask:

- the config path, the Rein home, the host label and the machine id;
- every enrolled queue, its agent kind, workspace and landing policy;
- whether that queue's token is in the keychain — never the token itself;
- whether the adapter for that agent kind can actually run here: `ready`, an
  actionable preflight failure, or `no adapter registered` when this build has
  no adapter for that kind at all;
- the machine as the fleet reading describes it — OS, CPU, memory, disk, load,
  and how many runs it can currently afford — with an em dash for anything this
  platform cannot measure, never a zero;
- the environment capability names this machine holds and how each was
  established — the half of a packet's `required_capabilities` that depends on
  what is installed and logged in rather than on the code;
- each queue's subscription as the running service last knew it — `exhausted
  until …`, or the windows and how long ago they were read, or a dash when no
  run has reported one yet.

`--online` also asks Elk (`list_executors`) which queues it can see, whether
they are online, and how much work is waiting.

### Configuration

One directory holds everything Rein owns — config, worktrees, and the
file-backed token fallback. It is resolved in this order:

| | |
|---|---|
| `$REIN_HOME` | if set — how the tests and CI isolate themselves |
| `~/.rein` | the default, on macOS, Linux and Windows alike |
| `os.UserConfigDir()/rein` | only when the home directory is undiscoverable |

The config file is `<home>/config.toml`, written `0600`:

```toml
host_id    = "mac"          # short label; the readable half of a queue name
machine_id = "9f1c…"        # opaque, minted once; Elk stores it as host_id
workspace  = "Elk Scout"    # default Elk workspace for queues below
work_dir   = "/Users/you/.rein/work"  # per-run git worktrees
log_keep_runs = 50          # run event logs kept under <home>/log/runs

[elk]
mcp_url = "https://<project>.supabase.co/functions/v1/elk-mcp"

[telemetry]                 # the fleet reading; see "Telling Elk what the
interval = "60s"            # machine looks like" above
disabled = false

inherit_env = ["SENTRY_ORG"]  # optional; daemon variables every run keeps

[[queues]]
name       = "mac-claude"   # at most 12 bytes; over the cap is a rejection
agent_kind = "claude"       # the adapter this queue drives
model      = "claude-opus-5-5"  # optional; the CLI's default when unset
effort     = "high"         # optional; the CLI's default when unset

[[queues]]
name       = "mac-codex"
agent_kind = "codex"
workspace  = "signal"       # optional; overrides the default above
land       = "pr"           # optional; merge (the default) | pr | branch

[queues.secrets]            # optional; scopes the queue above to these
POSTHOG_API_KEY = "signal-posthog"   # NAME = keychain item, never a value
```

**Plan login only.** Every run draws on the developer's own subscription — the
same limits the terminal uses — and Rein refuses anything that would move it
onto metered API billing:

- **Every run's environment is scoped.** A run inherits the system variables
  in `internal/secretenv` (`PATH`, `HOME`, locale, temp, certificates,
  proxies, `SSH_AUTH_SOCK`, toolchain locations such as `DEVELOPER_DIR` and
  `JAVA_HOME`) plus, for a queue without a secrets map, the daemon variables
  it names: `inherit_env` (top-level and per queue), credential-shaped
  `capabilities`, and its allowed MCP servers' keys. Nothing else the daemon
  holds reaches the agent. Before this, such a queue inherited the daemon's
  whole environment — harmless under the service, which carries little more
  than `PATH` and `HOME`, but a `rein run` started from a shell handed every
  run that shell, API keys included.
- **Metered keys are refused everywhere**: `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`, `OPENAI_API_KEY`, `CODEX_API_KEY`, `XAI_API_KEY`, and
  the `CLAUDE_CODE_USE_BEDROCK` / `_VERTEX` / `_FOUNDRY` switches. No run
  inherits one; config naming one in `inherit_env`, `capabilities`, a secrets
  map or an MCP server is refused at load; and every adapter refuses a spec
  carrying one. The Claude adapter also refuses a repository whose loaded
  settings set `apiKeyHelper` or put a metered key in their `env` block.
- **The start-up line is checked.** Claude Code's `system/init` must say
  `apiKeySource: "none"`; Codex's `account/read` must say account type
  `chatgpt`; Grok's `initialize` must name the `cached_token` (grok.com)
  sign-in. Anything else — including a line that does not say — stops the
  session before its first tool call, and the run is reported `stuck` as
  *"The agent was not on the plan login, so Rein stopped it"* with the reason.

**Model and effort, per queue.** `model` and `effort` are passed to the CLI
verbatim: Claude Code `--model` / `--effort` (low, medium, high, xhigh, max),
Codex the thread's and turn's `model` and the turn's `effort` (minimal, low,
medium, high, xhigh), Grok `--model` / `--reasoning-effort` (low, medium,
high, xhigh). Rein checks only their shape — vendors add levels on their own
schedule — so a level the CLI does not know fails the run with the CLI's own
message. A queue that sets neither keeps the CLI's defaults; the startup log
prints what each queue uses.

Queue names are `<host_id>-<agent_kind>` by convention and capped at **12
characters**. A name over the cap is rejected, never truncated — ruling 3 in
elk's `docs/rein.md`, because the name is the address `claim_run(queue: …)`
resolves and two names cut to the same prefix would be one queue draining two
machines' work.

`host_id` and `machine_id` are different things and both are needed:

- **`host_id`** is a short label chosen by a person. It exists so `mac-claude`
  reads as something, and it can change.
- **`machine_id`** is minted once by `rein enrol` and never changes. It is what
  Elk stores as an executor's `host_id`, because Elk's durable identity is
  `host_id` + `agent_kind` — a value that survives a hostname change, so a
  laptop and a desktop that both call their queue `codex` stop being one row.
  A machine that regenerated it would split its own history in Elk.

### Talking to Elk

`internal/elk` is the whole of Rein's Elk surface: a small MCP client over the
connector's streamable-HTTP JSON-RPC, with typed wrappers for
`connect_executor`, `claim_run`, `report_progress`, `submit_deliverable`,
`heartbeat_executor` and `list_executors`.

Three things it exists to get right, all of them cases where the obvious
reading of a reply is wrong:

- **A cancelled run is not an error.** `report_progress` and
  `submit_deliverable` against a run that is no longer running answer with
  plain text and no `isError` at all — deliberately, so a heartbeat always
  stays open. The wrappers detect it and return an error matching
  `elk.ErrCancelled`, so a run loop stops instead of burning tokens on work a
  human dropped.
- **A missing tool is `-32602`, not `-32601`.** Elk answers `unknown tool: …`
  with the invalid-params code; `-32601` is a whole tool *group* a deployment
  has not wired. `elk.IsUnknownTool` covers both, which is what lets Rein call
  `heartbeat_executor` against an Elk that has not shipped it and carry on.
- **A reply may carry a one-time workspace notice.** Elk prepends it to the
  first successful workspace-resolving reply for a token — very often the
  `claim_run` that fetches a work order. The client splits it off into
  `ToolResult.Notices` and logs it, so an announcement never arrives at an
  agent looking like part of its packet.

`internal/elk/elktest` is an httptest fake of the same endpoint, for tests
here and in the run loop.

### Tokens

The config holds no secrets. Run-scoped Elk tokens live in the OS keychain
(Keychain, Credential Manager, Secret Service) under the service name
**`rein`**, one per `(workspace, queue)` — so revoking one queue's token
leaves the others alone.

`REIN_KEYRING=file` switches to a `0600` JSON file at `<home>/tokens.json`,
for CI and tests. It is never selected implicitly: a box with no keychain
gets an error naming that variable rather than a silent downgrade to
plaintext on disk.

## Adapters

One adapter per coding agent, each in `internal/adapter/<kind>/`. The contract
they implement — the `Adapter` and `Session` interfaces, the closed capability
vocabulary, and the **fail-closed** manifest that refuses a run before a
worktree exists — is [`docs/adapters.md`](docs/adapters.md).

`internal/adapter/fake` is a scriptable test double that enforces the same
rules, for the run loop and for anyone testing a caller.

## Building

```
go build -ldflags "-X main.Version=$(git describe --tags)" -o rein ./cmd/rein
```

Releases are cut by tagging `v*`: `.github/workflows/release.yml` builds
`darwin/{arm64,amd64}`, `linux/{amd64,arm64}` and `windows/amd64` and uploads
each with a `.sha256` beside it. Like ark, Rein is consumed as a pinned
released binary — sessions never build it from source.

The per-PR gate, `.github/workflows/ci.yml`, runs gofmt, `go vet`, `go build`
and `go test` on **ubuntu, macOS and Windows**, with `go test -race` on the
Linux leg. All three, because the platform-specific code — a launchd agent, a
Windows service, three keychain backends, three memory probes — is where the
risk is, and cross-building it in a release job would find the compile errors
after the tag rather than before the merge.

## Work records

Rein's tasks and runs live in **Ark**, not GitHub issues. `.ark/` is
gitignored, so the repository ID is recorded here and in `docs/rein.md`;
never run `ark init` in a clone — join with
`ark init --repository <id>` then `ark remote set <url>` and `ark sync`.

- Ark repository: `01M17TTM53XJKBZW2M9E04HHYP`
- Remote: `https://ark.elk.work`

Named 2026-08-29. Drover and Outpost were the alternatives.

## Wrangler queues

The Wrangler is Elk's PM agent — on a workstation, a Rein queue that runs the
PM loop (elk `docs/wrangler.md`). It was called the PM driver until 2026-10-05.

**Wrangler queues are an explicit opt-in.** In `~/.rein/config.toml`:

```toml
[[queues]]
name = "mac-claude"
agent_kind = "claude"
wrangler = true
# Optional secret NAME; defaults to <workspace>/<queue>.
wrangler_connector_account = "my-wrangler-owner"
```

`pm = true` and `pm_connector_account` are the old spellings and are still
accepted, with identical behaviour; a config that uses them needs no change.
Both spellings may be set if they agree; a queue where they disagree (say
`wrangler = false` beside `pm = true`) is refused at load, never guessed.

Claude and Codex queues may be Wrangler queues. It is off by default. A
Wrangler queue advertises `pm` and `mcp:elk` — **the capability on the wire is
still `pm`**, the name Elk routes on — and neither `pm` nor `wrangler` can be
declared through the general capability lists to bypass the opt-in. Do not set
`mcp_servers` on a Wrangler queue: its Wrangler cycles receive exactly one
server, its owner's Elk connector, and its other runs receive none.

Bind that owner's connector URL in the OS keychain under service
`elk-connector-url`, account `wrangler_connector_account` (or the default
`<workspace>/<queue>`). Choose a distinct binding for each owner. Configure the
item using your keychain's secure UI; never put the URL in TOML, command arguments,
or a work order. Rein resolves this name at each session start, including resumes;
a missing or invalid binding refuses the session. This is the keychain option,
not a new `vault-run` output protocol: existing `supabase-vault` capabilities
still name the workstation's wrapper and do not give Rein its values.

The connector configuration is a 0600 file in Claude's private temporary
directory outside the worktree, removed on session exit or start failure. Only
the file path reaches argv. Connector URLs echoed by Claude are scrubbed before
decoding events and stderr, so they do not reach Rein's run log. Abrupt process
termination can prevent cleanup; ordinary exit, cancellation and failure clean
up. Keep the machine's temporary directory outside run worktrees.

Codex sends the connector as a URL MCP server in its thread configuration,
over the app-server pipe. Rein scrubs echoed connector URLs from events,
startup errors and session results before reporting them. Wrangler queues can
set `secrets` or `scoped_secrets` to inherit only system variables and their
own secret map; the owner connector remains a separate keychain binding.

**The rule and the connector are granted per run, to Wrangler cycles only.** A
Wrangler cycle is a run whose packet requires `pm` — Elk's `pm_cycle_packet`
sets that for a cycle on an agent queue — claimed on a queue with the opt-in.
Any other run on a Wrangler queue, such as an ordinary build run on a queue
that is also a build queue, gets the ordinary prompt and no MCP servers, exactly
as on a queue without the opt-in: no Wrangler rule, no owner connector, and no
`pm` or `mcp:elk` in its preflight list, so a build packet that requires
`mcp:elk` there is refused rather than handed the owner's connector. A packet
requiring `pm` on a queue without the opt-in is still refused at preflight. The
queue keeps advertising `pm` and `mcp:elk` either way: the opt-in decides which
queues may run cycles, the packet decides which runs are cycles
(`ark:rein#50 (01M46ZQDVPJY6VF1HEGQ4ZFHKF)`; until v0.7.1 every run on the
queue got both).

Wrangler cycle prompts authorize migrations only by scout `supabase/README.md`
rule 5, verified by object; edge functions only via scout
`scripts/deploy-functions.sh`; and Signal only through its deploy workflow
between refresh passes. Migration headers with a line starting `-- HOLD:` are
never applied, and `elk-work/website` is never merged. Wrangler cycles follow
elk `docs/wrangler.md`. Other queues, and other runs on a Wrangler queue, retain
their existing prompt and MCP allow-list behavior.

## License

Rein is licensed under the [Apache License, Version 2.0](LICENSE).
Copyright 2026 NOIR Labs, LLC.

To report a vulnerability, see [SECURITY.md](SECURITY.md).
