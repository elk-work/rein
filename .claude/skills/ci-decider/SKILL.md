---
name: ci-decider
description: Decide what CI a change needs before its PR lands — which suites the diff implicates, what to run locally, which declared legs to label (ci:elkkit, ci:apple-build, ci:apple-ui), which to skip and why — then read the ci-plan status. Use when finishing work and preparing, opening or landing a PR in any Elk repo (elk, scout, signal, pulse, rein, website), when asked what tests or CI a change needs, whether a PR needs a ci:* label, or what a ci-plan summary such as "enforce would fail" means.
---

# ci-decider

You are finishing a change. Before its PR lands, decide what runs, where, and
on what evidence, and leave that where `ci-plan` and the next reader can see
it. Design: elk `docs/ci-plan.md` (Elk Scout #665 → a). Merge rule: elk
`docs/pr-flow.md`, condition 1.

**The rule that outranks the rest: never describe coverage that did not run.**
A skipped, queued or absent job is not coverage. A local run counts only where
the manifest accepts it. A `skip` line is a decision you own, not a pass. If
the honest answer is "nothing ran this", write that.

## The loop

1. Find the repo's manifest, `ci/suites.yml`.
2. See which suites the diff implicates.
3. Run what you can locally with `scripts/ci-local.sh <suite>`.
4. Label the declared legs you need (`ci:<suite>`).
5. Write `skip <suite>: <reason>` for skippable suites you skip.
6. Settle Pulse coverage (pulse-decider).
7. Wait for `ci-plan` and read its summary, not its colour.

## 1. The manifest

Look for `ci/suites.yml` at the root of the repository you are changing. In
the superrepo that is the submodule, e.g. `scout/ci/suites.yml`.

- **Found** (scout): follow steps 2–7. Each suite has
  `paths`, a `tier` (`floor` starts by itself on every matching PR;
  `declared` is yours to start), `local` (the command `ci-local.sh` runs, or
  `no`), `skippable` and, for declared legs, a `label`.
- **Not found**: this repo has no manifest, and today's rules in elk
  `docs/ci-runners.md` apply
  (<https://github.com/elk-work/elk/blob/main/docs/ci-runners.md> from a bare
  clone). "CI is green" means every check that ran on the head passed. Run the
  tests covering what you changed before pushing, and put one line in the PR
  body naming what ran and what did not (`Tested: go test ./... (412 passed);
  Windows build not run locally`). Then do step 6 and stop.

## 2. What the diff implicates

From the repo root, after `git fetch origin main`:

```sh
node ci/run.mjs plan                          # committed changes vs origin/main; posts nothing
node ci/run.mjs explain --pr <n>              # the table ci-plan computes for an open PR
node ci/run.mjs explain --pr <n> --enforce    # what enforce mode would say
```

Each implicated suite comes back with the ways it can be satisfied. For each:

| Suite | Satisfied by | Your move |
|---|---|---|
| `floor` | its own check, green on the head | Nothing; it starts itself. Local runs are for iterating and do not count. |
| `declared`, `skippable: false` | its check green on the head, or `local/<suite>` where `local` is not `no` | Run it locally (step 3) or label it (step 4). Never a skip line. |
| `declared`, `skippable: true` | any of the above, or a skip line | Run it if your change could move what it tests; otherwise skip it with a reason (step 5). |

A red check on the head is never satisfied by local evidence or a skip line.
Fix it.

Where it runs, in order: your machine, then the lab (the label routes there,
per the suite's `run:` list), hosted only when neither can. The money is
macOS, so start a declared leg once, when the branch is ready, not on every
push.

## 3. Run it locally

Commit and push first. Evidence is for one exact commit.

```sh
scripts/ci-local.sh <suite>             # runs the manifest's `local` command; on success posts local/<suite> on HEAD
scripts/ci-local.sh <suite> --dry-run   # runs it and prints the status instead of posting
```

It refuses a `local: no` suite (`apple-build`, because local Xcode is not the
release toolchain, and every floor suite), a dirty tree, and a head that is
not on GitHub. It posts nothing when the command fails. The status belongs to
one SHA: **push again and it no longer counts**, so run it on the head you
mean to merge. Only the script posts `local/*`. Never hand-post one.

Cannot run it here (a cloudcode sandbox, no Xcode)? Label it instead.

## 4. Label the declared legs you need

```sh
gh pr edit <n> --add-label ci:<suite>   # ci:elkkit, ci:apple-build, ci:apple-ui
```

The label starts the leg on the first available provider. Every later push to
a labelled PR runs it again, so add the label when you have finished pushing.
Scout's `CI_PLAN_MODE` is `enforce`, so every declared leg needs its label:
nothing macOS starts by path. A queue on the lab Mac is waited out, never
routed around to hosted macOS.

## 5. Skip honestly

Only a `skippable: true` suite can be skipped. Write one line per suite in the
PR body, outside any code fence:

```text
skip apple-ui: copy-only change to the Settings footer; no thread, list or scroll code touched
```

The reason says why *this diff* cannot break what the suite tests. "No time",
"flaky" and "CI is expensive" are not reasons. Once enforce mode is on, the
nightly backstop (`ci-backstop.yml`) runs every declared leg on `main`. If it
turns red on something you skipped, the skip was wrong: say so on the task.

## 6. Pulse coverage

Pulse parity is not in `ci/suites.yml`, because `parity.yml` always measures
scout `main` and cannot test a PR branch. Whether a change needs Pulse is
**pulse-decider**'s question. That covers scout UI a corpus scenario names, a
stamped accessibility id, an IA move, and a Windows or macOS app change.
Follow pulse-decider for suite, surfaces, evidence class and provider, and put
its `**Pulse coverage:**` block in the PR body. In a scout PR the block is
always there, even as `none — <why>`. Elsewhere, add it only when Pulse
applies. In the superrepo pulse-decider loads scoped to `pulse/`. From any
other clone, read it:

```sh
gh api repos/elk-work/pulse/contents/.claude/skills/pulse-decider/SKILL.md -H "Accept: application/vnd.github.raw"
```

## 7. Wait for `ci-plan`, then read it

`ci-plan` is one commit status on the PR head. It re-evaluates on every push,
label, body edit, suite completion and `local/*` status.

```sh
gh pr checks <n>
gh api repos/elk-work/<repo>/commits/<head-sha>/status --jq '.statuses[] | select(.context=="ci-plan") | .state + ": " + .description'
```

- **Enforce** (scout since 2026-10-01; `ci-plan` is its required check):
  `failure` names each unmet suite and what would satisfy it. `pending` means
  a leg is still running. Merge on `success` only; an armed auto-merge waits
  for it. The status's Details link opens the job summary with the full table.
- **Advisory** (`CI_PLAN_MODE` unset — a repo trialling a manifest): the
  status is **always success**, so read the description. `advisory · plan
  met: …` or `advisory · no suites implicated` means done; `advisory · enforce
  would fail: …` means work is left — do what it says.

No `ci-plan` status on the head at all means the plan never ran. Push, or
dispatch it (`gh workflow run ci-plan.yml --repo elk-work/<repo> -f pr=<n> -f
post=true`). The sweep holds a manifest repo's PR that has no `ci-plan`.

---

Canonical copy: `elk-work/elk` `.claude/skills/ci-decider/SKILL.md`. The
copies in scout, signal, pulse, rein and website are byte-identical and are
diffed by elk's `ci-decider-sync.yml`. Change the skill in elk, then run
`bash scripts/ci-decider-sync.sh --open-prs` there. Never edit a copy.
