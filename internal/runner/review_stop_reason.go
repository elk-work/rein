package runner

import "regexp"

// reviewStopReason says why Rein stopped after Elk's review asked for
// changes round after round, from the last thing the review asked for. Before
// it, every such stop said only "A person should look at this", which named no
// cause and gave nobody anything to act on (Elk Scout #2018, #1948, #1939,
// #1531, #1573). The sentence never opens a line with "Needs you:", which
// would put the task on a person's To do; and the report's title stays
// "Elk's review still wants changes", which Elk reads (scout migration 0773).
//
// code is the stop code Elk records: a rule conflict is agent_rules, so Elk
// keeps the task with agents rather than on a person's tab; the rest send none.
func reviewStopReason(verdict string) (sentence, code string) {
	switch {
	case reRuleConflict.MatchString(verdict):
		return "Why it stopped: a rule conflict. Elk's review asked for a step this runner's rules forbid " +
			"(applying a database migration, deploying server code, or touching production), so no further " +
			"round could satisfy it. Elk's going-live step does that step after merge; the rest of the work " +
			"may be complete, as the agent's report below says.", "agent_rules"
	case reCIRunning.MatchString(verdict):
		return "Why it stopped: CI was still running. Elk's review wanted results that had not arrived when " +
			"the agent handed in. Once the checks finish, sending the run again lets the review see them.", ""
	case reMissingTool.MatchString(verdict):
		return "Why it stopped: a missing tool. Elk's review asked for something this agent has no tool or " +
			"access for, so the task needs an agent that has it.", ""
	}
	return "Why it stopped: Elk's review kept asking for changes the agent did not make. Either the request " +
		"is not something the agent can act on, or the work needs a different approach.", ""
}

var (
	reRuleConflict = regexp.MustCompile(`(?is)\b(apply|applying|applied|dispatch|run)\b.{0,60}\bmigrations?\b|` +
		`\bmigration-apply\b|\bdeploy-functions\b|\bdeploy(ing)?\b.{0,40}\b(edge )?functions?\b|\bproduction\b`)
	reCIRunning = regexp.MustCompile(`(?is)\b(ci|checks?|workflows?|builds?|tests?|lab (proof|run))\b.{0,50}` +
		`\b(still running|running|pending|in progress|queued|not (yet )?(green|finished|complete|done))\b|` +
		`\bwait(ing)? (for|until) (the )?(ci|checks?|build|tests?|workflow)`)
	reMissingTool = regexp.MustCompile(`(?is)\b(no|missing|without|lacks?|cannot reach|can't reach|unavailable|not available)\b` +
		`.{0,40}\b(tool|tools|access|credentials?|permission|connector|token)\b|` +
		`\b(tool|connector)\b.{0,30}\b(missing|not available|unavailable|not exposed)\b`)
)
