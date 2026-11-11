# The Gemini adapter

`internal/adapter/gemini` drives Google's two coding programs for agent kind
`gemini`: **Antigravity**'s command-line program (`agy`) where it is
installed, and **Gemini CLI** (`gemini`) otherwise, each on the person's own
existing setup. Gemini CLI checked against 0.64 on macOS, 2026-11-04;
Antigravity against 1.5.5 on macOS, 2026-11-10 and 2026-11-11.

## The person's own setup, like Codex and Claude Code

Casey's ruling, 2026-11-11: the person already has Antigravity or Gemini CLI
installed and set up, and Rein starts it with whatever sign-in or key it
already has. Elk installs nothing, asks for no key and manages no sign-in. He
accepted, for the person's own account, the risk Google's Antigravity FAQ names
for driving `agy` from another program on a consumer plan
(antigravity.google/docs/faq: "not supported under consumer plans" and "can
result in suspension or termination of your account").

This replaces the key-only design of 2026-11-04 to 2026-11-10 (rein#78): no
key is required, neither program is moved onto a key or off a sign-in, nothing
that picks a sign-in is stripped from the environment, a repository's own
Gemini settings are not refused, and Elk's `antigravity` switch is gone (it
never shipped in a release).

## Which program

A new run is Antigravity where it can be: a Mac or Windows computer (not
Linux, until its sandbox is measured there), `agy` 1.5.5 or later installed
(the one measured), and, on Windows, a build whose sandbox runs. Otherwise
Gemini CLI runs. The program is chosen once when a run starts and held for that
run. The program version Rein reports says which, `1.5.5 (Antigravity)` or
`0.64.0 (Gemini CLI)`, and every run's first event says "Running Antigravity on
this computer's own Antigravity setup". A follow-up round continues a
conversation only on the program that started it: Antigravity's conversation
ids are marked `agy:`, and a mismatch is refused rather than started over.

## Not set up

Headless, neither program can finish a Google sign-in. Measured on the lab
Mac, 2026-11-11, with `agy` 1.5.5 installed and not signed in:

- `agy --print=…` prints "Authentication required. Please visit the URL to log
  in", a Google sign-in address, "Waiting for authentication (timeout 60s)",
  and then fails: `authentication failed or timed out`.
- Fed its prompt on stdin as stream-json, as Rein runs it, it does not wait:
  the turn ends in about 0.4 seconds with status `ERROR` and
  `authentication failed or timed out`.
- `agy models` prints "Please sign in to view available models" and exits 0.

So Preflight asks `agy models` and fails as **"Set up Antigravity on this
computer first."** when it says to sign in; a turn that ends with that error
fails the same way; and a session that prints the sign-in prompt is stopped at
once, rather than wait for a person who is not there. All three were run live
on the lab Mac (`TestLiveAgyNotSignedInFailsAsNotSetUp`, 2026-11-11). Both match `adapter.ErrLoginRequired`, so Elk
hears `agent_login`, as for an expired Claude Code sign-in. Gemini CLI's
preflight is presence-only (below), and its sign-in prompt ("Opening
authentication page", "Please visit the following URL to authorize") stops a
session the same way, as **"Set up Gemini CLI on this computer first."** Not
installed is the same line, with the program's name.

## A stored key, if there is one

A person whose program runs on a Google key keeps it in their own shell, which
a Rein service never sees, and no run inherits `GEMINI_API_KEY` or
`GOOGLE_API_KEY` from the daemon (both are metered names in
`internal/secretenv`). So a key stored for Rein is used when there is one:

- Stored with `rein secret set GEMINI_API_KEY` (value on stdin), keychain
  service `elk-secret`. Optional: nothing asks for it, and with none the run
  goes ahead on the program's own setup.
- Read by the run loop for a gemini queue alone, handed to the adapter in
  `RunSpec.ProviderKey`, never `Env`, and set on the program's process as
  `GEMINI_API_KEY`. Never on a command line, in a log line, or in a file; the
  run's redactors carry it.
- Antigravity uses it only when the person's own settings say
  `"modelProvider": "gemini"`; Gemini CLI uses it when the person's settings
  choose no sign-in (the key sign-in is then chosen for the run).
- No other agent, task process or queue environment can carry a Google key:
  both names stay refused everywhere else.

## Gemini CLI's private home

Each run gets a private Gemini CLI home (`GEMINI_CLI_HOME`, under
`<rein home>/gemini/runs/`), removed when the session ends, for the reason
Codex runs get a private `CODEX_HOME` (`ark:rein#21`): the person's own MCP
servers, the Elk connector among them, act as the person and must not reach a
run. What comes in from the person's own home (`$GEMINI_CLI_HOME` or `~`) is
their sign-in and nothing else:

- `settings.json` carries only their `security.auth` (or the older top-level
  `selectedAuthType`), automatic updates off, and the run's own MCP servers.
- The files that can hold a sign-in are linked, not copied:
  `gemini-credentials.json`, `oauth_creds.json`, `google_accounts.json` and
  `.env`. On a Mac Gemini CLI 0.64 keeps a Google sign-in in the keychain, which
  a private home does not hide; those files are its fallback and older storage
  (from its own code). Rein never reads them. On Windows a hard link is used
  where a symbolic link needs a privilege.

`HOME` is left alone, so git, gh and SSH are the person's own as for every
other adapter. The child's environment is the scoped inheritance and the
queue's variables as they are, plus the private home, trust and a stored key,
which nothing can shadow.

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

Gemini CLI: `gemini --version` answers, and it is set up — the person's
settings choose a sign-in, their `~/.gemini/.env` is there (Gemini CLI reads a
key from it; Rein looks only at whether the file exists, never inside), or a
key is stored. Presence-only: Gemini CLI has no
command that checks a sign-in without sending a prompt, and its Google sign-in
lives in the keychain, which Rein does not read. A lapsed sign-in or a bad key
surfaces as the run's own error.

Antigravity: `agy` is installed, its sandbox runs here, and `agy models` does
not say to sign in (see *Not set up*). An answer Rein cannot read, or none, is
not held against it; the run will say.

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
  sign-in (`AGY_ADC_AUTH`). A person whose Gemini CLI is set up for Vertex AI
  in their own settings runs on it, provided those variables reach the
  service (`inherit_env` on the queue); `GOOGLE_API_KEY` is a metered name and
  never reaches a run, so Vertex on a key is not covered. Not run with a real
  company account.

## Antigravity (`ark:rein#121`)

### The person's own sign-in or key

Rein's session settings name no model provider, so the person's own
Antigravity settings decide between their saved Google sign-in (kept in the
operating system's keyring) and their key (`"modelProvider": "gemini"` plus
`GEMINI_API_KEY`, antigravity.google/docs/install). `--settings` applies to the
one session and is never saved. Earlier measurements, still true of a person
on a key: the model call carries the key in `x-goog-api-key` and no
`Authorization` header; with `modelProvider` set and no key, agy refuses to
start; a run behind a logging proxy reached only the model, agy's update check
and `play.googleapis.com` (its usage reporting).

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
rule beats an allow rule. Measured with agy 1.5.5 on a Mac (on Windows the lab
proof below runs the same attempts):

| action | result |
|---|---|
| write_file inside the folder | done |
| write_file outside it | refused; the turn ends (below) |
| a command writing inside the folder | done |
| a command writing in the home, outside the folder | "operation not permitted" |
| a command writing git's `hooks` | "operation not permitted" |
| write_file or a command rewriting the worktree's `.git` file, or `commondir` | refused by the deny rule; "operation not permitted" |
| a command renaming the `.git` file, or deleting or renaming `commondir` | "operation not permitted" |
| write_file or a command through a symlink in the folder that points outside it | refused: agy follows the link to the real path |
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
  with the sandbox off. Google's installer (`irm
  https://antigravity.google/cli/install.ps1 | iex` in PowerShell) puts agy in
  `%LOCALAPPDATA%\agy\bin`, where Rein looks when it is not on PATH (on a Mac
  or Linux, `~/.local/bin`). Rules are given with forward slashes; how agy matches
  a Windows path against them, drive letter included, has not been measured.
- **resume** is `yes`, by conversation id (`--conversation`); conversations are
  kept in the person's own Antigravity folder.
- **effort** is passed as `--effort`; **model** as `--model`. On either
  program a model or effort that is not a plain name (letters, digits and
  `._:/@+-`) is refused: on Windows, Gemini CLI's npm shim runs through
  `cmd.exe`, which does not escape special characters.
- **mcp** is `no`: agy takes no server from Rein (no settings key for one), so
  the Elk run connector is not offered, and an Antigravity run on a queue that
  names MCP servers is refused rather than run without them (`ark:rein#120`).
  The person's own servers load, and their tools are denied by rule
  (`mcp(*)`).
- **Not covered by Antigravity's rules:** Rein's own git commands in a
  worktree a run has written (removing it, inspecting it) do not yet turn off
  hooks and fsmonitor or skip submodules the run might have added. That
  hardening is Rein's worktree code, filed separately.
- **Not controlled:** agy checks for updates and reports usage per the
  person's own Antigravity settings; `--settings` has no key for either.

## Proof

- `go test ./internal/adapter/gemini/` — stand-ins for both programs, on
  macOS, Linux and Windows in CI. The tests hide any real `agy` on the machine
  (an empty HOME, PATH without it) and refuse to run if it is still
  reachable: Rein runs `agy` wherever it is installed, and the machine's own
  is signed in to somebody's Google account.
- `REIN_AGY_LIVE=1 go test -tags integration ./internal/adapter/gemini/ -run
  LiveAgy -v` — the real `agy`, playing a person whose own settings put it on
  a key, against a stand-in for the Gemini API (no Google call), every request
  through a logging proxy whose host list is printed: a fake key reaches only the key endpoint; edits and commands stay
  in a linked worktree; a commit lands; a write outside is refused and the run
  is not done. `REIN_AGY_PRIVATE_HOME=1` runs it away from the machine's own
  home; leave it unset on a lab machine signed in to Antigravity, where the
  point is to run beside that sign-in.
- Add `REIN_AGY_LIVE_KEY_FILE=<file holding a real key>` to send one small task
  to Google on that key and check the file and commit it makes.
- `REIN_AGY_LIVE_NOT_SIGNED_IN=1`, on a machine where agy is installed and not
  signed in: Preflight says "Set up Antigravity on this computer first", and a
  run fails the same way within seconds.
- `REIN_AGY_LIVE_SIGNED_IN=1`, on a machine a person has signed in to
  Antigravity: one small task on that sign-in, no key. Signing in is a
  person's step; an agent never signs in to Google.

## Checking it

`go test ./internal/adapter/gemini/` runs against a stand-in program.
`REIN_GEMINI_LIVE=1 go test -tags integration ./internal/adapter/gemini/ -run
Live` runs the real Gemini CLI: it sends one short prompt to Google with a key
that is not a key and expects Google's own refusal, and checks the worktree is
trusted. Add `REIN_GEMINI_LIVE_KEY_FILE=<file holding a real paid key>` to send
one small task on that key and check the file and commit it makes — the lab
proof for Windows (`ark:rein#151`).
