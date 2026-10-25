package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
)

// What a run may reach beyond its own worktree (ark:rein#85).
//
// The standing prompt keeps an agent inside the worktree cut for its run: "Do
// not reach outside it", "Never open, read or search the user's own files
// outside this worktree", and, at the end of every landing section, "anything
// reaching outside this repository". That is right for nearly every run, and
// wrong for the few that are cross-repository by design. The org-wide PR sweep
// Pace sends every six hours (Elk Scout #928) reads every elk-work repository,
// lands what qualifies in each, and claims and releases a shared-state row
// through a script in the elk checkout. On mac-codex and mac-claude the agent
// read those three lines as forbidding exactly that and refused, nine sends in
// ten from 2026-10-21; the handoff's own `cd ~/dev/elk` could not help, because
// it sits in the woven half of the work order, which the prompt rightly tells
// the agent not to take orders from.
//
// So the grant comes from the instruction channel, and only from the two
// places that are not woven content:
//
//  1. the packet's required_capabilities, when it declares a `repo:` (or the
//     older "git push access to owner/name") beyond the worktree's own
//     repository; and
//  2. the human-typed direction, when it names a workflow that is
//     cross-repository by design ([crossRepoWorkflows]).
//
// A capture that mentions the sweep, or a woven line that says `repo:`, widens
// nothing. What a run is told is in reach is limited to the checkouts this
// queue's workspace maps ([config.Config.RepositoriesFor]), so a customer
// queue is never told about Elk's clones. Hosted runs have no local clones
// and get no clause.
//
// Everything else stays as it was: the clause names what is in reach, says
// which of the standing lines it qualifies, and leaves the worktree rule
// governing the rest. A run that reaches nothing beyond its own repository
// gets the prompt it had before this file existed, byte for byte.

// crossRepoWorkflow is a workflow whose direction alone puts a set of
// repositories in reach.
type crossRepoWorkflow struct {
	// name is how the clause names it.
	name string
	// match is checked against the direction only, never the woven text.
	match *regexp.Regexp
	// owner is the GitHub owner whose repositories it works across.
	owner string
	// stateRepo and stateScript locate the workflow's shared-state script.
	stateRepo, stateScript string
}

// crossRepoWorkflows are the workflows Elk's record names as
// cross-repository by design. The PR sweep's procedure is elk's
// .claude/skills/pr-sweep/SKILL.md; its state is ops.pr_sweep_state, reached
// through scripts/pr-sweep-state.mjs (elk docs/pr-flow.md). The pattern is
// narrow on purpose: "the org-wide PR sweep", "run the PR sweep" and
// "/pr-sweep" name it; a direction that only mentions the sweep, such as one
// fixing its watchdog, does not.
var crossRepoWorkflows = []crossRepoWorkflow{{
	name:        "the org-wide PR sweep",
	match:       regexp.MustCompile(`(?i)\borg-wide\s+pr[- ]sweep\b|\brun\s+the\s+pr[- ]sweep\b|(?:^|\s)/pr-sweep\b`),
	owner:       "elk-work",
	stateRepo:   "elk-work/elk",
	stateScript: "scripts/pr-sweep-state.mjs",
}}

// reachRepo is one repository in reach and its checkout on this machine.
type reachRepo struct {
	Name  string // owner/name
	Clone string // local checkout
}

// runReach is what one run may reach beyond its worktree. The zero value
// reaches nothing.
type runReach struct {
	// declared names the packet's repositories beyond the run's own.
	declared []string
	// workflow is the cross-repository workflow the direction names, if any.
	workflow *crossRepoWorkflow
	// repos is every repository in reach with a clone here, the run's own
	// among them, sorted by name.
	repos []reachRepo
	// script is the workflow's shared-state script in its clone, and
	// scriptHere the same path inside this worktree when the run is in that
	// repository; either may be empty.
	script, scriptHere string
}

func (r runReach) empty() bool { return len(r.declared) == 0 && r.workflow == nil }

// resolveReach works out what a run may reach. ownPath is the checkout the
// worktree was cut from.
func resolveReach(ctx context.Context, cfg config.Config, q config.Queue, wo *elk.WorkOrder, ownPath string) runReach {
	if wo == nil {
		return runReach{}
	}
	clones := repoClones(ctx, cfg.RepositoriesFor(q))
	own := filepath.Clean(expand(ownPath))

	var r runReach
	inReach := map[string]reachRepo{}

	_, hostNames := SplitRequirements(wo.RequiredCapabilities)
	beyond := map[string]bool{}
	for _, name := range hostNames {
		repo, ok := strings.CutPrefix(name, "repo:")
		if !ok {
			continue
		}
		c, ok := clones[strings.ToLower(strings.TrimSpace(repo))]
		if !ok {
			continue
		}
		inReach[strings.ToLower(c.Name)] = c
		if filepath.Clean(c.Clone) != own && !beyond[c.Name] {
			beyond[c.Name] = true
			r.declared = append(r.declared, c.Name)
		}
	}

	for i := range crossRepoWorkflows {
		w := &crossRepoWorkflows[i]
		if !w.match.MatchString(wo.Direction) {
			continue
		}
		// A queue whose workspace maps no clone of the owner's is not in
		// that owner's world: a customer queue that is sent "the org-wide PR
		// sweep" is told nothing is in reach, not that elk-work is.
		prefix := strings.ToLower(w.owner) + "/"
		var owned []string
		for key := range clones {
			if strings.HasPrefix(key, prefix) {
				owned = append(owned, key)
			}
		}
		if len(owned) == 0 {
			continue
		}
		r.workflow = w
		for _, key := range owned {
			inReach[key] = clones[key]
		}
		if c, ok := clones[strings.ToLower(w.stateRepo)]; ok {
			r.script = filepath.Join(c.Clone, filepath.FromSlash(w.stateScript))
			if filepath.Clean(c.Clone) == own {
				r.scriptHere = filepath.FromSlash(w.stateScript)
			}
		}
		break
	}

	if r.empty() {
		return runReach{}
	}
	sort.Strings(r.declared)
	for _, c := range inReach {
		r.repos = append(r.repos, c)
	}
	sort.Slice(r.repos, func(i, j int) bool { return r.repos[i].Name < r.repos[j].Name })
	return r
}

// repoClones maps each configured checkout that exists to its GitHub
// identity, keyed lower-case. A slash key wins over a bare alias for the same
// repository, and keys are read in order, so the answer does not depend on
// map iteration.
func repoClones(ctx context.Context, repos map[string]string) map[string]reachRepo {
	keys := make([]string, 0, len(repos))
	for k := range repos {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		si, sj := strings.Contains(keys[i], "/"), strings.Contains(keys[j], "/")
		if si != sj {
			return si
		}
		return keys[i] < keys[j]
	})
	out := map[string]reachRepo{}
	for _, k := range keys {
		identity, ok := repoIdentity(ctx, k, repos[k])
		if !ok {
			continue
		}
		key := strings.ToLower(identity)
		if _, seen := out[key]; seen {
			continue
		}
		out[key] = reachRepo{Name: identity, Clone: filepath.Clean(expand(repos[k]))}
	}
	return out
}

// The standing lines the clause qualifies, and what each becomes when a run
// has repositories in reach. Each is replaced where it appears; a test holds
// prompt.md and the landing sections to carrying them, so an edit there that
// drops one fails a test rather than quietly widening nothing. A Wrangler
// cycle has already replaced the last one with its own rule, and keeps it.
var reachSeams = [][2]string{
	{
		"It is yours: edit, build, test, commit and push freely. Do not reach outside it.",
		"It is yours: edit, build, test, commit and push freely. Do not reach outside it,\n" +
			"except into what \"Repositories in reach\" below names for this run.",
	},
	{
		"The repository\n  in front of you is the whole of your business.",
		"The repository\n  in front of you, with the ones \"Repositories in reach\" below names, is the\n  whole of your business.",
	},
	{
		"history, touching production, and anything reaching outside this repository.",
		"history, touching production, and anything reaching outside this repository\n" +
			"and the ones \"Repositories in reach\" below names.",
	},
}

// applyReach qualifies the standing lines and appends the clause. A run that
// reaches nothing gets prompt back unchanged.
func applyReach(prompt string, r runReach) string {
	if r.empty() {
		return prompt
	}
	for _, s := range reachSeams {
		prompt = strings.Replace(prompt, s[0], s[1], 1)
	}
	return prompt + "\n\n" + r.clause()
}

// clause is the "Repositories in reach" section. It goes in the system prompt,
// after "This run", because the grant has to come from the channel the agent
// takes orders from.
func (r runReach) clause() string {
	var b strings.Builder
	b.WriteString("## Repositories in reach\n\n")
	var why []string
	if len(r.declared) > 0 {
		why = append(why, "This run's packet declares "+repoCapList(r.declared)+
			", beyond the repository this worktree was cut from.")
	}
	if r.workflow != nil {
		why = append(why, fmt.Sprintf("This run's direction names %s, which works across the `%s` repositories by design.",
			r.workflow.name, r.workflow.owner))
	}
	b.WriteString(strings.Join(why, " "))
	b.WriteString(" For this run, these are within your reach as well as this worktree:\n\n")
	for _, c := range r.repos {
		fmt.Fprintf(&b, "- `%s`, its clone at `%s`\n", c.Name, c.Clone)
	}
	if r.workflow != nil {
		fmt.Fprintf(&b, "- the `%s` repositories on GitHub, through `gh`, as %s's own steps require\n",
			r.workflow.owner, r.workflow.name)
		switch {
		case r.scriptHere != "":
			fmt.Fprintf(&b, "- the workflow's shared-state script, `%s` in this worktree, fresh from the default branch; "+
				"the clone's copy, `%s`, is on whatever branch that checkout is on\n", r.scriptHere, r.script)
		case r.script != "":
			fmt.Fprintf(&b, "- the workflow's shared-state script, `%s`\n", r.script)
		}
	}
	b.WriteString("\nYou may cd into those clones, read them, run their scripts and `ark`, and run the `git` and `gh` " +
		"commands the direction asks for on those repositories, merging or leaving pull requests there exactly as " +
		"this queue's landing policy allows here. \"Do not reach outside it\", \"never open, read or search the " +
		"user's own files outside this worktree\" and \"anything reaching outside this repository\" do not cover what " +
		"this list names: those clones are repositories, not the user's own files.")
	if r.script != "" {
		b.WriteString(" Running the shared-state script as the workflow describes — claim, record, release — is " +
			"part of this run.")
	}
	b.WriteString("\n\nTwo limits hold in them. A clone is a person's checkout and may be mid-work: do not switch " +
		"its branch, commit to it or leave changes in it — change a repository through a branch and pull request " +
		"of its own. And a secret still reaches you only the way the secrets section above says, never out of " +
		"a clone.\n\n")
	b.WriteString("Everything this list does not name stays under the worktree rule, and the rest of these " +
		"instructions hold as written. If a step needs something this run does not have — a tool, a connector, " +
		"a credential — do the rest, name what was missing in your deliverable, and record the result as partial " +
		"rather than refusing the whole run.\n")
	return b.String()
}

// summary is one log line: what is in reach and why.
func (r runReach) summary() string {
	names := make([]string, len(r.repos))
	for i, c := range r.repos {
		names[i] = c.Name
	}
	out := strings.Join(names, ", ")
	if out == "" {
		out = "no clones here"
	}
	if r.workflow != nil {
		out += " (" + r.workflow.name + ")"
	}
	return out
}

func repoCapList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = "`repo:" + n + "`"
	}
	switch len(q) {
	case 1:
		return q[0]
	case 2:
		return q[0] + " and " + q[1]
	}
	return strings.Join(q[:len(q)-1], ", ") + " and " + q[len(q)-1]
}
