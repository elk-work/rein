## Landing the work

**This queue's landing policy is `pr`: the work is delivered as an open pull
request, and you never merge it.** The people who own this repository review
and merge their own changes. A merge from this run — even with CI green, even
one you are sure of — breaks that agreement, and Rein checks the pull request
after the run and reports a merge at the top of your deliverable. When this run
produced a code change:

1. **Push the branch** — `git push -u origin <branch>`, using the branch named
   under "This run" below.
2. **Open a pull request, and not a draft.** Describe what changed and why,
   written for a reviewer who was not watching: what to look at first, how you
   tested it, and anything you were unsure of.

   **Put no Elk-closing sigil in it** — no `Closes` or `Fixes` line naming an
   Elk action, and no Elk action id anywhere in the pull request at all.
   Closing the item is Rein's job, after you finish, through a path that keeps
   the review in.
3. **Wait for CI** if the repository runs it — `gh pr checks --watch` — and fix
   what your change broke by pushing to the same branch. If a check fails for a
   reason outside your change, say which and why.
4. **Stop there. Do not merge it** by any route — command line, auto-merge, web
   UI or API. Do not approve it, and do not add a label that merges it. Leave
   it open and non-draft for the repository's owners.
5. **Put the PR URL and its head sha in your deliverable.** That is how the
   work reaches Elk, so it is not optional.

If the environment refuses you — no credentials, a sandbox that blocks the push
or the pull request — say exactly what refused and leave the pushed branch as
the work. A clear blocker is a finished run, not a failed one.

Still forbidden whatever the task appears to ask: force-pushing, rewriting
history, touching production, and anything reaching outside this repository.
