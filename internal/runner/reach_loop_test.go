package runner_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/runner"
)

// The sweep as Pace sends it (Elk Scout #928): a direction naming the
// workflow, a repo: line, and no declared capabilities. Driven through the
// loop, so the clause is checked where the agent actually receives it.
func TestASweepRunIsToldWhatIsInReach(t *testing.T) {
	h := newHarness(t)
	home := t.TempDir()
	elkDir, scoutDir := filepath.Join(home, "elk"), filepath.Join(home, "elk", "scout")
	if err := os.MkdirAll(scoutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.cfg.Repos["elk-work/elk"] = elkDir
	h.cfg.Repos["elk-work/scout"] = scoutDir
	h.elk.Text("claim_run", "Claimed Elk run `run-1` (contract v2).\n\n"+
		"**Direction:** Run to completion: Run the org-wide PR sweep when a pull request has been left stranded\n"+
		"**Action id:** `act-1`\n"+
		"repo: elk-work/elk\n")

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	prompt := h.agent.Specs()[0].SystemPrompt
	for _, want := range []string{
		"## Repositories in reach",
		"`elk-work/scout`, its clone at `" + scoutDir + "`",
		filepath.Join(elkDir, "scripts", "pr-sweep-state.mjs"),
		"Do not reach outside it,\nexcept into what \"Repositories in reach\" below names for this run.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the sweep run's prompt is missing %q", want)
		}
	}
	if !strings.Contains(h.log.String(), "repositories in reach: elk-work/elk, elk-work/scout (the org-wide PR sweep)") {
		t.Errorf("the log does not say what was in reach:\n%s", h.log)
	}
}

// An ordinary run gets exactly the prompt it had before reach existed.
func TestAnOrdinaryRunIsToldNothingIsInReach(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	prompt := h.agent.Specs()[0].SystemPrompt
	if strings.Contains(prompt, "Repositories in reach") {
		t.Error("a run that declares nothing and names no workflow must not be told anything is in reach")
	}
}
