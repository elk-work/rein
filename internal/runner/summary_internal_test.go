package runner

import (
	"strings"
	"testing"
)

// TestUnfinishedAccount pins which closing texts are asked again
// (ark:rein#83). The first two are Oct 14's, verbatim.
func TestUnfinishedAccount(t *testing.T) {
	unfinished := []string{
		"",
		"   \n",
		"The pull request's checks are still running. I'll merge it once they pass.",
		"Auto-merge is set on the pull request, so it lands when its checks pass. I'm waiting for that result before I write the final report.",
		"The pull request is open and its checks are running; I'll merge once they're green.",
		"PR opened. Waiting for CI.",
		"Let me check the CI run.",
		"I will deploy the function next.",
		"Opened #12. It merges once the checks pass.",
	}
	finished := []string{
		"Merged elk-work/rein#13 as abc1234. Nothing left.",
		"Done: the fix landed in #2171 (merge d6f1395), and the tests pass.",
		"Blocked: the Supabase token lacks edge-function permission, so the deploy did not run. Issac can grant it.",
		// A long report that mentions later steps is a report with a
		// what's-left section, not a promise.
		strings.Repeat("Changed the server rule and the notification tag; both merged. ", 6) +
			"Issac will deploy the edge function once his token has the permission.",
	}
	for _, s := range unfinished {
		if unfinishedAccount(s) == "" {
			t.Errorf("not asked again: %q", s)
		}
	}
	for _, s := range finished {
		if why := unfinishedAccount(s); why != "" {
			t.Errorf("asked again (%s): %q", why, s)
		}
	}
}
