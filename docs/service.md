# Running Rein as a service

Rein is meant to be *there*: enrolled once, and then claiming what Elk
dispatches whether or not anybody has a terminal open. `rein service` is how
that happens — a **user service** on each of the three platforms, installed and
controlled by five subcommands.

```
rein service install     write the service definition and enable it at login
rein service start       load and start it
rein service status      installed? running? where are the logs?
rein service stop        stop it, leaving it installed
rein service uninstall   stop it and remove the definition
```

The service runs one command:

```
rein run --service --log-file ~/.rein/log/rein.log
```

Every configured queue, daemon mode, **no `--once`**, the same
`~/.rein/config.toml` and the same keychain as the person who installed it.

Implemented by `ark:rein#8 (01M17TTZP26D58Z0Y4FE3MADWV)`. The takeover hatch,
`rein attach`, is a separate task — `ark:rein#26 (01M1895B4M85C301MB5N90AT96)`.

---

## Why a *user* service, and not a daemon

Everything Rein needs belongs to one person:

| | |
|---|---|
| the Elk queue tokens | that person's login keychain |
| `claude`, `codex`, `grok` | logged in as that person |
| `gh` | authenticated as that person |
| the repositories worktrees are cut from | under that person's home |

A LaunchDaemon or a LocalSystem service is a different user and loses all four.
So on macOS this is a **LaunchAgent** in `~/Library/LaunchAgents`, on Linux a
**`systemd --user`** unit, and on Windows a service that **logs on as you**
(Windows has no per-user services, which is why that one needs a password —
see below).

The library underneath is [`github.com/kardianos/service`], which is what elk's
[`docs/rein.md`] §5a named. `internal/service` wraps it; three of its defaults
were wrong for this job and are overridden.

[`github.com/kardianos/service`]: https://github.com/kardianos/service
[`docs/rein.md`]: https://github.com/elk-work/elk/blob/main/docs/rein.md

---

## macOS

```
rein service install
rein service start
rein service status
```

`install` writes `~/Library/LaunchAgents/work.elk.rein.plist` with `RunAtLoad`
and `KeepAlive` both true — it starts at login and comes back if it dies — and
creates `~/.rein/log/`. `start` is `launchctl load`; `stop` is
`launchctl unload`; `status` reads `launchctl list`.

### Does the keychain work from a LaunchAgent? Yes — verified

This is the question the whole design turns on, because a service that starts
and then cannot read a token is worse than one that does not start.

**It works, unmodified, on macOS 15.** Verified 2026-08-29 on a maintainer's Mac: the
service was installed, started, and its log showed all three queues resolving
their tokens —

```
20:01:27 mac-claude: serving claude in "Elk Scout", permission mode full
20:01:27 mac-codex: serving codex in "Elk Scout", permission mode full
20:01:27 mac-grok: serving grok in "Elk Scout", permission mode full
```

— which is a line the loop only reaches *after* `keyring.Get` has returned that
queue's token. No prompt appeared, and nothing was unlocked by hand.

It works for a reason worth writing down rather than by luck:

- go-keyring on darwin does not link against Security.framework. It shells out
  to **`/usr/bin/security`** (`keyring_darwin.go`), which is also what
  `rein enrol` used to *write* the item. The item's ACL therefore already
  trusts the exact binary doing the read, so there is nothing to prompt about.
- A LaunchAgent runs **inside the user's login session**, and the login
  keychain is unlocked at login. It is a daemon that would be outside it.

And it works only while one option stays off:

> **`SessionCreate` must stay `false`.** It asks launchd to start the job in a
> *new security session*, and a new security session has no login keychain.
> Turning it on is the exact way to produce a service that runs fine in a
> terminal and fails at its first token read. `internal/service` sets it to
> false explicitly, with a comment, so nobody enables it looking for isolation.

**If it ever does fail** — a locked keychain on a machine that boots to the
login window, a keychain reset, an item whose ACL was rebuilt by something
else — the log line is `no stored token … run rein enrol`. The remedy, in
order:

1. `security find-generic-password -s rein -a "Elk Scout/mac-claude" -w` in a
   terminal. If *that* prompts, the item's ACL is the problem, not launchd:
   re-run `rein enrol` for that queue, which rewrites the item through
   `/usr/bin/security` and restores the trust.
2. If the terminal reads it but the service does not, check the plist for
   `SessionCreate` and for a `UserName` key. Either will do it.
3. As a last resort — a CI box, a machine with no GUI login —
   `REIN_KEYRING=file` puts the tokens in `~/.rein/tokens.json` at `0600`.
   That is plaintext on disk and it is a downgrade; it is documented in
   `internal/keyring` and it is not what a developer machine should use.

### The service is also a containment boundary

There is a second reason to run Rein this way, and it is not convenience.

macOS grants Screen Recording, Accessibility and Input Monitoring **per
responsible process**, and a terminal that holds those grants passes them to
everything it spawns. So `rein run` started by hand from a granted terminal
gives every coding agent it drives the ability to capture the screen and drive
other applications — which is the configuration in which, on 2026-08-29, a run
under permission mode `full` took screenshots of the developer's desktop while
he was using it.

A LaunchAgent is **its own responsible process with its own, empty TCC
record**. `screencapture` from it returns a black image and `osascript` aimed at
another app is refused, with no prompt, because a launchd agent has no way to
raise one. The prompt now forbids all of this outright
(`internal/runner/prompt.md`), but a prompt is an instruction and this is a
wall. `docs/run-loop.md` § *What the prompt cannot enforce* has the whole
argument.

Do not "fix" that by granting the Rein agent those permissions.

### PATH is captured at install time

A LaunchAgent inherits `PATH=/usr/bin:/bin:/usr/sbin:/sbin` and nothing else,
which contains **none** of `claude`, `codex`, `grok` or `gh`. A service
installed without help would start, heartbeat, claim a run, and fail preflight
on every one of them.

So `rein service install` copies the **installing shell's PATH** into the
plist's `EnvironmentVariables`, along with `HOME` and any of `REIN_HOME`,
`REIN_KEYRING`, `REIN_HOST_CAPABILITIES` that are set. The install prints
exactly what it captured.

The cost is that the captured PATH is a snapshot. **Install a tool somewhere
new, and reinstall the service** — `rein service uninstall && rein service
install` — or it will not be found.

Nothing else is copied. The environment list is closed on purpose: a plist is
world-readable, so a token that reached it would stop being a secret.

### What the service does *not* get

`SSH_AUTH_SOCK` is not captured, because on macOS it is a per-login-session
path that changes. launchd sets it for agents in the user session, so
SSH-remote pushes work — but if a run reports "Permission denied (publickey)",
that is the first thing to check. HTTPS remotes with `gh` as the credential
helper are unaffected.

---

## Linux

```
rein service install && rein service start
```

Writes `~/.config/systemd/user/work.elk.rein.service` and enables it.

Two things the library gets wrong for a *user* unit, one fixed here and one
you have to do yourself:

- **Fixed:** kardianos writes `WantedBy=multi-user.target`, which is a *system*
  manager target. A user manager has no `multi-user.target`, so the unit would
  be installed, enabled, and never started. `internal/service/fixup_linux.go`
  rewrites that line to `default.target` and re-enables.
- **Yours:** a user manager only runs while that user has a session. For a
  runner on a headless box, `sudo loginctl enable-linger <user>`.

Logs go to `~/.rein/log/rein.log`, plus `journalctl --user -u work.elk.rein`.

---

## Windows

```
rein service install         # from an elevated prompt
rein service start
```

Windows has no user services in this library, so `install` creates a **system
service that logs on as you**. That is not a preference: **Credential Manager
is per-user**, and a service running as `LocalSystem` starts, heartbeats,
claims a run, and then cannot read the token `rein enrol` stored — a failure
three steps from its cause.

Which means the install needs two things a Mac's does not:

- **An elevated prompt.** Creating a service is an administrator action.
- **Your password**, for the SCM to create a service that logs on as you.
  Set `REIN_SERVICE_PASSWORD` rather than passing `--password`, which puts it
  in the process list and your shell history. Rein reads it once and hands it
  to the SCM; it is never written to the config or the log.

```
set REIN_SERVICE_PASSWORD=...
rein service install
```

`--service-user` overrides the account if it is not the current one. The
account also needs the **Log on as a service** right, which the SCM grants on
creation in most configurations and a domain policy can take away.

Windows Phase 1 is `ark:rein#10`; that task is where anything found on a Windows
box belongs.

---

## Concurrency: a ceiling, not a number

The service serves **every** configured queue at once, so how many runs may be
in flight is a real question rather than a theoretical one.

The answer is not a constant. It is the smaller of two numbers, recomputed
every time a queue is about to claim:

| | |
|---|---|
| the **ceiling** | `--max-concurrent`, default **4** |
| what the machine can **afford** | free RAM ÷ 4 GiB, and free disk under `work_dir` ÷ 10 GiB |

Four is elk's standing cap for a Mac, and it comes from the 2026-08-20 incident
where four concurrent agents took a machine's memory out and froze it. But four
is wrong in both directions on the same machine — timid on a 64 GB desktop,
still too many on a laptop with a simulator and two browsers open — so it is
the ceiling and the measurement decides the rest.

A run is budgeted **4 GiB of memory** and **10 GiB of disk under the work
directory**. So the ruling's thresholds fall out of the arithmetic: under 4 GiB
free and the memory term is zero; under 10 GiB free and the disk term is. The
floor is **one slot**, never zero — a runner that claimed nothing would be a
queue that silently stopped draining, which is worse than a run that is short of
memory and says so.

The slot count is logged **whenever it changes**, with the reason:

```
20:01:28 concurrency: 1 slot — held down by 5.0 GiB RAM free (cap 4)
```

That line matters because a throttled runner and an idle one look identical
from outside. So does the one a waiting queue sends Elk on its beat,
`session.waiting_on`, which names the limit, who holds each slot and who is
next in line — a Wrangler queue's claim, ahead of every build. `docs/run-loop.md`
(*Who gets the next slot*) has the order.

How each half is measured, and why not the obvious way:

- **macOS** — `/usr/bin/vm_stat`, summing free + inactive + speculative +
  purgeable. Not `vm.page_free_count`: macOS keeps free memory near zero on
  purpose, so a 24 GB Mac with 5 GB genuinely spare reads under 100 MB there
  and every Mac would clamp itself to one slot forever. Not the Mach
  `host_statistics64` call either — that needs cgo, which `release.yml`
  disables so the darwin binaries cross-build.
- **Linux** — `MemAvailable` from `/proc/meminfo`, the kernel's own estimate,
  falling back to free + buffers + cached on a kernel older than 3.14.
- **Windows** — `GlobalMemoryStatusEx`'s `ullAvailPhys`.
- **Disk**, everywhere — the space available *to this user* (`Bavail`,
  `freeBytesAvailableToCaller`), on the filesystem holding `work_dir`, not `/`.

A platform that cannot measure a half does **not** constrain on it. Not knowing
is not the same as knowing there is nothing spare, and a runner that refused
work because it could not read `/proc` would be failing in the expensive
direction.

---

## Logs

Everything is under `~/.rein/log/`:

| | |
|---|---|
| `rein.log` | the run loop's own log — the one to read |
| `work.elk.rein.out.log` | whatever the process wrote to stdout: panics, mostly |
| `work.elk.rein.err.log` | and to stderr |
| `runs/<run id>.jsonl` | one run's whole event stream — what `rein tail` reads |
| `runs/<run id>.meta.json` | that run's queue, direction, status, worktree, branch |

`rein.log` answers "is the daemon alive and what did it claim"; `runs/` answers
"what did the agent actually do", which is the question `rein.log` was never
going to answer. `rein tail <run id>` renders one; `rein tail` with no argument
is the live view over all of them. See [`tail.md`](tail.md), including how they
are pruned (`log_keep_runs`, newest 50).

The service also listens for a takeover on `~/.rein/run/control.sock` (a named
pipe on Windows), which is what `rein attach` connects to — a startup line says
so, and says why not when the endpoint is already held by another `rein run`.
The runner keeps serving its queues either way; the hatch is not a
prerequisite. See [`attach.md`](attach.md).

`rein.log` is **appended**, not truncated, so a `KeepAlive` restart does not
erase the reason for the last one. `rein service status` prints all three with
their sizes and modification times, which is usually enough to tell which one
has the answer.

A healthy idle log looks like this — the queues coming up, the slot count, and
then a line per queue whenever the depth changes and otherwise once a lease:

```
20:01:27 mac-claude: serving claude in "Elk Scout", permission mode full
20:01:27 mac-claude: host capabilities: claude (claude on PATH), …
20:01:28 concurrency: 1 slot — held down by 5.0 GiB RAM free (cap 4)
20:01:29 mac-claude: online — 0 waiting
```

Nothing rotates them. They are small — an idle queue writes four lines an hour
— but a machine that has been running for a year should be looked at.

---

## Stopping, and what a stop costs

`rein service stop` cancels the loop's context, which interrupts any session in
flight and **submits nothing for it**. Its claim lapses and Elk hands the run to
whoever asks next. That is the same contract as Ctrl-C on `rein run`, and it is
deliberately not "report what we had": an outcome that did not happen is worse
than no outcome.

`uninstall` stops it and removes the definition. It touches nothing else — the
config, the keychain tokens and any worktrees under `work_dir` are left exactly
as they are.

---

## Upgrading

Replace the binary at the installed path. Released services check that path every
60 seconds. When its version changes, every queue stops claiming, heartbeats
continue, and active runs finish, including their Elk review waits. There is no
drain timeout. Rein exits with code 75 once idle; the service manager launches
the new binary. Reinstalling the same version does not restart it. A missing
binary or a failed version probe leaves the daemon running.

To opt out, set `self_restart = false` at the top level of `config.toml` and
restart once to load that setting. Upgrades then require a manual
`rein service stop && rein service start`. Automatic restart defaults to on,
but interactive runs, `--once`, `--dry-run`, and `dev` builds never drain.

A daemon predating this feature needs one last manual restart to pick it up.
Reinstall the service only if the executable path or PATH changed.

`rein service install` refuses if the binary it would install is under the
temporary directory, which is what `go run ./cmd/rein service install`
produces: that service would break the moment the file was cleaned up.

## When something is wrong

| Symptom | Look at |
|---|---|
| `status` says *not installed* after an install | the install printed a path — is the plist/unit there? |
| installed, not running | `~/.rein/log/work.elk.rein.err.log`, then `rein.log` |
| `no stored token … run rein enrol` | the keychain section above |
| every run goes `stuck` at preflight | PATH: `rein service status` prints the captured one |
| nothing in any log, ever | the binary path in `status` — has it moved? |
| the loop is up but claims nothing | `concurrency:` in `rein.log`, and `rein status --online` |
