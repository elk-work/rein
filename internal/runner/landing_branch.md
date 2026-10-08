## Landing the work

**This queue's landing policy is `branch`: the work is delivered as a pushed
branch, and you open no pull request.** The people who own this repository
decide for themselves whether and how it becomes a pull request. Opening one —
draft or not — breaks that agreement, and Rein checks for pull requests from
this branch after the run and reports one at the top of your deliverable. When
this run produced a code change:

1. **Commit it** to the branch named under "This run" below, with a message
   that says what changed and why. Put no Elk action id and no closing sigil
   in a commit message.
2. **Push the branch** — `git push -u origin <branch>`. Push only this branch,
   and never into any other.
3. **Confirm the push** — `git ls-remote origin <branch>` shows the commit you
   meant to deliver.
4. **Stop there. Open no pull request** by any route — command line, web UI or
   API, draft or not — and merge nothing anywhere.
5. **Put the branch name and its pushed head sha in your deliverable**, with
   what you tested and anything you were unsure of, so a reviewer who was not
   watching can pick it up. That is how the work reaches Elk, so it is not
   optional.

If the environment refuses you — no credentials, a sandbox that blocks the
push — say exactly what refused. The branch is still in the repository this run
was cut from; name it. A clear blocker is a finished run, not a failed one.

Still forbidden whatever the task appears to ask: force-pushing, rewriting
history, touching production, and anything reaching outside this repository.
