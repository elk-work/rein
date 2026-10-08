package runner_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runner"
	"github.com/elk-work/rein/internal/worktree"
)

// The landing policy through the whole loop (ark:rein#47): what the agent is
// told, what Elk hears before and after, and what reaches the executor row.

// fakeLanding is the post-run check's view of GitHub, scripted.
type fakeLanding struct {
	mu    sync.Mutex
	state runner.LandingState
	seen  []string
}

func (f *fakeLanding) Inspect(_ context.Context, wt *worktree.Worktree) runner.LandingState {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, wt.Branch)
	return f.state
}

func (f *fakeLanding) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func TestAMergeQueueRunsAsItAlwaysHas(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	landing := &fakeLanding{}

	if err := h.run(runner.Options{Landing: landing}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if got := h.agent.Specs()[0].SystemPrompt; !strings.HasPrefix(got, runner.SystemPrompt) {
		t.Error("a queue with no landing policy was not given the vendored prompt as it stands")
	}
	// Merge is the policy that cannot be exceeded, so it is not checked: no
	// call to GitHub, and no check in the deliverable.
	if n := len(landing.calls()); n != 0 {
		t.Errorf("the landing check ran %d time(s) on a merge queue", n)
	}
	sub := h.submitted()
	if strings.HasPrefix(sub.Arg("title"), "Landing policy breached") {
		t.Errorf("title = %q", sub.Arg("title"))
	}
	d := sub.Arg("deliverable")
	if !strings.Contains(d, "- Landing policy: `merge`") || strings.Contains(d, "Landing check") {
		t.Errorf("the merge footer is wrong:\n%s", d)
	}
}

func TestAPRQueueIsToldItsPolicyAndCheckedAfterTheRun(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Land = config.LandPR
	h.elk.Text("claim_run", order("run-1"))
	landing := &fakeLanding{state: runner.LandingState{
		Commits: 1, CommitsKnown: true, Pushed: true, RemoteSHA: "feedface00", PushKnown: true, PRsKnown: true,
		PRs: []runner.PullRequest{{Number: 41, URL: "https://github.com/acme/app/pull/41", State: "OPEN"}},
	}}

	if err := h.run(runner.Options{Landing: landing}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	prompt := h.agent.Specs()[0].SystemPrompt
	for _, want := range []string{"This queue's landing policy is `pr`", "Landing policy: `pr`", "Stop there. Do not merge it"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the pr queue's prompt is missing %q", want)
		}
	}
	if strings.Contains(prompt, "Merge it yourself") {
		t.Error("the pr queue was still told to merge")
	}
	if !strings.Contains(h.elk.CallsTo("report_progress")[0].Arg("body"), "Landing policy: `pr`") {
		t.Error("the opening report does not say the run's landing policy")
	}

	if got := landing.calls(); len(got) != 1 || got[0] != worktree.BranchPrefix+"run-1" {
		t.Fatalf("the check inspected %q, want the run's own branch once", got)
	}
	sub := h.submitted()
	if sub.Arg("status") != "ready" || strings.HasPrefix(sub.Arg("title"), "Landing policy breached") {
		t.Errorf("a clean pr run was submitted as %q / %q", sub.Arg("status"), sub.Arg("title"))
	}
	d := sub.Arg("deliverable")
	for _, want := range []string{"- Landing policy: `pr`", "- Landing check: no breach found", "#41", "open and not merged"} {
		if !strings.Contains(d, want) {
			t.Errorf("the deliverable is missing %q:\n%s", want, d)
		}
	}
}

func TestAMergeOnAPRQueueHeadlinesTheDeliverable(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Land = config.LandPR
	h.elk.Text("claim_run", order("run-1"))
	landing := &fakeLanding{state: runner.LandingState{
		Commits: 1, CommitsKnown: true, Pushed: true, PushKnown: true, PRsKnown: true,
		PRs: []runner.PullRequest{{Number: 41, URL: "https://github.com/acme/app/pull/41", State: "MERGED"}},
	}}

	if err := h.run(runner.Options{Landing: landing}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	sub := h.submitted()
	// Still `ready`: the work is what it is, and ready puts it in front of a
	// person — but nobody can accept it without reading the breach first.
	if sub.Arg("status") != "ready" {
		t.Errorf("status = %q", sub.Arg("status"))
	}
	if !strings.HasPrefix(sub.Arg("title"), "Landing policy breached: ") {
		t.Errorf("title = %q", sub.Arg("title"))
	}
	d := sub.Arg("deliverable")
	if !strings.HasPrefix(d, "## Landing policy breached\n") || !strings.Contains(d, "#41 (https://github.com/acme/app/pull/41) was merged") {
		t.Errorf("the breach is not the first thing in the deliverable:\n%s", d)
	}
	if !strings.Contains(d, "fake session completed") {
		t.Errorf("the agent's own account was dropped:\n%s", d)
	}
	var said bool
	for _, r := range h.elk.CallsTo("report_progress") {
		said = said || strings.Contains(r.Arg("body"), "Landing check: the run went past its queue's policy (pr)")
	}
	if !said {
		t.Error("the run's timeline does not record the breach")
	}
	if !strings.Contains(h.log.String(), "landing policy pr breached") {
		t.Errorf("the local log does not record the breach:\n%s", h.log)
	}
}

func TestAPROnABranchQueueIsABreach(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].Land = config.LandBranch
	h.elk.Text("claim_run", order("run-1"))
	landing := &fakeLanding{state: runner.LandingState{
		Commits: 2, CommitsKnown: true, Pushed: true, PushKnown: true, PRsKnown: true,
		PRs: []runner.PullRequest{{Number: 5, State: "OPEN"}},
	}}

	if err := h.run(runner.Options{Landing: landing}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	prompt := h.agent.Specs()[0].SystemPrompt
	if !strings.Contains(prompt, "This queue's landing policy is `branch`") || !strings.Contains(prompt, "Landing policy: `branch`") {
		t.Error("the branch queue was not told its policy")
	}
	sub := h.submitted()
	if !strings.HasPrefix(sub.Arg("title"), "Landing policy breached: ") ||
		!strings.Contains(sub.Arg("deliverable"), "Pull request #5 was opened from `"+worktree.BranchPrefix+"run-1`") {
		t.Errorf("a PR on a branch queue was not reported as a breach:\n%s", sub.Arg("deliverable"))
	}
}

func TestTheExecutorRowCarriesTheLandingPolicy(t *testing.T) {
	for _, tc := range []struct{ land, want string }{{"", "merge"}, {config.LandPR, "pr"}, {config.LandBranch, "branch"}} {
		t.Run(tc.want, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Queues[0].Land = tc.land
			if err := h.run(runner.Options{Landing: &fakeLanding{}}); err != nil {
				t.Fatalf("%v\nlog:\n%s", err, h.log)
			}
			beats := h.elk.CallsTo("heartbeat_executor")
			if len(beats) == 0 {
				t.Fatal("no heartbeat")
			}
			session, ok := beats[0].Args["session"].(map[string]any)
			if !ok {
				t.Fatalf("session = %T", beats[0].Args["session"])
			}
			if session["land"] != tc.want {
				t.Errorf("session.land = %v, want %q", session["land"], tc.want)
			}
			if !strings.Contains(h.log.String(), "landing policy "+tc.want) {
				t.Errorf("the startup line does not say the policy:\n%s", h.log)
			}
		})
	}
}
