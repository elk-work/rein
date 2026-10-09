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

An omitted or empty list inherits the top-level map only when there is one
workspace and no explicit lists in that workspace. On multi-workspace machines,
a workspace with no lists has no declared repositories. Add explicit lists
before using queues in a second workspace.

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
