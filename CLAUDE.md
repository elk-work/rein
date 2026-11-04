# Rein

Rein is Elk's runner daemon. The README says what it is and how to run it; the
house rules every session follows live in `elk-work/elk`'s `CLAUDE.md`, and this
file only points there.

**Agents do the work.** Casey's organization-wide rule (2026-11-03) is stated
once, in `elk/CLAUDE.md` → "Agents do the work". A review, a ruling an agent can
reason out, an investigation or a fix is an agent's. Work goes to a person only
where a written rule names one (a gate, an app release, a ruling that names a
person) or no agent can do it (a password, an Apple ID or Keychain approval,
physical access to a machine, a click no tool can make). When a review is
merited, run an independent review agent, not a person.

**Plain names.** Anything a person reads (Elk item titles, PR titles, messages)
uses real product names and says what a thing does. No invented shorthand, lane
numbers or nicknames; when unsure of a term, say so.

The runner's own prompt (`internal/runner/prompt.md`) keeps its rule that a run
stops when verifying needs a screen, a device or a person at the machine: Rein
runs on a person's own computer and must never watch their desktop, which is a
written rule and a real limit.
