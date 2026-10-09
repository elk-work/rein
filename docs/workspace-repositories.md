# Queues and repositories across workspaces

Queue identity is the pair of workspace and name. Two workspaces can both
have `mac-claude`; enrolling the second adds an entry, never replaces the first.
Re-enrolling the same pair with `--force` updates the agent kind and keeps its
repository, secrets, landing policy, Wrangler settings and other local settings.
Keychain accounts remain `<workspace>/<queue>`.

A claim exchange's workspace takes precedence over the config default. An
explicit `--workspace` that disagrees is refused without writing the config or
keychain. Older servers whose reply omits the workspace use the existing sole
workspace lookup; an ambiguous answer requires `--workspace`.

```toml
workspace = "Scout"

[repos]
"elk-work/scout" = "/Users/you/dev/scout"
"friend/gallery" = "/Users/you/dev/gallery"

[[queues]]
name = "mac-claude"
agent_kind = "claude"
repos = ["elk-work/scout"]
repo = "elk-work/scout"

[[queues]]
name = "mac-claude"
agent_kind = "claude"
workspace = "Gallery"
repos = ["friend/gallery"]
repo = "friend/gallery"
land = "pr"
```

The top-level `[repos]` table maps names to checkouts. Each queue's `repos`
list selects keys from that table; unknown keys are configuration errors.
Lists for queues in the same workspace combine into that workspace's repository
set. Only that set becomes `repo:` capabilities on heartbeats and is available
to runs. Repository capability names added manually cannot bypass this filter.

Until a queue declares a list, the config is unscoped: every queue sees the
whole top-level map, and machine-wide `default_repo` applies to a queue with no
`repo`. The first list on any queue scopes the whole config: each queue then
sees only the lists declared in its own workspace, and a workspace with no
lists has no declared repositories. Add lists to every queue in one edit.

## Configs written before lists

Before v0.8.4 there were no lists, so a machine serving several workspaces had
a config like this:

```toml
workspace = "Elk Scout"
default_repo = "/Users/you/dev/elk"

[repos]
"elk-work/scout" = "/Users/you/dev/elk/scout"
signal = "/Users/you/dev/signal"

[[queues]]
name = "mac-claude"
agent_kind = "claude"
wrangler = true

[[queues]]
name = "mac-codex"
agent_kind = "codex"
workspace = "Signal"
```

v0.8.4 read that as "no workspace declares a repository" and dropped
`default_repo`, so after the service restarted into it every run on every
queue ended stuck at preflight — `repository "elk-work/scout" is not declared
for queue "mac-codex" in workspace "Elk Scout"`, and a Wrangler cycle "does
not say which repository it is in" (`ark:rein#67`).

Rein now reads a multi-workspace config with no list on any queue in
**compatibility mode**, by the rules it was written for: every queue sees the
whole `[repos]` map, names resolve by last segment and by local path as before,
and `default_repo` applies to a queue with no `repo`. Nothing is scoped by
workspace, so a run in one workspace can reach another workspace's checkout —
which is why the mode is loud: `rein run` logs a `WARNING` at every start,
and `rein status` and `rein config check` print the same warning.

To leave it, give every queue a list in one edit, and a `repo` to any queue
whose runs name no repository — a Wrangler queue in particular, because a
cycle never names one:

```toml
[[queues]]
name = "mac-claude"
agent_kind = "claude"
wrangler = true
repos = ["elk-work/scout"]
repo = "elk-work/scout"

[[queues]]
name = "mac-codex"
agent_kind = "codex"
workspace = "Signal"
repos = ["signal"]
repo = "signal"
```

Then run `rein config check` before restarting. It prints each queue's
repositories and the checkout a run that names none is cut from, and exits 1
with a `PROBLEM` line for a queue that would strand its runs: a scoped queue
with no repositories and no default, a `repo` its workspace does not declare,
or a Wrangler queue with no default. Adding a list to some queues and not
others is the trap the check exists for — the first list ends compatibility
mode for every workspace at once.

A running service makes the same check before it restarts into a newly
installed binary, and stays on the running version if the new one would take
a repository or a default away from any queue. See
[service.md](service.md#upgrading).

## Scoped resolution

A scoped run must name an exact declared repository (case-insensitive), or its
mapped checkout path. Names with another owner and the same last segment do
not match. An undeclared repository hint or GitHub URL ends the run stuck with
a plain reason; it cannot fall through to a default. A queue's `repo` must also
be declared. Machine-wide `default_repo` applies only to legacy unscoped configs.
Existing single-workspace configs keep their top-level repository mappings and
local path defaults.

On multi-workspace machines, status, live sessions, run logs and subscription
state use workspace-qualified queue labels. Use the label with local commands:

```sh
rein run --queue 'Gallery/mac-claude' --once
rein tail 'Gallery/mac-claude'
rein attach 'Gallery/mac-claude'
rein enrol --revoke --workspace Gallery --queue mac-claude
```

For `run` and enrol revoke, a bare queue name remains accepted when unique.
For tail and attach on a multi-workspace machine, use the displayed qualified
label. Ambiguous names are refused;
revoke requires the workspace to select which keychain account to delete.
