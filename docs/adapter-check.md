# The check adapter

**`ark:rein#89 (01M5RKG2AHBGKHC1WCVTXHJ69X)`. Package `internal/adapter/check`. Agent kind `runner`.**

A check is a CI job that Rein runs as an ordinary run. It is one job of one
workflow file, run at one exact commit with [nektos/act](https://nektosact.com),
outside GitHub. Read [`docs/adapters.md`](./adapters.md) first: this file
records only what is true of checks.

The ruling is Elk Scout #1163 (a), 2026-10-18: Rein takes the check run kind,
and there is no separate runner binary. The plan is elk
`docs/milestones/elk-ci.md` §5.2, Phase B. A check is one more adapter behind
the contract, so claim, capability checks, preflight, worktree, progress,
submit and reap all run unchanged.

## Enrolling a runner

```
rein enrol --workspace "Elk Scout" --agent-kind runner --token <connector-token> --queue lab-checks
```

`runner` is an `agent_kind` like `claude`. Elk must accept it, which is
ark:scout#4033's half. The queue declares:

- `agent:runner`, from the kind;
- `tool:act` when `act` is on PATH;
- `tool:docker` when `docker info` answers;
- `os:<this OS>`, and the runtime capabilities in the manifest below.

## What a check packet looks like

`claim_run` hands Rein markdown, so Elk renders the packet into the claim
text. Rein reads a fenced JSON block under a `### Check` heading:

````
### Work order
repo: elk-work/scout
kind: check

### Check

```json
{"repo":"elk-work/scout","sha":"<40 hex>","ref":"refs/heads/main","suite":"migration-numbers",
 "workflow":".github/workflows/quick-checks.yml","job":"checks","runs_on":["ubuntu-latest"],
 "provider":"linux-lab","changed_paths":["supabase/migrations/0550_x.sql"]}
```
````

Since scout#2480, Elk makes one run per (workflow, job, runs_on) at a
commit, so one run can satisfy several suites. Its packet lists them all in
`check.suites`, and `check.suite` stays set to the first:

```json
{"repo":"elk-work/scout","sha":"<40 hex>","ref":"refs/heads/main","suite":"migration-numbers",
 "suites":["migration-numbers","no-schema-fallbacks"],"workflow":".github/workflows/quick-checks.yml",
 "job":"checks","runs_on":["self-hosted","linux","linux-lab"],"provider":"linux-lab",
 "changed_paths":["supabase/migrations/0559_check_runs_per_job.sql"]}
```

Rein runs the job once and writes one record per suite in `suites`; see
[One run, several suites](#one-run-several-suites).

The block may also hold `{"check": {…}}` or the whole packet
(`{"kind":"check","check":{…}}`). Rein also accepts:

- a single `check: {…}` line;
- `check.sha: …` lines;
- `sha: …` lines under `### Check`.

The first shape found wins. Unknown fields are ignored. The packet's
top-level `repo` must be `check.repo`, so the existing `repo:` line picks the
checkout. Rein refuses a check whose `check.repo` differs from that
checkout's GitHub origin.

Every value is validated before it reaches git or act:

| Field | Rule |
|---|---|
| `sha` | a full 40- or 64-hex commit |
| `workflow` | a file directly under `.github/workflows/` |
| `job` | an id or a name, never a flag |
| `suite` | optional; at most 200 bytes, no line break; default `<workflow file name>/<job>` |
| `suites` | optional; an array (or one comma-separated string) of names under `suite`'s rule, at most 64. When present and not empty it is the list and `suite` becomes its first entry; a name listed twice is recorded once. When absent or empty it is `[suite]` |
| `runs_on` | runner labels, at most 16 |
| `event` | optional; one of `workflow_dispatch`, `push`, `pull_request`, `schedule` |

One more rule covers the packet as a whole. Its records must fit the 8 KiB
block that ends the deliverable, even when every field the run fills in
later is at its longest. Rein refuses a packet whose suites could not all fit
there, before anything runs, and the run goes stuck saying to send them as
separate runs. With ordinary names that is about nine suites per run: nine
22-character names fit, and scout's six quick-checks suites fit easily.

A run that reaches a runner queue without a packet goes stuck: "The work
order cannot be run as written". A runner runs checks, not tasks.

The job is found by id, then by id ignoring case, then by name. A workflow
with exactly one job runs that job, and the deliverable says so. Scout's
`quick-checks.yml` names its one job `checks` (named "Quick checks"), so a
packet saying `quick-checks` still runs it.

### What runs-on requires

The rule is elk-ci.md §5.2. `RequiredCapabilities` implements it, and Elk's
packet writer applies the same rule:

| runs-on | requires |
|---|---|
| `ubuntu-*`, `linux`, `linux-lab` | `agent:runner os:linux tool:docker` |
| `macos-*`, `mac-01`, `mac-lab` | `agent:runner os:macos tool:xcode` |
| `windows-*`, `win-*`, `win11` | `agent:runner os:windows desktop` |

## The commit

`Adapter.Prepare` runs where `Preflight` runs, before anything exists on
disk. It reads the packet and hands the commit to the run loop. The worktree
manager checks that the repository holds the commit, and fetches it from
origin by name (`git fetch --no-tags origin <sha>`) if not. It then cuts the
worktree detached at exactly that commit: no branch, no Ark link, no
bootstrap script.

A commit that cannot be fetched goes stuck with `stop_code: repo_access` and
the title "Commit `<sha>` could not be fetched from origin". No directory is
created.

## The two executors

```
act <event> -W <workflow> -j <job> --json --rm -P <label>=<image>        # container
act <event> -W <workflow> -j <job> --json --rm -P <label>=-self-hosted   # host
```

`--rm` is there because act tears a job's environment down only after a
success unless told otherwise. Without it, every failed check left its job
container running on the Docker daemon, or its host-executor directory in
act's cache, for good. See [What a check leaves behind](#what-a-check-leaves-behind).

| | container | host |
|---|---|---|
| Runs the steps | in a container on the Docker daemon the environment names (`DOCKER_HOST`, `DOCKER_CONTEXT`, … from the daemon, or from the queue's secrets) | on this machine itself: what `mac-01` and the Windows boxes need |
| Chosen by default for | `ubuntu-*` and `linux-lab` jobs | `macos-*`, `mac-01` and Windows jobs |
| Image | `catthehacker/ubuntu:act-latest`, or `settings.image` | — |
| Pulls | `--pull=false` when the daemon already has the image | — |
| Git | the source repository's git directory is mounted read-only at its own path, because a worktree's `.git` is a file pointing there | works as is |
| Preflight | `docker info` must answer, or the run goes stuck before the worktree | git and act on PATH |

A workflow's `runs-on` is often an expression. Scout's resolves
`vars.LINUX_RUNNER` to `ubuntu-slim` outside GitHub. So Rein maps every label
the job could resolve to, not only the packet's:

1. the packet's labels;
2. the queue's `settings.labels`;
3. a default set (`ubuntu-latest`, `ubuntu-slim`, `ubuntu-24.04`,
   `self-hosted`, `macos-*` and so on).

An unmapped label sends act to its own default Docker image. That fails
"Set up job", which Rein records as an error, not a failure.

`GITHUB_TOKEN` reaches the job only when the queue's secrets map holds one; a
token the daemon inherits does not count. It is passed as `-s GITHUB_TOKEN`,
by name, and act reads the value from its environment, so it is never on a
command line.

On the host executor, act hands its own environment to every step. So:

- `STRIDE_INGEST_URL` and `STRIDE_TOKEN` never reach act at all; only pulse
  gets them.
- Every secret value is taken out of each line before it reaches the log, a
  progress event or the deliverable. That covers the queue's scoped secrets
  and any Stride or GitHub variable the queue inherits.

## Queue settings

```toml
[[queues]]
name = "lab-checks"
agent_kind = "runner"
repos = ["elk-work/scout"]

[queues.settings]
executor = "container"                   # container | host | auto (default: by the job's labels)
image = "catthehacker/ubuntu:act-latest" # the container image for every mapped label
labels = "ubuntu-slim, linux-lab"        # extra runs-on labels to map
timeout = "30m"                          # default 1h; the run's own cap wins when it is shorter
event = "workflow_dispatch"              # what every check on the queue runs as
```

Rein refuses an unknown key or value rather than guessing: a typo in
`executor` that quietly became the default would run a desktop job in a
container.

## Progress, results and the record

Rein renders every line of act's `--json` log, with terminal colour
stripped, and sends each one as a progress event. The run loop throttles
these as it throttles an agent's. If act prints nothing for a minute, the
session says it is still running, so a silent compile is not mistaken for a
stall. The total is bounded by the timeout.

| act said | record `result` | session | Elk run |
|---|---|---|---|
| 🏁 Job succeeded | `pass` | succeeded | `ready`, deliverable says pass |
| a workflow step failed, 🏁 Job failed | `fail` | **succeeded** | `ready`, deliverable says fail |
| "Set up job" failed, no 🏁 line, or act exited early | `error` | failed | `stuck`, "Did not finish" |
| past the timeout | `error` | timed out | `stuck` |
| interrupted (Elk cancelled the run) | `cancelled` (written, never sent) | interrupted | discarded |

A failed job is a finished check: it found out. Only a harness fault fails
the session.

The suite record follows [pulse/docs/suite-records.md] schema_version 1. It
is written to `<Rein home>/checks/<run id>/suite-record.json`, outside the
worktree, beside the full log (`act.log`). A run that satisfies several
suites writes one record per suite: the first to `suite-record.json`, the
rest to `suite-record-2.json`, `suite-record-3.json` and so on, in the
order of `check.suites`. The fields are:

| Field | Value |
|---|---|
| `pr` | 0 |
| `runner` | the queue name |
| `provider` | the packet's, default `rein` |
| `branch` | only when `git ls-remote origin refs/heads/<ref's branch>` is exactly the commit tested |
| `counts` | step counts from act's log (`steps`, `steps_passed`, `steps_failed`, `steps_skipped`), named for steps because a step is not a test; `{}` when no step ran |
| `failing_test_ids` | `[]` |
| `changed_paths` | the packet's, capped as the schema caps it: over 1,024 bytes of JSON, the longest prefix that fits, ending with `...[truncated]` |
| `retention_class` | `ci-history` |

Rein applies the `changed_paths` cap itself, exactly as pulse applies it
when it emits. So the file, the copy in the deliverable and what pulse sends
are the same list, and a capped list passes through pulse unchanged.

When `pulse` is on PATH and the queue's environment holds
`STRIDE_INGEST_URL` and `STRIDE_TOKEN`, Rein runs `pulse suite-record --file
<record>`, once per record. That is the scoped secrets map, or `inherit_env`
for an unscoped queue. Otherwise the record stays on the machine and the
deliverable says why. A failed send is reported in the deliverable, never a
stuck run. Each send has its own time limit, so a slow first send cannot cut
a later one short. Either way the deliverable carries every record, so Elk
can send on the ones Rein did not.

The deliverable leads with one plain sentence: the suite passed, failed, or
could not be run. It then gives:

1. the check: commit, job, executor, and the suite or suites;
2. each record's path, what happened to it, and the log's path;
3. the last 40 lines of the log, each cut at 500 characters;
4. the records themselves, last.

## The record in the deliverable

Every check deliverable ends with the suite record, so that Elk can forward
it to Signal from its own side. A runner with no `pulse` binary and no
Stride credentials still produces history that way. The scout side reads
this section, so its shape is a contract:

- It starts with a line that is exactly `### Suite record`. It is the last
  section, and nothing follows it.
- Then comes one fenced block, opened by a line that is exactly ```` ```json ````
  and closed by a line that is exactly ```` ``` ````.
- For a packet with one suite, the block holds one JSON object, indented and
  over several lines. Its bytes are the bytes Rein wrote to
  `suite-record.json`. If that write failed, they are the bytes Rein would
  have written.
- **An array appears only when a packet carried more than one distinct
  suite.** Then the block holds a JSON array of record objects, one per
  suite, in the order of `check.suites`, indented and over several lines.
  The records are identical except for `suite`. Each element is the JSON of
  its record file, re-indented as an array element. It is equal as JSON,
  not byte for byte.
- One field may be added at the end of a record: `"emitted_by": "rein"`. It
  is there only when Rein itself ran `pulse suite-record` for that record and
  the command succeeded. In an array each record carries its own: one send
  can fail while the others succeed.
- If pulse is missing, the queue lacks the Stride variables, the send fails,
  or the run was stopped, the field is absent.
- **Elk forwards a record only when its `emitted_by` is absent**, so a record
  is never sent twice.
- A backtick inside a string is written as `\u0060`. That is the same string
  to any JSON reader, and no line of the record can close the fence.
- The fenced block, fences included, stays under 8 KiB.
  - The `changed_paths` cap already keeps a real record far below that.
  - If the block would still reach 8 KiB, it sets `changed_paths` to `[]`
    and adds a `"note"` field saying how many entries were left out. In an
    array it does so on every record. The files keep them.
  - So `[]` with a `note` means the list was left out, not that the diff was
    empty. Every other field is bounded by the packet's validation, and a
    packet whose suites could not all fit is refused before it runs. So the
    rest always fits.

The section is there on every deliverable a check submits:

- `ready` for a pass or a fail;
- `stuck` ("Did not finish") for an error or a timeout.

A cancelled run submits nothing, so it has no section.

The run loop's redaction of deliverables still applies to the whole text, as
the last line of defence. The record holds no secret to begin with. It is
built from the packet and act's step counts, and never from the environment.

````
### Suite record

```json
{
  "schema_version": 1,
  "repo": "elk-work/scout",
  "suite": "migration-numbers",
  "sha": "10acf20cc78f081edf12dcee619dc5b8d609b636",
  "pr": 0,
  "branch": "main",
  "provider": "linux-lab",
  "runner": "lab-checks",
  "result": "pass",
  "duration_ms": 41873,
  "counts": {
    "steps": 3,
    "steps_failed": 0,
    "steps_passed": 3
  },
  "failing_test_ids": [],
  "changed_paths": [
    "supabase/migrations/0550_x.sql"
  ],
  "retention_class": "ci-history",
  "started": "2026-10-25T05:34:28.593Z",
  "ended": "2026-10-25T05:35:10.466Z",
  "emitted_by": "rein"
}
```
````

## What a check leaves behind

**ark:rein#91 (01M5RP54QM0XFQZBKVTDPMRB1V).** The run loop reaps the
worktree as it does for every run. At the same moment it asks the check
adapter to tidy up the rest:

| What | Where | When it goes |
|---|---|---|
| The records and act.log | `<Rein home>/checks/<run id>/` | past the newest `log_keep_runs` check runs (50 by default), like the run logs |
| act's scratch directory for a host-executor job | `<act cache>/<16 hex>/` holding `act/`, `hostexecutor/` and `tmp/` | once nothing has touched it for a day |
| act's action clones | `<act cache>/<owner>-<repo>@<ref>/` | never |
| act's tool cache | `<act cache>/tool_cache/` | never |

`<act cache>` is where act itself puts it, read from the environment Rein
hands act: `$XDG_CACHE_HOME/act`, else `~/.cache/act`.

- **Records follow `log_keep_runs`**, so a run's record and its event log age
  out together. A negative number keeps everything. The copy in Elk is the
  one people read; the file is for looking into a recent run.
- **act names a host-executor directory with eight random bytes**, so nothing
  ties it to a run but its age. act removes it itself when the job ends, and
  `--rm` makes that true of a failed job too. What is still there was left by
  an act that was killed. Only a directory named sixteen hex digits with a
  `hostexecutor/` inside, not a link, is ever removed. Its age is the newest
  modification time of the directory and its children.
- **Action clones and the tool cache stay.** They are bounded by the actions
  and tool versions the workflows use, and act refreshes them in place. They
  are also expensive to lose: the Linux lab's first check spent 88 of its
  123 seconds downloading node. On a desktop the same directory is a
  person's own act cache too. On the container executor the tool cache is a
  Docker volume, not a directory. act's `actions/cache` server
  (`~/.cache/actcache`) expires its own entries.

Nothing a live run could be using is ever removed. Which runs are live is
what the run logs say, exactly as for pruning the logs themselves:

- a live run's record counts against `log_keep_runs` and is kept;
- no act directory counts as a day old while a run that started before it
  was last touched is still going;
- with no run logs (an unwritable Rein home) nothing is removed at all.

`--keep-worktrees` does not change any of this. It keeps the run's
worktree; act has already removed its own directory by then.

## Manifest

| Capability | Declared | Why |
|---|---|---|
| `git`, `worktree`, `shell` | yes | the job reads its worktree and runs shell steps in it |
| `structured_events` | yes | act's `--json` log, read line by line |
| `file_edit`, `mcp`, `resume`, `approvals`, `steer` | no | a check edits nothing, takes no MCP servers, cannot be resumed, asks nobody and takes no input |

## What was measured, and when

Measured on 2026-10-24 against act 0.2.89 on darwin/arm64:

- The recorded logs in `testdata/` come from act's host executor on a
  three-step workflow: pass, a failing step, and an unmapped label.
- `REIN_REAL_ACT=1 go test ./internal/adapter/check -run RealAct -v` runs the
  real act. `REIN_REAL_ACT_REPO` pointed at a checkout of elk-work/scout also
  runs its quick-checks job through the adapter.
- The container executor ran against a real Docker daemon on the Linux lab
  on 2026-10-25, and the round trip through Elk with it; both are quoted on
  ark:rein#91 (01M5RP54QM0XFQZBKVTDPMRB1V).
- The act spike that measured which workflow keywords act ignores is on
  ark:pulse#78 (01M56D1BR2B7ZGBPR8JNEJYVQG).

### One run, several suites

A packet whose `check.suites` names two suites ends with this. Here the
first record's send succeeded and the second's failed, so Elk forwards only
the second:

````
### Suite record

```json
[
  {
    "schema_version": 1,
    "repo": "elk-work/scout",
    "suite": "migration-numbers",
    "sha": "10acf20cc78f081edf12dcee619dc5b8d609b636",
    "pr": 0,
    "branch": "main",
    "provider": "linux-lab",
    "runner": "lab-checks",
    "result": "pass",
    "duration_ms": 41873,
    "counts": {
      "steps": 3,
      "steps_failed": 0,
      "steps_passed": 3
    },
    "failing_test_ids": [],
    "changed_paths": [
      "supabase/migrations/0559_check_runs_per_job.sql"
    ],
    "retention_class": "ci-history",
    "started": "2026-10-25T05:34:28.593Z",
    "ended": "2026-10-25T05:35:10.466Z",
    "emitted_by": "rein"
  },
  {
    "schema_version": 1,
    "repo": "elk-work/scout",
    "suite": "no-schema-fallbacks",
    "sha": "10acf20cc78f081edf12dcee619dc5b8d609b636",
    "pr": 0,
    "branch": "main",
    "provider": "linux-lab",
    "runner": "lab-checks",
    "result": "pass",
    "duration_ms": 41873,
    "counts": {
      "steps": 3,
      "steps_failed": 0,
      "steps_passed": 3
    },
    "failing_test_ids": [],
    "changed_paths": [
      "supabase/migrations/0559_check_runs_per_job.sql"
    ],
    "retention_class": "ci-history",
    "started": "2026-10-25T05:34:28.593Z",
    "ended": "2026-10-25T05:35:10.466Z"
  }
]
```
````

Every record carries the job's one result. Under `workflow_dispatch`,
scout's quick-checks runs every step, so a pass covers each suite it lists.
Rein does not attribute steps to suites.

[pulse/docs/suite-records.md]: https://github.com/elk-work/pulse/blob/main/docs/suite-records.md
