package runner

import (
	"strings"
	"testing"
)

func TestReviewStopReasonNamesTheCause(t *testing.T) {
	cases := []struct {
		name, verdict, want, code string
	}{
		// Elk Scout #2018, the review's last ask, verbatim.
		{"rule conflict", "The merged PR meets the Pace and tab criteria with green CI, but migration 0795 is left " +
			"unapplied for the front door, which is unfinished work under the migration rule. Dispatch migration-apply " +
			"for 0795 from main, then post the workflow run link as proof.", "a rule conflict", "agent_rules"},
		{"deploy", "Deploy the elk-mcp function through the workflow and link the run.", "a rule conflict", "agent_rules"},
		{"ci running", "The pull request's checks are still running and the lab proof is pending; resubmit when green.",
			"CI was still running", ""},
		// The word alone is not a rule conflict: a review that wants proof in
		// production while checks run is about CI.
		{"production as a place", "Verify the fix in production once the checks are still running finish.",
			"CI was still running", ""},
		{"touch production", "Touch production yourself and repair the rows.", "a rule conflict", "agent_rules"},
		{"missing tool", "Read the failed run with Signal; this session exposes no Signal read tool, so get one.",
			"a missing tool", ""},
		{"other", "The note never names the price: add the tier prices from the camp summary.",
			"kept asking for changes", ""},
	}
	for _, c := range cases {
		got, code := reviewStopReason(c.verdict)
		if !strings.Contains(got, c.want) || code != c.code {
			t.Errorf("%s: got %q (code %q), want it to say %q with code %q", c.name, got, code, c.want, c.code)
		}
		if strings.Contains(got, "A person should look at this") {
			t.Errorf("%s: still the old sentence: %q", c.name, got)
		}
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "Needs you") {
				t.Errorf("%s: a line opens with Needs you, which puts the task on a person's To do: %q", c.name, got)
			}
		}
	}
}
