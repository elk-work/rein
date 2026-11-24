package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/worktree"
)

// Yielding the machine to its GitHub Actions runner.
//
// Elk Scout's Windows lab machines, win-01 and win-02, are GitHub Actions
// runners first: the Windows port train runs on both, and win-02 is the one
// interactive desktop for on-screen device checks. Casey's ruling of
// 2026-11-23 lets them take Elk agent work too, but only while no such job is
// waiting for them. GitHub's runner is its own process and starts a job the
// moment GitHub hands it one, whatever Rein is doing, so "the job comes first"
// takes two things on Rein's side:
//
//   - Rein claims nothing while a job is waiting for, or running on, this
//     machine's runner. It also claims nothing while it cannot tell: a lookup
//     that fails holds claims exactly as a job does (failing closed), so a
//     GitHub outage or a lost `gh` login can only leave the machine idle,
//     never put agent work in front of a port.
//   - An agent run already going when a job turns up is stopped once
//     [config.YieldToGitHub.Grace] has passed, its committed work pushed, and
//     the run handed back to Elk to start again elsewhere. A failed lookup
//     never stops a run: only a job actually seen does.

// errYielded is what a run stopped for a GitHub Actions job returns. The
// serve loop treats it as a finished claim, not a failed one.
var errYielded = errors.New("stopped for a GitHub Actions job this machine runs first")

// PriorityJob is one GitHub Actions job that comes before agent work here.
type PriorityJob struct {
	Repo     string   `json:"repo"`
	Workflow string   `json:"workflow"`
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	Runner   string   `json:"runner_name"`
	Labels   []string `json:"labels"`
	URL      string   `json:"html_url"`
}

// PriorityLookup returns the open (not completed) jobs in the configured
// repositories and workflows. [priorityJobs] decides which are this
// machine's.
type PriorityLookup func(ctx context.Context, y config.YieldToGitHub) ([]PriorityJob, error)

// priorityJobs keeps the jobs that come before agent work on this machine:
// waiting with labels this runner carries, or running on this runner.
//
// "Labels this runner carries" is GitHub's own rule for which runner may take
// a job: every label in the job's runs-on must be on the runner. Scout's lab
// jobs name the host (["self-hosted","windows","win11","ui-lab","win-01"]), so
// win-02 does not stop for a job only win-01 can take.
func priorityJobs(y config.YieldToGitHub, jobs []PriorityJob) []PriorityJob {
	have := map[string]bool{}
	for _, l := range y.Labels {
		have[strings.ToLower(strings.TrimSpace(l))] = true
	}
	var out []PriorityJob
	for _, j := range jobs {
		switch j.Status {
		case "in_progress":
			if strings.EqualFold(j.Runner, y.Runner) {
				out = append(out, j)
			}
		case "queued", "waiting", "pending", "requested":
			if len(j.Labels) == 0 {
				continue
			}
			ours := true
			for _, l := range j.Labels {
				if !have[strings.ToLower(l)] {
					ours = false
					break
				}
			}
			if ours {
				out = append(out, j)
			}
		}
	}
	return out
}

// yieldLookupTimeout bounds one lookup: a call per open run, so a busy
// repository watched with no workflows named takes a while. A lookup that
// runs out fails closed, which is why `workflows` should be set.
const yieldLookupTimeout = 2 * time.Minute

// yieldGate is the machine's one watch on its GitHub Actions runner, shared
// by every queue.
type yieldGate struct {
	y      config.YieldToGitHub
	lookup PriorityLookup
	logf   func(string, ...any)
	now    func() time.Time

	// lookupMu runs one lookup at a time, so an older answer can never land
	// after a newer one.
	lookupMu sync.Mutex

	mu sync.Mutex
	// checked is when the last lookup finished; zero before the first.
	checked time.Time
	// jobs is the last successful reading's priority jobs, and err the last
	// lookup's failure (nil when it succeeded).
	jobs []PriorityJob
	err  error
	// since is when the jobs now waiting were first seen, zero when the last
	// successful reading had none. A failed lookup leaves it as it was.
	since time.Time
	// sessions are the agent sessions running now, each with the function
	// that stops it.
	sessions map[uint64]yieldSession
	nextID   uint64
	// said is the last state sentence logged, so each change is logged once.
	said string
}

type yieldSession struct {
	runID string
	stop  context.CancelCauseFunc
}

func newYieldGate(y config.YieldToGitHub, lookup PriorityLookup, logf func(string, ...any)) *yieldGate {
	return &yieldGate{y: y, lookup: lookup, logf: logf, now: time.Now, sessions: map[uint64]yieldSession{}}
}

// refresh asks GitHub once and records the answer, then stops any session
// whose grace has run out.
func (g *yieldGate) refresh(ctx context.Context) {
	asked := g.now()
	g.lookupMu.Lock()
	defer g.lookupMu.Unlock()
	g.mu.Lock()
	answered := g.checked.After(asked)
	g.mu.Unlock()
	if answered {
		// A lookup that started while this one waited has answered for it.
		g.preempt()
		return
	}
	lctx, cancel := context.WithTimeout(ctx, yieldLookupTimeout)
	jobs, err := g.lookup(lctx, g.y)
	cancel()
	if ctx.Err() != nil {
		return
	}
	g.mu.Lock()
	g.checked = g.now()
	g.err = err
	if err == nil {
		g.jobs = priorityJobs(g.y, jobs)
		switch {
		case len(g.jobs) == 0:
			g.since = time.Time{}
		case g.since.IsZero():
			g.since = g.checked
		}
	}
	said := g.reasonLocked()
	changed := said != g.said
	g.said = said
	g.mu.Unlock()
	if changed {
		if said == "" {
			g.logf("GitHub runner %s: no job waiting for it; claiming agent work", g.y.Runner)
		} else {
			g.logf("%s", said)
		}
	}
	g.preempt()
}

// holdFresh is [yieldGate.hold] on a reading taken now: the last word before
// a claim.
func (g *yieldGate) holdFresh(ctx context.Context) string {
	g.refresh(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reasonLocked()
}

// hold is the sentence saying why no claim may be made now, or "" when one
// may. A reading older than two intervals (or none at all) is renewed first,
// synchronously, so a claim is never made on a stale "all clear".
func (g *yieldGate) hold(ctx context.Context) string {
	g.mu.Lock()
	stale := g.checked.IsZero() || g.now().Sub(g.checked) > 2*g.y.IntervalOrDefault()
	g.mu.Unlock()
	if stale {
		g.refresh(ctx)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reasonLocked()
}

// reasonLocked is the claim-hold sentence for the current state. Every part
// of it is Rein's own or GitHub's job names: it rides the heartbeat to Elk.
func (g *yieldGate) reasonLocked() string {
	switch {
	case g.err != nil:
		return fmt.Sprintf("not claiming: could not ask GitHub whether runner %s has a job waiting (%s), "+
			"so this machine stays on its GitHub jobs until it can", g.y.Runner, clip(firstLine(g.err.Error()), 160))
	case len(g.jobs) > 0:
		j := g.jobs[0]
		what := j.Name
		if j.Workflow != "" {
			what = j.Workflow + ": " + j.Name
		}
		more := ""
		if n := len(g.jobs) - 1; n > 0 {
			more = fmt.Sprintf(" and %d more", n)
		}
		verb := "is waiting for"
		if j.Status == "in_progress" {
			verb = "is running on"
		}
		return fmt.Sprintf("not claiming: GitHub Actions job %q%s %s runner %s, and it comes first", what, more, verb, g.y.Runner)
	}
	return ""
}

// dueLocked reports whether a job has been seen for at least the grace
// period, on a lookup that succeeded: a failed one never stops a run.
func (g *yieldGate) dueLocked() bool {
	return g.err == nil && len(g.jobs) > 0 && !g.since.IsZero() && g.now().Sub(g.since) >= g.y.GraceOrDefault()
}

// preempt stops every session when a job has waited out the grace period.
func (g *yieldGate) preempt() {
	g.mu.Lock()
	if !g.dueLocked() {
		g.mu.Unlock()
		return
	}
	stops := make([]yieldSession, 0, len(g.sessions))
	for _, s := range g.sessions {
		stops = append(stops, s)
	}
	g.mu.Unlock()
	for _, s := range stops {
		g.logf("run %s: stopping the agent; a GitHub Actions job for runner %s has waited %s", s.runID, g.y.Runner, g.y.GraceOrDefault())
		s.stop(errYielded)
	}
}

// watch registers an agent session and returns the context that is cancelled
// (with cause [errYielded]) when the session must give way, and the function
// that unregisters it. A session that starts after the grace has already run
// out is stopped at once.
func (g *yieldGate) watch(ctx context.Context, runID string) (context.Context, func()) {
	wctx, stop := context.WithCancelCause(ctx)
	g.mu.Lock()
	g.nextID++
	id := g.nextID
	g.sessions[id] = yieldSession{runID: runID, stop: stop}
	g.mu.Unlock()
	g.preempt()
	return wctx, func() {
		g.mu.Lock()
		delete(g.sessions, id)
		g.mu.Unlock()
		stop(nil)
	}
}

// run asks GitHub every interval, and once more the moment a job's grace runs
// out, until ctx is done.
func (g *yieldGate) run(ctx context.Context) {
	g.logf("GitHub runner %s comes first: no claims while a job waits for it in %s; a running agent gets %s, then stops and is handed back",
		g.y.Runner, strings.Join(g.y.Repos, ", "), g.y.GraceOrDefault())
	for {
		g.refresh(ctx)
		wait := g.y.IntervalOrDefault()
		g.mu.Lock()
		if len(g.jobs) > 0 && !g.dueLocked() {
			if left := g.since.Add(g.y.GraceOrDefault()).Sub(g.now()); left < wait {
				wait = left
			}
		}
		g.mu.Unlock()
		if wait < time.Second {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// GitHubPriorityJobs asks GitHub with the GitHub CLI, on its own login. gh
// empty means "gh" on PATH.
func GitHubPriorityJobs(gh string) PriorityLookup {
	if gh == "" {
		gh = "gh"
	}
	return func(ctx context.Context, y config.YieldToGitHub) ([]PriorityJob, error) {
		var all []PriorityJob
		for _, repo := range y.Repos {
			scopes := []string{"repos/" + repo + "/actions/runs"}
			if len(y.Workflows) > 0 {
				scopes = scopes[:0]
				for _, wf := range y.Workflows {
					scopes = append(scopes, "repos/"+repo+"/actions/workflows/"+wf+"/runs")
				}
			}
			// queued runs hold queued jobs; an in_progress run can too (the
			// Android legs of a train whose Windows legs are running).
			seen := map[int64]bool{}
			for _, scope := range scopes {
				for _, status := range []string{"queued", "in_progress"} {
					lines, err := ghLines(ctx, gh, scope+"?per_page=100&status="+status,
						`.workflow_runs[] | {id, path}`)
					if err != nil {
						return nil, err
					}
					for _, line := range lines {
						var run struct {
							ID   int64  `json:"id"`
							Path string `json:"path"`
						}
						if err := json.Unmarshal([]byte(line), &run); err != nil {
							return nil, fmt.Errorf("could not read GitHub's list of runs: %w", err)
						}
						if seen[run.ID] {
							continue
						}
						seen[run.ID] = true
						jobs, err := ghLines(ctx, gh, fmt.Sprintf("repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", repo, run.ID),
							`.jobs[] | select(.status != "completed") | {name, status, runner_name, labels, html_url}`)
						if err != nil {
							return nil, err
						}
						wf := run.Path[strings.LastIndex(run.Path, "/")+1:]
						for _, jl := range jobs {
							var j PriorityJob
							if err := json.Unmarshal([]byte(jl), &j); err != nil {
								return nil, fmt.Errorf("could not read GitHub's list of jobs: %w", err)
							}
							j.Repo, j.Workflow = repo, wf
							all = append(all, j)
						}
					}
				}
			}
		}
		return all, nil
	}
}

// ghLines runs `gh api --paginate` with a jq filter and returns its lines.
func ghLines(ctx context.Context, gh, path, jq string) ([]string, error) {
	cmd := exec.CommandContext(ctx, gh, "api", "--paginate", "-H", "Accept: application/vnd.github+json", path, "--jq", jq)
	// Plain lines whatever the service's environment says about colour.
	cmd.Env = append(os.Environ(), "CLICOLOR_FORCE=0", "GH_FORCE_TTY=", "NO_COLOR=1")
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(strings.TrimSpace(string(exit.Stderr))) > 0 {
			return nil, fmt.Errorf("gh api: %s", firstLine(string(exit.Stderr)))
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("the GitHub CLI (gh) is not installed")
		}
		return nil, fmt.Errorf("gh api: %w", err)
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, sc.Err()
}

// yieldBack hands a run stopped for a GitHub Actions job back to Elk, after
// pushing what the agent had committed so nothing committed is lost with the
// worktree.
//
// Only committed work is pushed. Committing what the agent left uncommitted
// would mean running git in the worktree, whose config the agent controls —
// the reason [worktree.Worktree.PinnedGit] exists — so it is not done, and the
// hand-back says so.
func (qr *queueRunner) yieldBack(ctx context.Context, wo *elk.WorkOrder, wt *worktree.Worktree, pending *elk.Usage) {
	qr.handedBack = true
	runner := qr.r.opts.Config.YieldToGitHub.Runner
	pushed, note := qr.pushRunBranch(ctx, wt)
	if note != "" {
		qr.logf("run %s: %s", wo.RunID, note)
	}
	branch := ""
	if wt != nil {
		branch = wt.Branch
	}
	what := fmt.Sprintf("%s stopped this run because a GitHub Actions job for runner %s was waiting, and that machine's GitHub jobs come before agent work.",
		qr.reinHere(), runner)
	err := qr.handBackAs(ctx, wo.RunID, branch, "Stopped for a GitHub Actions job", what, pushed, pending)
	switch {
	case err == nil:
		qr.logf("run %s: handed back to Elk for runner %s's GitHub job; Elk starts it again", wo.RunID, runner)
		qr.log.Runner(runlog.KindSubmit, "handed back to Elk: stopped for a GitHub Actions job")
	case errors.Is(err, elk.ErrCancelled):
		qr.logf("run %s: nothing to hand back, Elk says the run is no longer running", wo.RunID)
	default:
		qr.logf("run %s: could not hand the run back to Elk (%s); its claim lapses in up to 15 minutes and Elk offers it again",
			wo.RunID, handBackFailure(err))
	}
}

// pushTimeout bounds the push of a stopped run's branch.
const pushTimeout = time.Minute

// pushRunBranch pushes the run's branch to origin when it has commits beyond
// its base. It reports whether the branch is on origin afterwards, and a line
// for the log when something was done or went wrong.
func (qr *queueRunner) pushRunBranch(ctx context.Context, wt *worktree.Worktree) (bool, string) {
	if wt == nil || wt.Hosted || wt.Detached || wt.Branch == "" || wt.BaseSHA == "" {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTimeout)
	defer cancel()
	dir, pinned, ref, err := wt.PinnedGit(ctx, "git")
	if err != nil {
		return false, "could not push the branch: " + err.Error()
	}
	g := GitHubLanding{}
	out, err := g.runEnv(ctx, dir, pinned, "git", worktree.SafeGit("rev-list", "--count", wt.BaseSHA+".."+ref)...)
	if err != nil {
		return false, "could not count the branch's commits: " + errLine(err, out)
	}
	if strings.TrimSpace(out) == "0" {
		return false, ""
	}
	out, err = g.runEnv(ctx, dir, pinned, "git", worktree.SafeGit("push", "origin", ref+":refs/heads/"+wt.Branch)...)
	if err != nil {
		return false, "could not push branch " + wt.Branch + ": " + errLine(err, out)
	}
	return true, "pushed branch " + wt.Branch + " before handing the run back"
}
