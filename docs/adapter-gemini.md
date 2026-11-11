# The Gemini adapter

`internal/adapter/gemini` drives Google's **Gemini CLI** (`gemini`) on the
person's own **Gemini API key**. Agent kind `gemini`. Checked against Gemini
CLI 0.64 on macOS, 2026-11-04.

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
model, may need an Antigravity CLI (`agy`) adapter later. One was built and
checked against agy 1.3.4 on the same day and is kept, unlanded, on branch
`gemini-antigravity-cli-adapter`.

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

## Not yet

- **resume** — each run's home is private and removed, and Gemini CLI keeps
  its sessions there.
- **the Elk run connector** — not offered, as for Codex and Grok, until
  Gemini CLI's `excludeTools` is verified against Rein's proxy.
- **effort** — Gemini CLI has no effort flag; a queue's effort is noted in the
  run and not applied.
- **Windows** — not measured there; the manifest leaves it out.

## Checking it

`go test ./internal/adapter/gemini/` runs against a stand-in program.
`REIN_GEMINI_LIVE=1 go test -tags integration ./internal/adapter/gemini/ -run
Live` runs the real Gemini CLI: it sends one short prompt to Google with a key
that is not a key and expects Google's own refusal, and checks the worktree is
trusted.

## Model list

Rein reports none. `gemini --help` (checked 2026-11-10) has `-m, --model` and no
command that lists models; a list written into Rein would be a guess.
