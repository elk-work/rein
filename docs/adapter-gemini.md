# The Gemini adapter

`internal/adapter/gemini` drives Google's **Gemini CLI** (`gemini`) on the
person's own **Gemini API key**, and, only while Elk's server switches it on,
**Antigravity**'s command-line program (`agy`) on the same key. Agent kind
`gemini`. Gemini CLI checked against 0.64 on macOS, 2026-11-04; Antigravity
against 1.5.5 on macOS, 2026-11-10 (the Antigravity section below).

## Why a key, and why only here

Every other adapter runs on the developer's own plan login and refuses a
metered API key. Gemini is the one exception, by Casey's ruling of
2026-11-04: the person brings their own Google key, it stays on their
computer, Google bills them, and Elk never sees it. There is no plan-login
path instead: Google's terms do not allow another program to drive its agent
on a personal Google sign-in. So Rein never uses a Google sign-in for Gemini.

## Why Gemini CLI, and what that leaves out

Gemini CLI is what Elk's installer sets up (scout `scripts/connect-agent.mjs`),
and Google still documents it on a Google AI Studio key for headless use:

> "If you don't want to authenticate using your Google account, you can use an
> API key from Google AI Studio. … Set the `GEMINI_API_KEY` environment
> variable to your key." — geminicli.com/docs/get-started/authentication

> Gemini CLI "will remain accessible via paid Gemini and Gemini Enterprise
> Agent Platform API keys." — developers.googleblog.com, "An important update:
> transitioning Gemini CLI to Antigravity CLI"

What it leaves out, in Google's words: the Gemini CLI README says "No free
tier or Google AI plans" (free and Google One accounts "moved to Antigravity
CLI on June 18, 2026"), lists Gemini CLI for "legacy paid API / Vertex keys",
and calls it "Not optimized for Gemini 4+"; the authentication page says "To
use Gemini 4 Argon, please use Antigravity." So a free-tier key, or a Gemini 4
model, needs Antigravity, which this adapter now drives behind Elk's switch
(below).

## The key

- Stored once: `rein secret set GEMINI_API_KEY` (value on stdin), keychain
  service `elk-secret`, the same place the Elk app's secrets go.
- Read by the run loop for a gemini queue alone, at the start of each run,
  before the worktree. Missing → the run is stuck with *"Add your Google key in
  Elk to use Gemini."* (stop code `agent_login`), and the queue's setup state
  reads as needing authentication.
- Handed to the adapter in `RunSpec.ProviderKey`, never `Env`, and set on the
  Gemini CLI process as `GEMINI_API_KEY`. Never on a command line, in a log
  line, or in a file. The run's redactors carry it, so an agent that echoes it
  has the echo replaced with `[redacted:GEMINI_API_KEY]`.
- `GEMINI_API_KEY` and `GOOGLE_API_KEY` are metered names in
  `internal/secretenv`: no run of any kind inherits one, config naming one is
  refused, and every adapter refuses a spec whose `Env` carries one. A secret
  of that name chosen in the Elk app is skipped rather than mapped into a
  queue, since a queue naming it would be refused.

## Never a Google sign-in

Each run gets a private Gemini CLI home (`GEMINI_CLI_HOME`, under
`<rein home>/gemini/runs/`), removed when the session ends. Its
`settings.json` sets `security.auth.selectedType` and `enforcedType` to
`gemini-api-key`. Measured: with that and no key, Gemini CLI exits 41 ("you
must specify the GEMINI_API_KEY environment variable") instead of signing in.
The private home also means the person's own Gemini settings, cached sign-in,
extensions and MCP servers — including the Elk connector the installer
registers at user scope — never reach the run. `HOME` is left alone, so git,
gh and SSH are the person's own as for every other adapter.

Two more guards, because a repository's own `.gemini/settings.json` outranks
the user layer (measured: a checked-in `enforcedType: oauth-personal` opened
Google's sign-in page):

- Start refuses a worktree whose `.gemini/settings.json` sets
  `security.auth.selectedType` or `enforcedType` to anything but
  `gemini-api-key`, sets `useExternal`, or cannot be read.
- A session whose stderr says it is opening a Google sign-in is killed at
  once and fails as not on the key.

The child never inherits `GEMINI_*`, `GOOGLE_GEMINI_*`, `GOOGLE_GENAI_*`,
`GOOGLE_CLOUD_*`, `GOOGLE_API_KEY` or `GOOGLE_APPLICATION_CREDENTIALS`, any of
which could change the sign-in, relocate settings or trust, or send the key to
another host.

## Trusted folders (`ark:rein#119`)

In a folder Gemini CLI does not trust, a headless run exits 55 before it
starts and no MCP server connects. Every run trusts its worktree with both of
Google's documented headless switches (geminicli.com/docs/cli/trusted-folders):
`--skip-trust` and `GEMINI_CLI_TRUST_WORKSPACE=true`. Measured: `gemini mcp
list` under a run's environment shows its server enabled; without the switch,
"disabled because this folder is untrusted".

The user-scope `elk-agent` server is deliberately not loaded (above); the run
reaches Elk through Rein, which reports for it.

## Permissions

Headless Gemini CLI cannot ask, so `approvals` is `no` and only two modes are
served:

| Rein | Gemini CLI | measured |
|---|---|---|
| `read_only` | `--approval-mode plan` | `write_file` into the worktree refused |
| `full` | `--approval-mode yolo` | `write_file` and `run_shell_command` ran |

## Events

`--output-format stream-json`: `init` (session id, model), `message`
(assistant text deltas), `tool_use`, `tool_result` (a failed one is reported
as progress), `result` (status and per-model token stats; usage is reported
per model, prompt tokens split into fresh and cached). Fixtures are in
`internal/adapter/gemini/testdata`.

## The probe

Preflight checks that `gemini --version` answers and that the key is stored.
It is presence-only: Gemini CLI has no command that checks a key without
sending a prompt, and Rein does not call Google's API with the person's key
itself. A bad key surfaces as the run's own error, in Google's words ("API key
not valid").

## Not yet, on Gemini CLI

- **resume** — each run's home is private and removed, and Gemini CLI keeps
  its sessions there. Antigravity resumes (below); Gemini CLI, which Google is
  retiring, will not (`ark:rein#120`, closed with that reason).
- **the Elk run connector** — not offered, as for Codex and Grok, until
  Gemini CLI's `excludeTools` is verified against Rein's proxy.
- **effort** — Gemini CLI has no effort flag; a queue's effort is noted in the
  run and not applied.

## Windows (`ark:rein#151`)

Gemini CLI is listed for Windows. It is the same program, installed with
`npm install -g @google/gemini-cli` (a `gemini.cmd` shim, found on PATH or in
`%APPDATA%\npm`), with the same private home, settings and environment; the
environment is filtered without regard to case, as Windows names are; and a
run is stopped by killing its process tree (`taskkill /T`). The package's
session tests use a stand-in built from the test binary, so Rein's Windows CI
runs a whole Gemini CLI session. A real Windows run on a real key is a lab step
(see *Proof* below).

## Company Google Cloud keys

- **A Google Cloud API key with the Gemini API enabled** is the same kind of
  key as a Google AI Studio one (AI Studio makes its keys in a Google Cloud
  project), sent to the same endpoint, so it works as `GEMINI_API_KEY` on both
  programs. Not yet run with a real company key.
- **A Vertex AI key** (Google's Gemini Enterprise Agent Platform) is not the
  same. Gemini CLI reads it as `GOOGLE_API_KEY` and also needs
  `GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION` and the Vertex AI sign-in
  choice (geminicli.com/docs/get-started/authentication); Antigravity's
  command-line program documents no Vertex key at all, only Google Cloud
  sign-in (`AGY_ADC_AUTH`), which is a sign-in and so ruled out. Rein strips
  all of those variables today. Supporting Vertex keys on Gemini CLI needs a
  project and a location from the person as well as the key, which is new
  work in the Elk apps as well as here; it is not done.

## Antigravity, behind Elk's switch (`ark:rein#121`)

Google moved free keys and Gemini 4 models to Antigravity. Its help page says
outside software driving Antigravity breaks its terms on consumer plans, and
recommends a Google AI Studio or Gemini Enterprise key for third-party agents.
Whether Elk may run it on a person's key is the question put to Google, so:

- **The switch is Elk's, and it fails closed.** Each gemini queue's
  heartbeat reply is that queue's say: `"antigravity": true` is on, anything
  else is off, and a say counts for 15 minutes (three heartbeats). The switch
  is on only while at least one say counts and every say that counts is on —
  so it is off at start-up, two Elk servers that disagree leave it off, and
  heartbeats that keep failing let it lapse to off. Another kind of queue's
  reply never touches it. While it is off Rein never executes `agy` — not a
  run, a version probe or a preflight. Google's yes needs a server change, not
  a new Rein.
- **On, a new run is Antigravity where it can be:** installed, version 1.5.5
  or later (the one measured), and, on Windows, a build whose sandbox runs.
  Otherwise Gemini CLI runs as before. The program is chosen once when a run
  starts and held for that run. The program version Rein reports says which
  runs, `1.5.5 (Antigravity)` or `0.64.0 (Gemini CLI)`, and is read again at
  the next heartbeat after the switch flips; every run's first event says
  "Running Antigravity on the person's own Google key".
- **A run under way when it flips** finishes on its program. Its follow-up
  rounds (a review, a missing summary, a stall restart) continue the same
  conversation only on the program that started it: Antigravity's
  conversation ids are marked `agy:`, and a mismatch is refused rather than
  started over with only the follow-up's words.

### The key, never a Google sign-in

agy uses a key only when its settings say `"modelProvider": "gemini"`. Rein
passes that with `--settings`, which applies to the one session and is never
saved, so the person's own `agy` is never moved onto their key. Measured:

- with it, the model call carries the key in `x-goog-api-key` and no
  `Authorization` header;
- with it and no key, agy refuses to start ("modelProvider is set to
  \"gemini\" … but the GEMINI_API_KEY environment variable is not set");
- a run behind a logging proxy reached only the stand-in for the model, plus
  agy's update check and `play.googleapis.com` (its usage reporting) — no
  Google sign-in endpoint. That was on a machine with no Antigravity sign-in;
  the same check on a machine signed in to Antigravity is the lab step below.

A saved Google sign-in lives in the operating system's keyring, not in a
folder, so a private home would not hide it; the session settings are the
guard. The child never inherits `AGY_*` (`AGY_ADC_AUTH` and `AGY_ACCOUNT` pick
a sign-in or Google Cloud billing; `AGY_LLM_GATEWAY_*` reroutes the key),
`ANTIGRAVITY_*`, `JETSKI_*`, `GEMINI_*`, `GOOGLE_GEMINI_*` (the base URL would
send the key elsewhere), `GOOGLE_GENAI_*`, `GOOGLE_CLOUD_*`, `GOOGLE_API_KEY`
or `GOOGLE_APPLICATION_CREDENTIALS`, in any case. A session whose stderr says
it is starting a sign-in is killed and fails as not on the key.

### Edits stay in the task's folder, by Antigravity's own rules

`--dangerously-skip-permissions` is never passed. A full run is
`--mode accept-edits --sandbox` with `toolPermission: proceed-in-sandbox` and
these allow rules: the task folder (under its real path too), and, for the
linked worktree Rein cuts, the worktree's own git folder and the shared
`objects`, `refs`, `logs`, `packed-refs` and its lock — what a commit writes —
plus read access to the whole git folder and `read_url(*)` (the sandbox has
no network otherwise). Git's paths are given under their real names too.

What would let a run plant code that runs outside the sandbox the next time
git runs there — Rein removing the worktree, or a person looking at a kept
one — is never writable. The shared folder's hooks and config are not
granted, and the files that point git at a folder are denied by rule: the
worktree's `.git` file and the private folder's `commondir`, `gitdir` and
`config.worktree` (for a plain repository, its `config` and `hooks`). A deny
rule beats an allow rule. Measured with agy 1.5.5:

| action | result |
|---|---|
| write_file inside the folder | done |
| write_file outside it | refused; the turn ends (below) |
| a command writing inside the folder | done |
| a command writing in the home, outside the folder | "operation not permitted" |
| a command writing git's `hooks` | "operation not permitted" |
| write_file or a command rewriting the worktree's `.git` file, or `commondir` | refused by the deny rule; "operation not permitted" |
| `git commit` from the linked worktree | done |
| `git ls-remote https://github.com/…` | needs `read_url(*)`; done with it |
| reading `~/.gitconfig`, writing `~/Library/Caches` and `$TMPDIR` | allowed by the sandbox |

So "stays in the folder" means the person's files and other projects: the
sandbox still lets commands write the system's temporary and cache folders, as
build tools need.

A refused action ends agy's turn with status `SUCCESS`, an empty answer and
the action in `denied_actions`. Rein reports that as failed, naming the
action, never as done.

Settings given with `--settings` replace the same keys in the person's file
(measured: a user file allowing writes elsewhere with `always-proceed` did not
widen a run), and settings checked into a repository did not widen it either.
agy refuses a settings key it does not know, and it has none for MCP servers
or plugins: the person's own servers start, so their tools are denied by rule
(`mcp(*)`) — a run must not act through the person's own connections, Elk's
among them (`ark:rein#21`). The person's own plugins, skills and hooks still
load, as a person's own setup does for the Claude adapter.

A read-only run is the default mode with the same sandbox and no write
grants: edits ask, which headless means refused, and commands can read but not
write. (`--mode plan` has no effect alongside `--disable-slash-commands`.)

### What else

- **Windows** needs Windows 11 24H2 (build 26100) or later, where the terminal
  sandbox runs; on older Windows, Gemini CLI runs instead, never Antigravity
  with the sandbox off. Rules are given with forward slashes; how agy matches
  a Windows path against them, drive letter included, has not been measured.
- **resume** is `yes`, by conversation id (`--conversation`); conversations are
  kept in the person's own Antigravity folder.
- **effort** is passed as `--effort`; **model** as `--model`. On either
  program a model or effort that is not a plain name (letters, digits and
  `._:/@+-`) is refused: on Windows, Gemini CLI's npm shim runs through
  `cmd.exe`, which does not escape special characters.
- **mcp** is `no`: agy takes no server from Rein (no settings key for one).
  Rein adds the Elk run connector to a queue's runs when it starts, before it
  has heard the switch, so an Antigravity run goes without it and its log says
  so; any other server on an Antigravity run is refused (`ark:rein#120`).
- **Not controlled:** agy checks for updates and reports usage per the
  person's own Antigravity settings; `--settings` has no key for either.

## Proof

- `go test ./internal/adapter/gemini/` — stand-ins for both programs, on
  macOS, Linux and Windows in CI.
- `REIN_AGY_LIVE=1 go test -tags integration ./internal/adapter/gemini/ -run
  LiveAgy -v` — the real `agy` against a stand-in for the Gemini API (no
  Google call), every request through a logging proxy whose host list is
  printed: a fake key reaches only the key endpoint; edits and commands stay
  in a linked worktree; a commit lands; a write outside is refused and the run
  is not done. `REIN_AGY_PRIVATE_HOME=1` runs it away from the machine's own
  home; leave it unset on a lab machine signed in to Antigravity, where the
  point is to run beside that sign-in.
- Add `REIN_AGY_LIVE_KEY_FILE=<file holding a real key>` to send one small task
  to Google on that key and check the file and commit it makes.

## Checking it

`go test ./internal/adapter/gemini/` runs against a stand-in program.
`REIN_GEMINI_LIVE=1 go test -tags integration ./internal/adapter/gemini/ -run
Live` runs the real Gemini CLI: it sends one short prompt to Google with a key
that is not a key and expects Google's own refusal, and checks the worktree is
trusted.
