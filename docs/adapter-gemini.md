# The Gemini adapter

`internal/adapter/gemini` drives Google's **Gemini CLI** (`gemini`) for agent
kind `gemini`, as it is already set up on the person's computer, on their own
Google AI Studio or Google Cloud key. Checked against Gemini CLI 0.64 on macOS,
2026-11-04.

## The person's own key, like Codex and Claude Code

Casey's ruling, 2026-11-11: Rein starts the person's installed Gemini CLI with
the setup it already has, the way the Codex and Claude Code adapters do. Elk
installs nothing and adds no key step. Google allows this on your own Google AI
Studio or Google Cloud key.

A personal Google sign-in is never driven by Rein in any form, and Rein never
starts **Antigravity**: a person who uses it adds Elk to Antigravity as a
connector, the way they add Elk to Claude. The Antigravity driver of
2026-11-10 (#78) and Elk's `antigravity` switch are removed; neither shipped
in a release.

## Which setup a run goes on

A run goes on a key, and only on a key (`keyAuth` in `home.go`):

| The person's Gemini CLI | The run |
|---|---|
| settings choose the Gemini API key or Vertex AI | runs on that choice |
| settings choose a Google sign-in, or nothing, and `~/.gemini/.env` is there or a key is stored for Rein | runs on the Gemini API key sign-in |
| no key anywhere | refused: **"Set up Gemini CLI on this computer with your own Google AI Studio or Google Cloud key first."** |

The run's settings carry that one choice and enforce it (`enforcedType`), and
nothing else of the person's `security.auth`. Gemini CLI then refuses to start
on any other sign-in, whatever else would choose one; unattended, it takes the
settings' choice before the environment's (its `validateNonInteractiveAuth`,
read 2026-11-11). Rein looks only at whether `.env` exists, never inside it.
The refusal matches
`adapter.ErrLoginRequired` and `adapter.ErrPreflight`, so Elk hears
`agent_login`, as for an expired Claude Code sign-in; Preflight says the same.
Not installed is the same line, and so is a run Gemini CLI ends because the
key it was set to use has gone missing ("you must specify the GEMINI_API_KEY").

Four more guards keep a run off a Google sign-in:

- **A repository's own settings.** Gemini CLI layers a worktree's
  `.gemini/settings.json` over the user's (measured: a checked-in
  `enforcedType: oauth-personal` opened Google's sign-in page). A repository
  whose settings choose anything but the Gemini API key, or that Rein cannot
  read, is refused before anything starts (`checkWorkspaceAuth`, `ErrNotPlanAuth`).
  A repository's `.env` can still set `GOOGLE_GENAI_USE_GCA`; that is harmless,
  because the enforced choice outranks the environment (measured on the lab
  Mac with it in the person's own `.env`: the run stayed on the key).
- **This computer's system settings.** Gemini CLI applies its system settings
  over every other layer (macOS `/Library/Application Support/GeminiCli/settings.json`,
  Linux `/etc/gemini-cli/settings.json`, Windows
  `C:\ProgramData\gemini-cli\settings.json`). They are held to the
  repository's rule (`checkSystemAuth`), except that Vertex AI is allowed there:
  it is a Google Cloud key, and whoever set it owns the computer.
- **The environment.** `GOOGLE_GENAI_USE_GCA`, which picks a Google sign-in,
  never reaches a run, whatever the daemon or a queue says; nor do
  `GEMINI_CLI_SYSTEM_SETTINGS_PATH`, `GEMINI_CLI_SYSTEM_DEFAULTS_PATH` and
  `GEMINI_CLI_AUTH_OVERRIDE`, which would point it at settings Rein never
  checked.
- **A sign-in prompt.** A session that prints Gemini CLI's sign-in prompt
  ("Opening authentication page", "Please visit the following URL to
  authorize") is stopped at once and fails as not set up, rather than wait for
  a person who is not there.

## A stored key, if there is one

A person's key in their own shell never reaches a Rein service, and no run
inherits `GEMINI_API_KEY` or `GOOGLE_API_KEY` from the daemon (both are
metered names in `internal/secretenv`). Gemini CLI's own `~/.gemini/.env` is
where its key normally lives, and the private home links it (below). A key
stored for Rein is the other way:

- Stored with `rein secret set GEMINI_API_KEY` (value on stdin), keychain
  service `elk-secret`. Optional: nothing asks for it.
- Read by the run loop for a gemini queue alone, handed to the adapter in
  `RunSpec.ProviderKey`, never `Env`, and set on Gemini CLI's process as
  `GEMINI_API_KEY`. Never on a command line, in a log line, or in a file; the
  run's redactors carry it.
- No other agent, task process or queue environment can carry a Google key:
  both names stay refused everywhere else.

## The private home

Each run gets a private Gemini CLI home (`GEMINI_CLI_HOME`, under
`<rein home>/gemini/runs/`), removed when the session ends, for the reason
Codex runs get a private `CODEX_HOME` (`ark:rein#21`): the person's own MCP
servers, the Elk connector among them, act as the person and must not reach a
run. What comes in from the person's own home (`$GEMINI_CLI_HOME` or `~`) is
their key setup and nothing else:

- `settings.json` carries the run's `security.auth` (table above), automatic
  updates off, and the run's own MCP servers.
- `.env` is linked, not copied (on Windows a hard link where a symbolic link
  needs a privilege). The files that hold a Google sign-in
  (`gemini-credentials.json`, `oauth_creds.json`, `google_accounts.json`) are
  never linked. On a Mac Gemini CLI keeps a Google sign-in in the keychain,
  which a private home does not hide; the run's settings never choose that
  sign-in, and the guards above keep anything else from choosing it.

`HOME` is left alone, so git, gh and SSH are the person's own as for every
other adapter. The child's environment is the scoped inheritance and the
queue's variables as they are, minus the names above, plus the private
home, trust and a stored key, which nothing can shadow.

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

`gemini --version` answers, and Gemini CLI is on a key (table above).
Presence-only: Gemini CLI has no command that checks a key without sending a
prompt. A bad key surfaces as the run's own error, in Google's words.

## Not yet

- **resume** — each run's home is private and removed, and Gemini CLI keeps
  its sessions there (`ark:rein#120`, closed with that reason).
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
  project), sent to the same endpoint, so it works as `GEMINI_API_KEY`. Not yet
  run with a real company key.
- **Vertex AI.** A person whose Gemini CLI is set up for Vertex AI in their own
  settings runs on it, provided `GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION`
  and Google Cloud's own credentials reach the service (`inherit_env` on the
  queue, or their `.env`). `GOOGLE_API_KEY` in the daemon's environment is a
  metered name and never reaches a run; in their `.env` Gemini CLI reads it
  itself. Not run with a real company account.

## Checking it

Measured on the lab Mac, 2026-11-11, with Gemini CLI 0.68.0 and a key that is
not a key (`TestLiveTheKeyComesFromItsEnvFileNeverASignIn`): a person whose
settings choose a Google sign-in and whose `.env` holds the key, even with
`GOOGLE_GENAI_USE_GCA=true` in it too, runs on the key: Google refused it as
"API key not valid", and no sign-in was started. A person set to the Gemini
API key with no key anywhere (`TestLiveAMissingKeyIsNotSetUp`) gets the
not-set-up line from Gemini CLI's own refusal.

`go test ./internal/adapter/gemini/` runs against a stand-in program, on
macOS, Linux and Windows in CI; the tests run with an empty HOME so the
machine's own Gemini CLI setup is never read. `REIN_GEMINI_LIVE=1 go test
-tags integration ./internal/adapter/gemini/ -run Live` runs the real Gemini
CLI: it sends one short prompt to Google with a key that is not a key and
expects Google's own refusal, and checks the worktree is trusted. Add
`REIN_GEMINI_LIVE_KEY_FILE=<file holding a real paid key>` to send one small
task on that key and check the file and commit it makes — the lab proof for
Windows (`ark:rein#151`). An agent never signs in to Google.
