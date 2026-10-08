# Codex app-server protocol schema

**Generated from the binary, not transcribed from OpenAI's prose docs** — those
are stale (elk `docs/rein.md` §3: `codex exec` has no `--ask-for-approval` or
`--full-auto`, whatever the published docs say).

```
codex app-server generate-json-schema --out <dir>
```

- generated **2026-08-29** from `codex-cli 0.150.1` on darwin/arm64
- `--out` is required; the subcommand takes a directory, not a file

The full dump is 41 top-level files plus 258 under `v2/` (≈3.6 MB). What is
checked in here is the subset the Go types in this package model — enough to
read the schema beside `protocol.go` and check a field, without carrying the
whole protocol surface in the repo. Regenerate the full dump with the command
above whenever a field is missing; it takes under a second.

## What is here

| Path | Holds |
|---|---|
| `JSONRPC*.json`, `RequestId.json` | the envelope. **Note what the wire actually does**: Codex omits `"jsonrpc"` on the frames it sends, so a decoder that requires it rejects every message. |
| `v1/Initialize*.json` | the handshake. `initialize` is still v1; everything else the adapter uses is v2. |
| `v2/Thread*.json`, `v2/Turn*.json`, `v2/Item*.json` | the session lifecycle and the notification stream |
| `*RequestApproval*.json` | the three approval round-trips, client-answered |
| `ToolRequestUserInput*.json` | the interactive-question round-trip this adapter declines — see `docs/adapter-codex.md` |
| `v2/GetAccountRateLimitsResponse.json`, `v2/NullableGetAccountRateLimitsParams.json`, `v2/AccountRateLimitsUpdatedNotification.json` | the subscription window: `account/rateLimits/read` and its notification (`ark:rein#40`). **Generated 2026-09-29 from codex-cli 0.159.0**, unlike the rest. |

## Things the schema does not tell you, which the binary did

Verified by driving the real `codex app-server` on 2026-08-29 (transcripts are
the fixtures in `testdata/`):

- **Framing is newline-delimited JSON**, one object per line. Not
  `Content-Length`. `--listen stdio://` is the default.
- **The server's request ids live in their own space.** It sent `id: 0` for an
  approval while the client was using 1, 2, 3. Match a response to a call by
  your own id; tell a server *request* from a response by the presence of
  `method`, never by the id.
- **`thread/start` really does take MCP servers** through
  `params.config.mcp_servers` — a probe server appeared in
  `mcpServer/startupStatus/updated` beside the developer's own. The
  developer's `~/.codex/config.toml` servers load too, always.
- **`thread/started` carries the rollout path**, which is where
  `Result.TranscriptPath` comes from.
