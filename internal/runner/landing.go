package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	_ "embed"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/secretenv"
	"github.com/elk-work/rein/internal/worktree"
)

// The per-queue landing policy (ark:rein#47): how far a run takes its change.
//
//	merge   push, open a non-draft PR, wait for CI, squash-merge — elk-work's house rule
//	pr      push, open a non-draft PR, and stop — the repository's owners merge
//	branch  push the run branch, and stop — no pull request
//
// The policy reaches the run three ways, and none of them is a control:
//
//  1. The system prompt's "Landing the work" section is the policy's own text.
//     prompt.md carries merge's, unchanged, so a merge queue's prompt is
//     byte-for-byte what it was before the setting existed; the other two
//     replace that one section (landing_pr.md, landing_branch.md).
//  2. "This run", the per-run block with the branch name, states it again.
//  3. After the session, Rein looks at what the run branch left on the remote
//     and puts any breach at the top of the deliverable ([judgeLanding]).
//
// An agent holding a shell can still merge. What stops it is the repository's
// own branch protection, and a `pr` queue belongs on a repository that
// requires a review. The check makes a breach impossible to miss; it does not
// make one impossible.

//go:embed landing_pr.md
var landingPRSection string

//go:embed landing_branch.md
var landingBranchSection string

// The seams in prompt.md the non-merge policies are cut in at. Each must
// match exactly once; [renderLandingPrompts] panics at init when one does not,
// and since the text is compiled in, that is a failure every test run sees
// rather than one a release could ship.
const (
	landingHeading = "## Landing the work\n"
	landingEnd     = "\n## Finish with a deliverable"
	mergeRefs      = "the\npull request URL and its merge sha,"
)

// landedRefs is what each policy's deliverable names in place of merge's
// "the pull request URL and its merge sha".
var landedRefs = map[string]string{
	config.LandPR:     "the\npull request URL and its head sha (open, not merged),",
	config.LandBranch: "the\npushed branch and its head sha (no pull request),",
}

// landingPrompts is the standing prompt each policy renders, built once.
var landingPrompts = renderLandingPrompts(SystemPrompt)

func renderLandingPrompts(base string) map[string]string {
	out := map[string]string{config.LandMerge: base}
	for land, section := range map[string]string{
		config.LandPR:     landingPRSection,
		config.LandBranch: landingBranchSection,
	} {
		rendered, err := renderLanding(base, section, landedRefs[land])
		if err != nil {
			panic(fmt.Sprintf("runner: rendering the %s landing prompt: %v", land, err))
		}
		out[land] = rendered
	}
	return out
}

func renderLanding(base, section, refs string) (string, error) {
	if strings.Count(base, landingHeading) != 1 {
		return "", fmt.Errorf("prompt.md must carry %q exactly once", strings.TrimSpace(landingHeading))
	}
	start := strings.Index(base, landingHeading)
	rel := strings.Index(base[start:], landingEnd)
	if rel < 0 {
		return "", fmt.Errorf("prompt.md has no %q after the landing section", strings.TrimSpace(landingEnd))
	}
	if strings.Count(base, mergeRefs) != 1 {
		return "", fmt.Errorf("prompt.md must name %q exactly once", mergeRefs)
	}
	end := start + rel
	out := base[:start] + strings.TrimRight(section, "\n") + "\n" + base[end:]
	return strings.Replace(out, mergeRefs, refs, 1), nil
}

// landingPrompt is the standing prompt for a policy. Anything unrecognised is
// merge's — but config validation refuses an unknown value at load, so a
// running queue never has one.
func landingPrompt(land string) string {
	if p, ok := landingPrompts[land]; ok {
		return p
	}
	return landingPrompts[config.LandMerge]
}

// landingSummary is one line saying what a policy means, for the opening
// report and the deliverable footer: the places a person sees before and after
// the run what the agent was allowed to do.
func landingSummary(land string) string {
	switch land {
	case config.LandPR:
		return "`pr` — push and open a pull request; the repository's owners merge it"
	case config.LandBranch:
		return "`branch` — push the branch; no pull request"
	default:
		return "`merge` — push, open a pull request, merge it on green CI"
	}
}

// ------------------------------------------------------- the post-run check

// LandingInspector reads what a run left on the remote, for the post-run
// landing check. [GitHubLanding] is the real one; tests supply their own.
type LandingInspector interface {
	Inspect(ctx context.Context, wt *worktree.Worktree) LandingState
}

// LandingState is what a run's branch looks like after the session.
//
// Every half has its own "known" flag, because "Rein could not ask GitHub" and
// "GitHub says there is no pull request" are opposite answers to the question
// the check exists for, and a check that rendered the first as the second
// would report a clean run it had never looked at.
type LandingState struct {
	// Commits is how many commits the run branch carries beyond its base.
	// CommitsKnown is false when git could not say.
	Commits      int
	CommitsKnown bool

	// Pushed reports whether origin has the run branch, and RemoteSHA its head
	// there. PushKnown is false when `git ls-remote` failed.
	Pushed    bool
	RemoteSHA string
	PushKnown bool

	// PRs are the pull requests whose head is the run branch, in any state.
	// PRsKnown is false when `gh` could not be asked.
	PRs      []PullRequest
	PRsKnown bool

	// Problems says what could not be read, one line each.
	Problems []string
}

// PullRequest is one pull request from the run branch, as `gh pr list --json`
// spells it.
type PullRequest struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	State   string `json:"state"` // OPEN, CLOSED or MERGED
	IsDraft bool   `json:"isDraft"`
}

// GitHubLanding inspects with git and the GitHub CLI, in the run's worktree —
// which is still there when the check runs, because the worktree is reaped
// after the deliverable is submitted, not before.
type GitHubLanding struct {
	Env map[string]string
	// Git and GH are the binaries; empty means "git" and "gh" on PATH.
	Git, GH string
	// Retry is the pause before the one retry of a failed `gh` call; zero
	// means five seconds. GitHub's API is the flakiest thing here, and one
	// blip should not leave the check incomplete.
	Retry time.Duration
}

// landingTimeout bounds each subprocess. A slow GitHub must delay the
// deliverable by seconds, not hold the run's claim.
const landingTimeout = 30 * time.Second

// Inspect implements [LandingInspector].
func (g GitHubLanding) Inspect(ctx context.Context, wt *worktree.Worktree) LandingState {
	git, gh := orDefault(g.Git, "git"), orDefault(g.GH, "gh")
	var st LandingState

	if wt.BaseSHA != "" {
		out, err := g.runIn(ctx, wt.Dir, git, "rev-list", "--count", wt.BaseSHA+"..HEAD")
		if n, convErr := strconv.Atoi(strings.TrimSpace(out)); err == nil && convErr == nil {
			st.Commits, st.CommitsKnown = n, true
		} else {
			st.Problems = append(st.Problems, "could not count the branch's commits: "+errLine(err, out))
		}
	}

	out, err := g.runIn(ctx, wt.Dir, git, "ls-remote", "--heads", "origin", "refs/heads/"+wt.Branch)
	if err != nil {
		st.Problems = append(st.Problems, "could not read origin: "+errLine(err, out))
	} else {
		st.PushKnown = true
		if f := strings.Fields(out); len(f) >= 2 {
			st.Pushed, st.RemoteSHA = true, f[0]
		}
	}

	// Nothing committed and nothing pushed is nothing delivered: there is no
	// branch on GitHub for a pull request to come from, so asking would only
	// spend a network call to learn what is already known.
	if st.CommitsKnown && st.Commits == 0 && st.PushKnown && !st.Pushed {
		st.PRsKnown = true
		return st
	}

	retry := g.Retry
	if retry <= 0 {
		retry = 5 * time.Second
	}
	for attempt := 1; ; attempt++ {
		out, err = g.runIn(ctx, wt.Dir, gh, "pr", "list", "--head", wt.Branch, "--state", "all",
			"--json", "number,url,state,isDraft", "--limit", "50")
		if err == nil {
			var prs []PullRequest
			if jerr := json.Unmarshal([]byte(out), &prs); jerr != nil {
				st.Problems = append(st.Problems, "could not read `gh pr list`: "+jerr.Error())
			} else {
				st.PRs, st.PRsKnown = prs, true
			}
			break
		}
		if attempt == 2 || ctx.Err() != nil {
			st.Problems = append(st.Problems, "could not list pull requests: "+errLine(err, out))
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(retry):
		}
	}
	return st
}

// runIn runs one command in dir and returns its combined output.
func runIn(ctx context.Context, dir, bin string, args ...string) (string, error) {
	return (GitHubLanding{}).runIn(ctx, dir, bin, args...)
}
func (g GitHubLanding) runIn(ctx context.Context, dir, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, landingTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if g.Env != nil {
		cmd.Env = secretenv.Filter(os.Environ())
		for k, v := range g.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func errLine(err error, out string) string {
	if strings.TrimSpace(out) != "" {
		return firstLine(out)
	}
	if err != nil {
		return err.Error()
	}
	return "no output"
}

// landingVerdict is what the check concluded.
type landingVerdict struct {
	// Breaches are facts that break the policy — a merge under `pr`, any pull
	// request under `branch`. Any one of them headlines the deliverable.
	Breaches []string
	// Notes are worth a reviewer's eye without breaking anything: no pull
	// request opened, a draft, an unpushed branch, a check that could not run.
	Notes []string
}

// judgeLanding holds a run's branch to its queue's policy. Pure, so each
// policy's rules are tested without git or GitHub.
//
// It looks at the run's own branch and nothing else. An agent that pushed a
// second branch of its own and merged that is outside what this can see — one
// more reason the real control is branch protection.
func judgeLanding(land string, branch string, st LandingState) landingVerdict {
	var v landingVerdict
	if land != config.LandPR && land != config.LandBranch {
		return v
	}
	b := "`" + branch + "`"

	for _, pr := range st.PRs {
		ref := prRef(pr)
		switch {
		case land == config.LandPR && pr.State == "MERGED":
			v.Breaches = append(v.Breaches, ref+" was merged. This queue's policy is `pr`: "+
				"the repository's owners merge, not the run.")
		case land == config.LandBranch && pr.State == "MERGED":
			v.Breaches = append(v.Breaches, ref+" was opened from "+b+" and merged. "+
				"This queue's policy is `branch`: no pull request, and nothing merged.")
		case land == config.LandBranch:
			v.Breaches = append(v.Breaches, ref+" was opened from "+b+" ("+prState(pr)+"). "+
				"This queue's policy is `branch`: no pull request.")
		case pr.State == "OPEN" && pr.IsDraft:
			v.Notes = append(v.Notes, ref+" is a draft; this policy asks for a non-draft pull request.")
		case pr.State == "OPEN":
			v.Notes = append(v.Notes, ref+" is open and not merged, as this policy asks.")
		case pr.State == "CLOSED":
			v.Notes = append(v.Notes, ref+" was closed without merging.")
		}
	}

	delivered := st.Pushed || (st.CommitsKnown && st.Commits > 0)
	if land == config.LandPR && st.PRsKnown && len(st.PRs) == 0 && delivered {
		v.Notes = append(v.Notes, "No pull request was opened from "+b+"; this policy asks for one.")
	}
	switch {
	case st.Pushed:
		v.Notes = append(v.Notes, b+" is on origin at "+shortSHA(st.RemoteSHA)+".")
	case st.PushKnown && st.CommitsKnown && st.Commits > 0:
		v.Notes = append(v.Notes, fmt.Sprintf("%s was not pushed: its %d commit(s) are only in this machine's checkout.",
			b, st.Commits))
	case st.PushKnown && st.CommitsKnown:
		v.Notes = append(v.Notes, "Nothing was committed or pushed, so there was nothing to land.")
	}
	for _, p := range st.Problems {
		v.Notes = append(v.Notes, "Rein "+p+", so this check is incomplete.")
	}
	return v
}

func prRef(pr PullRequest) string {
	if pr.URL != "" {
		return fmt.Sprintf("Pull request #%d (%s)", pr.Number, pr.URL)
	}
	return fmt.Sprintf("Pull request #%d", pr.Number)
}

func prState(pr PullRequest) string {
	s := strings.ToLower(pr.State)
	if pr.IsDraft && pr.State == "OPEN" {
		s = "open, draft"
	}
	if s == "" {
		s = "state unknown"
	}
	return s
}

// banner is the section a breach puts at the very top of the deliverable,
// above the agent's own account — the first thing a reviewer reads.
func (v landingVerdict) banner(land string) string {
	var b strings.Builder
	b.WriteString("## Landing policy breached\n\n")
	fmt.Fprintf(&b, "This queue's landing policy is %s. Rein checked the run branch after the agent finished:\n\n",
		landingSummary(land))
	for _, line := range v.Breaches {
		b.WriteString("- " + line + "\n")
	}
	b.WriteString("\nRein reports a breach; it does not undo one. Look at the change and at why the agent " +
		"went past its policy before accepting this run.\n\n---\n\n")
	return b.String()
}

// footer is the check's result for the "Run details" block.
func (v landingVerdict) footer() string {
	var b strings.Builder
	if len(v.Breaches) == 0 {
		b.WriteString("- Landing check: no breach found\n")
	} else {
		fmt.Fprintf(&b, "- Landing check: **%d breach(es)** — see the top of this deliverable\n", len(v.Breaches))
	}
	for _, n := range v.Notes {
		b.WriteString("  - " + n + "\n")
	}
	return b.String()
}

// checkLanding runs the post-run check for this queue's policy. A merge queue
// is not checked at all — merge is the policy that cannot be exceeded — so
// Elk's own queues make no extra calls and their deliverables read as before
// apart from the policy line in the footer.
func (qr *queueRunner) checkLanding(ctx context.Context, wt *worktree.Worktree) (landingVerdict, bool) {
	land := qr.q.LandOrDefault()
	if wt == nil || (land != config.LandPR && land != config.LandBranch) {
		return landingVerdict{}, false
	}
	st := qr.r.opts.Landing.Inspect(ctx, wt)
	v := judgeLanding(land, wt.Branch, st)
	for _, line := range v.Breaches {
		qr.logf("landing policy %s breached: %s", land, line)
		qr.log.Runner(runlog.KindNote, "landing policy %s breached: %s", land, line)
	}
	for _, line := range v.Notes {
		qr.log.Runner(runlog.KindNote, "landing check: %s", line)
	}
	return v, true
}
