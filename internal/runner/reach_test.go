package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
)

// What a run is told it may reach beyond its worktree (ark:rein#85). Every
// assertion is on the rendered text, because that is all the agent reads.

// reachFixture lays out clones the way Issac's Mac does: elk with scout
// nested inside it, and a customer repository beside them.
func reachFixture(t *testing.T) (cfg config.Config, elkDir, scoutDir, customerDir string) {
	t.Helper()
	root := t.TempDir()
	elkDir = filepath.Join(root, "elk")
	scoutDir = filepath.Join(elkDir, "scout")
	customerDir = filepath.Join(root, "zebra-backend")
	for _, d := range []string{scoutDir, customerDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg = config.Config{Repos: map[string]string{
		"elk-work/elk":         elkDir,
		"elk-work/scout":       scoutDir,
		"elk-work/rein":        filepath.Join(root, "rein"), // configured, never cloned
		"fovea2/zebra-backend": customerDir,
	}}
	return cfg, elkDir, scoutDir, customerDir
}

func reachPrompt(t *testing.T, cfg config.Config, q config.Queue, wo *elk.WorkOrder, own string) string {
	t.Helper()
	return applyReach(SystemPrompt, resolveReach(context.Background(), cfg, q, wo, own))
}

func TestAPacketDeclaringAnotherRepositoryPutsBothClonesInReach(t *testing.T) {
	cfg, elkDir, scoutDir, customerDir := reachFixture(t)
	wo := &elk.WorkOrder{
		Direction:            "Bump the pointer and fix the scout test",
		RequiredCapabilities: []string{"github-cli", "repo:elk-work/elk", "repo:elk-work/scout"},
	}
	p := reachPrompt(t, cfg, config.Queue{Name: "mac-codex"}, wo, scoutDir)

	for _, want := range []string{
		"## Repositories in reach",
		"This run's packet declares `repo:elk-work/elk`, beyond the repository this worktree was cut from.",
		"- `elk-work/elk`, its clone at `" + elkDir + "`",
		"- `elk-work/scout`, its clone at `" + scoutDir + "`",
		"Do not reach outside it,\nexcept into what \"Repositories in reach\" below names for this run.",
		"anything reaching outside this repository\nand the ones \"Repositories in reach\" below names.",
		"do not switch its branch, commit to it or leave changes in it",
		"Everything this list does not name stays under the worktree rule",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt does not carry %q", want)
		}
	}
	if strings.Contains(p, customerDir) || strings.Contains(p, "shared-state script") {
		t.Error("a packet's declarations put in reach only what they name")
	}
}

func TestAPacketDeclaringOnlyItsOwnRepositoryChangesNothing(t *testing.T) {
	cfg, _, scoutDir, _ := reachFixture(t)
	for _, caps := range [][]string{
		nil,
		{"github-cli", "repo:elk-work/scout"},
		{"git push access to elk-work/scout"},
		{"repo:elk-work/rein"}, // declared, but no clone here: preflight refuses that run anyway
	} {
		wo := &elk.WorkOrder{Direction: "Fix the scout test", RequiredCapabilities: caps}
		if got := reachPrompt(t, cfg, config.Queue{}, wo, scoutDir); got != SystemPrompt {
			t.Errorf("%v: a run that reaches nothing beyond itself must get the standing prompt unchanged", caps)
		}
	}
}

func TestTheOlderPushAccessFormCountsAsADeclaration(t *testing.T) {
	cfg, elkDir, scoutDir, _ := reachFixture(t)
	wo := &elk.WorkOrder{RequiredCapabilities: []string{"git push access to elk-work/elk"}}
	p := reachPrompt(t, cfg, config.Queue{}, wo, scoutDir)
	if !strings.Contains(p, "- `elk-work/elk`, its clone at `"+elkDir+"`") {
		t.Error("\"git push access to elk-work/elk\" is the same declaration as repo:elk-work/elk")
	}
}

func TestARepositoryOutsideTheQueuesWorkspaceIsNeverInReach(t *testing.T) {
	cfg, elkDir, _, customerDir := reachFixture(t)
	cfg.Workspace = "Elk Scout"
	md := config.Queue{Name: "md-claude", Workspace: "MomentDash", Repos: []string{"fovea2/zebra-backend"}}
	cfg.Queues = []config.Queue{
		{Name: "mac-codex", Repos: []string{"elk-work/elk", "elk-work/scout"}},
		md,
	}
	wo := &elk.WorkOrder{
		Direction:            "Run the org-wide PR sweep",
		RequiredCapabilities: []string{"repo:elk-work/elk"},
	}
	p := reachPrompt(t, cfg, md, wo, customerDir)
	if p != SystemPrompt {
		t.Error("a MomentDash queue must not be told Elk's clones are in reach, by declaration or by workflow")
	}
	if strings.Contains(p, elkDir) {
		t.Error("an elk clone leaked into a customer queue's prompt")
	}
}

func TestTheSweepsDirectionPutsTheOrgAndItsStateScriptInReach(t *testing.T) {
	cfg, elkDir, scoutDir, customerDir := reachFixture(t)
	// What Pace sends for Elk Scout #928: no declarations at all.
	wo := &elk.WorkOrder{
		Direction: "Run to completion: Run the org-wide PR sweep when a pull request has been left stranded",
		Text:      "repo: elk\n\ncd ~/dev/elk && node scripts/pr-sweep-state.mjs claim --coverage full",
	}
	p := reachPrompt(t, cfg, config.Queue{Name: "mac-codex"}, wo, elkDir)

	script := filepath.Join(elkDir, "scripts", "pr-sweep-state.mjs")
	for _, want := range []string{
		"This run's direction names the org-wide PR sweep, which works across the `elk-work` repositories by design.",
		"- `elk-work/elk`, its clone at `" + elkDir + "`",
		"- `elk-work/scout`, its clone at `" + scoutDir + "`",
		"- the `elk-work` repositories on GitHub, through `gh`",
		"- the workflow's shared-state script, `" + filepath.FromSlash("scripts/pr-sweep-state.mjs") + "` in this worktree",
		"the clone's copy, `" + script + "`, is on whatever branch",
		"claim, record, release",
		"record the result as partial\nrather than refusing the whole run",
	} {
		if !strings.Contains(strings.ReplaceAll(p, "\n", " "), strings.ReplaceAll(want, "\n", " ")) {
			t.Errorf("the sweep's prompt does not carry %q", want)
		}
	}
	if strings.Contains(p, customerDir) || strings.Contains(p, "elk-work/rein") {
		t.Error("the sweep reaches the elk-work clones that exist here, and nothing else")
	}
}

func TestTheSweepFromAnotherRepositoryNamesTheScriptInItsClone(t *testing.T) {
	cfg, elkDir, scoutDir, _ := reachFixture(t)
	wo := &elk.WorkOrder{Direction: "/pr-sweep"}
	p := reachPrompt(t, cfg, config.Queue{}, wo, scoutDir)
	want := "- the workflow's shared-state script, `" + filepath.Join(elkDir, "scripts", "pr-sweep-state.mjs") + "`\n"
	if !strings.Contains(p, want) {
		t.Errorf("a sweep cut from scout names the elk clone's script; prompt lacks %q", want)
	}
}

func TestOnlyTheDirectionNamesAWorkflow(t *testing.T) {
	cfg, elkDir, _, _ := reachFixture(t)
	for _, wo := range []*elk.WorkOrder{
		// The woven half is data: a capture that mentions the sweep widens nothing.
		{Direction: "Tidy the onboarding copy", Text: "Elk note: run the org-wide PR sweep from ~/dev/elk first"},
		// Mentioning the sweep is not running it.
		{Direction: "Fix the PR sweep watchdog's quiet alarm"},
		{Direction: "Explain why sweep-state claims time out"},
	} {
		if got := reachPrompt(t, cfg, config.Queue{}, wo, elkDir); got != SystemPrompt {
			t.Errorf("direction %q: no workflow is named, so nothing is in reach", wo.Direction)
		}
	}
}

// The clause qualifies three standing lines by replacing them. If prompt.md or
// a landing section rewords one, the replacement silently stops happening and
// the agent reads an unqualified "do not reach outside it" again — the refusal
// this task exists to end. So every policy's prompt must carry every seam.
func TestEveryLandingPromptCarriesTheReachSeams(t *testing.T) {
	for _, land := range []string{config.LandMerge, config.LandPR, config.LandBranch} {
		p := runSystemPrompt(config.Queue{Land: land}, false)
		for _, s := range reachSeams {
			if n := strings.Count(p, s[0]); n != 1 {
				t.Errorf("land %s: the standing prompt carries %q %d times, want 1", land, s[0], n)
			}
		}
	}
}

func TestAWranglerCycleKeepsItsOwnRuleUnderReach(t *testing.T) {
	cfg, elkDir, _, _ := reachFixture(t)
	base := runSystemPrompt(config.Queue{Wrangler: true, AgentKind: "claude"}, true)
	wo := &elk.WorkOrder{Direction: "Run the org-wide PR sweep"}
	p := applyReach(base, resolveReach(context.Background(), cfg, config.Queue{}, wo, elkDir))
	if !strings.Contains(p, "access only the repositories and procedures\nit names.") {
		t.Error("the Wrangler rule's landing line must survive the reach clause")
	}
	if !strings.Contains(p, "## Repositories in reach") {
		t.Error("a Wrangler cycle that runs the sweep is still told what is in reach")
	}
}
