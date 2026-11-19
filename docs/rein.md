# What this machine offers Elk

Rein tells Elk what this machine can do, and Elk routes work by it: a packet
whose `required_capabilities` name `xcode` goes to a machine that declares
`xcode`. The list is built from three sources — names built into Rein, what Rein
detects at startup (`xcodebuild -version` answering, `gh auth status` passing,
`node` on PATH, …) and `capabilities` in `~/.rein/config.toml`. The detail is in
[`run-loop.md`](run-loop.md#what-elk-is-told).

## Withholding a capability

Detection says what is installed, not what a person wants the machine used for.
A laptop with Xcode installed is detected as able to build the iPhone and Mac
apps, and Elk will send it that work. `withhold_capabilities` is how a person
says no:

```toml
# ~/.rein/config.toml — every queue on this machine
withhold_capabilities = ["xcode"]

[[queues]]
name                  = "mac-claude"
agent_kind            = "claude"
withhold_capabilities = ["docker"]   # this queue only, on top of the line above
```

A withheld name is removed after every other source has had its say, so it
wins over detection, over `REIN_HOST_CAPABILITIES`, and over the same name in
`capabilities`. With `withhold_capabilities = ["xcode"]` in effect:

- **Elk does not see it.** Neither `xcode` nor `tool:xcode` is in the
  `declared_capabilities` Rein sends on `connect_executor` and every
  `heartbeat_executor`, so Elk's executor list (`list_executors`) no longer
  shows this machine as able to build the apps, and Elk stops routing that work
  here. The setting takes effect when Rein next starts.
- **A packet that requires it is refused here.** If one arrives anyway, Rein's
  preflight finds `xcode` missing and ends the run before a worktree is cut or
  an agent starts. The stuck report says the machine withholds `xcode` on
  purpose and that the work belongs on another machine.
- **`rein status` shows it**, on a `withheld` line under the host capability
  list.

Withholding a name the machine does not hold anyway does nothing and is not an
error, so the same file can be copied between machines. The names built into
Rein — `elk-connector`, `git` and `worktree` — are how every run happens at
all and cannot be withheld; the config is refused at load if it tries.

Prefer this to `REIN_HOST_CAPABILITIES`, which replaces all of Rein's detection
with a fixed list and lives in the service's environment, where the Elk Mac app
writes it.
