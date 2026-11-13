package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	checkadapter "github.com/elk-work/rein/internal/adapter/check"
	geminiadapter "github.com/elk-work/rein/internal/adapter/gemini"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/hosted"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/secretenv"
	"github.com/elk-work/rein/internal/worktree"
)

// drive takes one claimed run all the way: the fail-closed checks, the
// worktree, the session, and exactly one terminal report to Elk.
//
// Every path out of here either submits a deliverable or has been told by Elk
// that the run is no longer running. Returning without doing one of those two
// things would leave a claim to lapse and the run to be handed to somebody
// else fifteen minutes later, having already been worked.
func (qr *queueRunner) drive(ctx context.Context, wo *elk.WorkOrder) error {
	if qr.r.opts.Hosted {
		qr.hostedDeadline = time.Now().Add(qr.r.opts.Config.Hosted.RunCap())
	}
	var fetchSecrets map[string]string
	if qr.q.AgentKind == checkadapter.Kind {
		if c, err := checkadapter.ParsePacket(wo.Text); err == nil {
			fetchSecrets = c.Fetch.Secrets()
		}
	}
	qr.redact.Store(secretenv.NewRedactor(qr.withOwnToken(fetchSecrets)))
	qr.replyOpened = time.Time{} // a window is one run's
	qr.openLog(wo)
	// Cleanup is a stack rather than defers so a review wait can take it with
	// it (review_detach.go). The serve loop then claims the queue's next run
	// while this one is still with Elk, and the worktree stays until the
	// review — and any revision — is actually finished.
	clean := &cleanupStack{}
	st := &driveState{cur: qr}
	defer func() {
		if !st.detached {
			clean.run()
		}
	}()
	clean.add(func() { st.cur.closeLog() })
	// From the claim to the end of the run: keep the machine from idling to
	// sleep, and renew the lease the moment it wakes (sleep.go, ark:rein#90).
	clean.add(qr.holdAcrossSleep(ctx, wo.RunID))
	if qr.r.opts.Hosted {
		qr.applySecrets(hosted.Values())
	}

	a, err := adapter.Get(qr.q.AgentKind)
	if err != nil {
		qr.setPreflightState("No adapter for this queue")
		return qr.stuck(ctx, wo, "No adapter for this queue", err.Error()+
			"\n\nThis machine's Rein build cannot drive that agent kind. Either install a build that can, "+
			"or point the queue at a kind this one has.")
	}
	manifest := a.Manifest()

	// The packet's required_capabilities span two namespaces, and each name
	// goes to the list that can actually answer it. Only the runtime half may
	// reach RunSpec.RequiredCapabilities: adapters re-check that field inside
	// Start, so putting an environment name there would reintroduce the same
	// refusal one layer down, after the worktree exists.
	runtimeNames, hostNames := SplitRequirements(wo.RequiredCapabilities)

	// What the Wrangler opt-in adds — the rule, the owner's connector, and the
	// `pm` the queue declares for them — belongs to one run at a time, and
	// only to a Wrangler cycle: a packet that requires `pm` (wrangler.go,
	// ark:rein#50). Any other run on a Wrangler queue sees the queue exactly
	// as it would be without the opt-in: its `mcp:elk` is the queue's own run
	// connector where the adapter takes one (ark:rein#112), and otherwise
	// missing rather than quietly answered by the owner's connector.
	cycle := wranglerCycle(qr.q, wo.RequiredCapabilities)
	host, mcps := qr.host, qr.mcp.declared
	if qr.q.IsWrangler() {
		if cycle {
			qr.log.Runner(runlog.KindNote, "Wrangler cycle: the Wrangler rule and the owner's Elk connector apply")
			qr.logf("run %s: Wrangler cycle; the Wrangler rule and the owner's Elk connector apply", wo.RunID)
		} else {
			// A Wrangler queue may configure no mcp_servers of its own
			// (config.Validate), so its ordinary allow-list is empty.
			host = host.withoutProvenance(wranglerOptIn)
			if !qr.mcp.connector {
				mcps = nil
			}
			qr.log.Runner(runlog.KindNote, "not a Wrangler cycle (no pm requirement): ordinary prompt, no owner connector")
			qr.logf("run %s: not a Wrangler cycle; ordinary prompt and no owner connector", wo.RunID)
		}
	}

	// A hosted run has exactly its selected allowlisted repository. Local
	// checkout probes cannot establish this capability before the clone exists.
	if qr.r.opts.Hosted {
		target, err := ResolveHostedRepo(qr.r.opts.Config, qr.q, wo.Text)
		if err != nil {
			return qr.stuck(ctx, wo, "Rein could not tell which repository this run is in", err.Error())
		}
		host = host.withExtra("hosted target repository", "repo:"+target.Name)
		var names []string
		for name := range hosted.Values() {
			names = append(names, name)
		}
		host = host.withExtra("hosted environment", names...)
	}

	advisory := AdvisoryRequirements(hostNames, host)
	for _, name := range advisory {
		qr.log.Runner(runlog.KindNote, "Unknown environment capability %q is advisory; proceeding", name)
		qr.logf("run %s: unknown environment capability %q is advisory; proceeding", wo.RunID, name)
	}

	// What Elk last set for the queue wins over config.toml, which wins over
	// the CLI's own default (modelchoice.go, ark:rein#106).
	choice := qr.runModelChoice()
	// A model a person chose for this run alone wins over the queue's.
	if m := wo.Model; m != "" {
		if config.ValidModel(m) {
			choice.Model, choice.ModelFrom = m, FromRun
		} else {
			qr.logf("run %s: not using the run's model %q: it is not a model id", wo.RunID, m)
		}
	}
	if choice.ModelFrom == FromElk || choice.ModelFrom == FromRun || choice.EffortFrom == FromElk {
		qr.log.Runner(runlog.KindNote, "%s", choice.String())
	}

	spec := adapter.RunSpec{
		RunID:                wo.RunID,
		Hosted:               qr.r.opts.Hosted,
		AgentKind:            qr.q.AgentKind,
		Prompt:               wo.Text,
		SystemPrompt:         runSystemPrompt(qr.q, cycle),
		PermissionMode:       qr.mode,
		Model:                choice.Model,
		Effort:               choice.Effort,
		RequiredCapabilities: runtimeNames,
		Timeouts:             adapter.Timeouts{Idle: qr.r.opts.StallAfter},
		Queue:                qr.q.Name,
		Settings:             qr.q.Settings,
		PersonalFolders:      qr.q.PersonalFolders,
		// The queue's allow-list, and nothing else: each adapter keeps the
		// developer's own servers out (ark:rein#21, #22), so these are the
		// only MCP servers the session sees (mcp.go, ark:rein#40). It holds
		// the Elk run connector, on the queue's own token, where the adapter
		// can take one (ark:rein#112).
		MCPServers: qr.mcpServersForRun(),
	}
	// The run connector is the token Rein claims and reports with, so Rein's
	// own tools on it are kept from the session (adapter.RunConnectorDeniedTools).
	if qr.mcp.connector && !cycle {
		spec.DeniedTools = append(spec.DeniedTools, qr.mcp.denied...)
	}

	if qr.r.opts.Hosted {
		spec.Timeouts.Total = qr.r.opts.Config.Hosted.RunCap()
	}
	if cycle {
		spec.MCPServers = nil
		spec.WranglerConnectorAccount = qr.q.WranglerAccount()
		if spec.WranglerConnectorAccount == "" {
			spec.WranglerConnectorAccount = qr.workspace + "/" + qr.q.Name
		}
	}

	// Fail closed, before anything exists on disk. CheckSupport does not read
	// WorktreeDir, which is what lets it run here — ahead of `git worktree
	// add`, so a run that cannot be served produces no partial work, no orphan
	// worktree and nothing to reap.
	if err := adapter.CheckSupport(manifest, spec); err != nil {
		qr.setPreflightState("The agent cannot do something this run requires")
		return qr.stuck(ctx, wo, "The agent cannot do something this run requires", err.Error()+
			"\nPacket requirements: "+strings.Join(wo.RequiredCapabilities, ", ")+
			"\n\n"+capabilityHelp(manifest, host)+
			"\nNo work was started and no worktree was created.")
	}
	if missing := MissingQueueRequirements(hostNames, manifest, host, mcps); len(missing) > 0 {
		qr.setPreflightState("This machine does not hold a capability this run requires")
		return qr.stuck(ctx, wo, "This machine does not hold a capability this run requires",
			"Missing from this machine's environment: **"+strings.Join(missing, ", ")+"**\n\n"+
				qr.wranglerOnlyNote(cycle, missing)+
				capabilityHelp(manifest, host)+
				"\nIf this machine does hold "+quoteList(missing)+", declare it in this machine's\n"+
				"`~/.rein/config.toml` — Rein detects what it can, and the config is how you tell it\n"+
				"the rest:\n\n```toml\ncapabilities = ["+quoteList(missing)+"]\n```\n\n"+
				"or for this queue only:\n\n```toml\n[[queues]]\nname = \""+qr.q.Name+"\"\n"+
				"capabilities = ["+quoteList(missing)+"]\n```\n\n"+
				"No work was started and no worktree was created.")
	}
	var preflightErr error
	var prepared adapter.Prepared
	if p, ok := a.(adapter.RunPreparer); ok {
		// An adapter whose packet names something the worktree must honour —
		// the check adapter's commit — reads it here, before anything exists
		// on disk, in place of its preflight.
		prepared, preflightErr = p.Prepare(ctx, spec)
	} else if qr.r.opts.Hosted {
		spec.Env = hosted.Values()
		preflightErr = spec.CheckPlanEnv()
		if preflightErr == nil {
			if p, ok := a.(interface{ PreflightHosted(context.Context) error }); ok {
				preflightErr = p.PreflightHosted(ctx)
			}
		}
	} else {
		preflightErr = a.Preflight(ctx)
	}
	if err := preflightErr; err != nil && errors.Is(err, adapter.ErrWorkOrder) {
		qr.setPreflightState("ready")
		return qr.stuck(ctx, wo, "The work order cannot be run as written", err.Error()+
			"\n\nNothing was started and no worktree was created.")
	}
	if err := preflightErr; err != nil {
		qr.setPreflightState(agentSetupFailure(err))
		title := "The agent is not ready on this machine"
		if errors.Is(err, adapter.ErrNotAPIAuth) {
			title = notAPITitle
		}
		code, _ := stopCode(err)
		if code == "" {
			code = "other"
		}
		return qr.stuckCode(ctx, wo, title, err.Error()+
			"\n\nThis is a local problem on the machine serving queue "+qr.q.Name+
			" — a missing binary or an expired vendor login. Nothing was started.", code)
	}

	qr.setPreflightState("ready")

	// A Google key the person stored for Rein, for a gemini queue and no
	// other (gemini.go). Optional: with none, the program runs on its own
	// setup. Read fresh for each run and put in every redactor below before
	// the agent can say anything. It travels in ProviderKey, never Env, so no
	// other adapter and no task process can be handed it.
	var providerKey map[string]string
	if qr.q.AgentKind == geminiadapter.Kind && !qr.r.opts.Hosted {
		if key := qr.storedGeminiKey(); key != "" {
			spec.ProviderKey = key
			providerKey = map[string]string{geminiadapter.KeyName: key}
		}
	}

	// A scoped queue's secrets, read fresh for this run so a rotated item
	// takes effect without a restart, and before the worktree so a missing
	// one leaves nothing behind (secrets.go, ark:rein#48).
	if qr.r.opts.Hosted {
		spec.Env = hosted.Values()
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		for k, v := range hosted.GitEnv(executable) {
			spec.Env[k] = v
		}
		spec.Env["GH_TOKEN"] = spec.Env["REIN_GITHUB_TOKEN"]
		// The queue's token is Rein's alone, read once at start-up: a session
		// holding it could skip the run connector's proxy (ark:rein#112).
		delete(spec.Env, "REIN_ELK_TOKEN")
	} else if qr.q.Scoped() {
		env, err := qr.resolveSecrets()
		if err != nil {
			qr.log.Runner(runlog.KindNote, "a scoped secret could not be read; nothing started")
			return qr.stuck(ctx, wo, "A secret this queue needs is not on this machine",
				err.Error()+"\n\n"+qr.secretsHelp())
		}
		spec.Env = env
		combined := map[string]string{}
		for k, v := range env {
			combined[k] = v
		}
		for k, v := range fetchSecrets {
			combined[k] = v
		}
		for k, v := range providerKey {
			combined[k] = v
		}
		qr.applySecrets(qr.withOwnToken(combined))
	} else {
		// Every run is scoped; a queue without a secrets map keeps, beyond
		// the system variables, only what it names (env.go).
		spec.PassEnv = qr.passEnv(spec.MCPServers != nil)
		if len(providerKey) > 0 {
			values := map[string]string{}
			for k, v := range fetchSecrets {
				values[k] = v
			}
			for k, v := range providerKey {
				values[k] = v
			}
			red := secretenv.NewRedactor(qr.withOwnToken(values))
			qr.redact.Store(red)
			qr.log.SetRedactor(red)
		}
	}
	// The run connector: a loopback proxy that holds the queue's token for
	// this run and refuses Rein's own tools (elkproxy.go, ark:rein#112). The
	// session gets only its address.
	if qr.mcp.connector && !cycle {
		proxy, err := qr.startRunConnector(&spec)
		if err != nil {
			qr.log.Runner(runlog.KindNote, "the Elk connector for this run could not start; nothing started")
			return qr.stuck(ctx, wo, "Rein could not start this run's Elk connector", err.Error()+
				"\n\nNo work was started and no worktree was created.")
		}
		clean.add(proxy.Close)
	}
	// The capture gate first, then Chrome, so `chrome` stays at the front of
	// PATH and the gate stays ahead of /usr/sbin/screencapture.
	clean.add(qr.giveCapture(&spec))
	clean.add(qr.giveChrome(&spec))

	var repo RepoResolution
	var wt *worktree.Worktree
	if qr.r.opts.Hosted {
		repo, err = ResolveHostedRepo(qr.r.opts.Config, qr.q, wo.Text)
	} else {
		repo, err = ResolveRepo(qr.r.opts.Config, qr.q, wo.Text)
	}
	if err != nil {
		var unresolved *UnresolvedRepoError
		if !qr.r.opts.Hosted && errors.As(err, &unresolved) && unresolved.Name == "" &&
			QueueHasNoCode(qr.r.opts.Config, qr.q) {
			return qr.driveWithoutRepo(ctx, a, spec, wo, cycle, host, advisory, clean, st)
		}
		qr.setPreflightState("Rein could not tell which repository this run is in")
		return qr.stuck(ctx, wo, "Rein could not tell which repository this run is in", err.Error())
	}
	qr.logf("run %s: repository %s", wo.RunID, repo)
	if qr.r.opts.Hosted {
		clone := qr.r.opts.HostedClone
		if clone == nil {
			root := qr.r.opts.Config.WorkDir
			if root == "" {
				home, e := config.Dir()
				if e != nil {
					return e
				}
				root = filepath.Join(home, "work")
			}
			clone = (&worktree.Manager{Root: root}).Clone
		}
		cloneCtx, cancel := context.WithDeadline(ctx, qr.hostedDeadline)
		wt, err = clone(cloneCtx, repo.Name, wo.RunID, spec.Env)
		cancel()
		if err != nil && !time.Now().Before(qr.hostedDeadline) {
			return qr.submitStuck(ctx, wo, "The run hit the hosted run cap", fmt.Sprintf("hit the hosted run cap of %s", qr.r.opts.Config.Hosted.RunCap()), &elk.Usage{})
		}
		if wt != nil {
			repo.Path = wt.Dir
		}
	} else {
		wt, err = qr.r.opts.Worktrees.Create(ctx, worktree.Request{Repo: repo.Path, RunID: wo.RunID,
			BaseRef: qr.baseRefFor(repo), Commit: prepared.Commit, Fetch: prepared.Fetch})
	}

	if err != nil {
		title := "Could not create a worktree"
		code, line := stopCode(err)
		if line != "" {
			title = truncate(line, 200)
		}
		qr.setPreflightState(title)
		return qr.stuckCode(ctx, wo, title, err.Error()+
			"\n\nRepository: "+repo.String(), code)
	}
	// Added like defers, so they run in reverse: the adapter tidies up, Ark
	// is finished, then the worktree is reaped.
	clean.add(func() {
		if !st.cur.keepReview {
			st.cur.reap(wt)
		}
	})
	clean.add(func() {
		if !st.cur.keepReview {
			st.cur.finishArkRuns(ctx, wt)
		}
	})
	clean.add(func() {
		if !st.cur.keepReview {
			st.cur.reapAdapter(a, wo.RunID, &spec)
		}
	})
	spec.WorktreeDir = wt.Dir
	spec.SystemPrompt = runSystemPrompt(qr.q, cycle) + "\n\n### Declared environment capabilities — preflight list\n" + strings.Join(host.Names(), ", ") + "\n\n" + deliveryInstructions(wt, wo, qr.q.LandOrDefault())
	if !qr.r.opts.Hosted {
		// Repositories the packet declares, or the direction's workflow
		// reaches, beyond this one (reach.go, ark:rein#85).
		if reach := resolveReach(ctx, qr.r.opts.Config, qr.q, wo, repo.Path); !reach.empty() {
			spec.SystemPrompt = applyReach(spec.SystemPrompt, reach)
			qr.logf("run %s: repositories in reach: %s", wo.RunID, reach.summary())
		}
	}
	if qr.r.opts.Hosted {
		var names []string
		for _, name := range hosted.Names {
			if spec.Env[name] != "" {
				names = append(names, name)
			}
		}
		spec.SystemPrompt = scopedSecretsPromptNames(spec.SystemPrompt, names)
		spec.SystemPrompt = strings.ReplaceAll(spec.SystemPrompt, "from this machine's keychain", "from the hosted environment")
	}
	if len(advisory) > 0 {
		spec.SystemPrompt += "\n\nPreflight treated these unknown packet requirements as advisory: " + strings.Join(advisory, ", ") + ". Proceed; these names do not block this run."
	}

	qr.logf("run %s: worktree %s on branch %s from %s (%s)",
		wo.RunID, wt.Dir, wt.Branch, wt.BaseRef, shortSHA(wt.BaseSHA))
	qr.recordWorktree(wt, repo)
	if wt.Ark.Wanted && !wt.Ark.OK {
		qr.logf("run %s: %s", wo.RunID, wt.Ark.Note)
	}

	return qr.driveWithReview(ctx, a, spec, wo, wt, repo, clean, st)
}

// driveWithoutRepo runs a task that needs no code on a worker that has none,
// in an empty folder that is removed afterwards (ark:rein#103). There is no
// branch, nothing to push and nothing to land, and the agent is told so.
func (qr *queueRunner) driveWithoutRepo(ctx context.Context, a adapter.Adapter, spec adapter.RunSpec,
	wo *elk.WorkOrder, cycle bool, host *HostCapabilities, advisory []string,
	clean *cleanupStack, st *driveState) error {
	dir, err := os.MkdirTemp("", "rein-run-")
	if err != nil {
		return qr.stuck(ctx, wo, "Could not make a folder for this run", err.Error())
	}
	if !qr.r.opts.KeepWorktrees {
		clean.add(func() {
			if !st.cur.keepReview {
				_ = os.RemoveAll(dir)
			}
		})
	}
	clean.add(func() {
		if !st.cur.keepReview {
			st.cur.reapAdapter(a, wo.RunID, &spec)
		}
	})
	spec.WorktreeDir = dir
	spec.SystemPrompt = runSystemPrompt(qr.q, cycle) +
		"\n\n### Declared environment capabilities — preflight list\n" + strings.Join(host.Names(), ", ") +
		"\n\n" + noRepoInstructions
	if len(advisory) > 0 {
		spec.SystemPrompt += "\n\nPreflight treated these unknown packet requirements as advisory: " + strings.Join(advisory, ", ") + ". Proceed; these names do not block this run."
	}
	qr.logf("run %s: no repository (this worker has none and the task names none); running in %s", wo.RunID, dir)
	return qr.driveWithReview(ctx, a, spec, wo, nil, RepoResolution{Path: dir, Source: RepoNone}, clean, st)
}

// noRepoInstructions is the "This run" section for a run with no repository.
const noRepoInstructions = "## This run\n\n" +
	"- No repository: this worker has none and the task names none. You are in an empty folder that is removed afterwards.\n" +
	"- Do the task and put the answer in your final message. There is nothing to commit, push, open or merge.\n" +
	"- If the task turns out to need code, say which repository it needs and stop: that is a stuck result, not a guess.\n"

// reviewOutcome is how a wait on Elk's auto-review pass ended.
type reviewOutcome int

const (
	// reviewSettled — nothing more to do: the review approved it, somebody
	// settled the run, or the wait timed out. An Elk that says "it is with the
	// person for review now" is heard as an approval at once
	// ([elk.NotRunningError.ReviewPassed]); without that sentence approval and
	// "the pass has not run yet" are indistinguishable through the poll, which
	// is why a timeout is still a settlement rather than a failure.
	reviewSettled reviewOutcome = iota
	// reviewRevisions — Elk wants changes and has reopened the run.
	reviewRevisions
	// reviewCancelled — a person cancelled it while the review was thinking.
	reviewCancelled
	// reviewInterrupted — Rein itself is stopping, so nobody knows what the
	// review would have said (ark:rein#81).
	reviewInterrupted
)

// driveWithReview runs the agent, submits, and then — when Elk has parked the
// run for its own review pass — stays to see the review through.
//
// Leaving at the submit is what the third dogfood found: Elk's pass posts a
// revision request and REOPENS the run to `running` expecting the agent to
// still be there, so a runner that exits strands the run with nobody driving
// it. `review_run` then refuses it, because there is no ready, unresolved run
// to review.
//
// Hosted mode is the exception, and leaves at the submit on purpose
// (ark:rein#78): a billed container is the wrong place to wait, and a revision
// there is taken by a fresh attempt rather than this process.
//
// On a machine, the wait does not stay on this goroutine (ark:rein#65). The
// serve loop is what claims the queue's next run, and it used to sit here for
// up to the review timeout — slot already given back, queue still unable to
// claim, Wrangler cycle included. The wait continues beside the loop.
func (qr *queueRunner) driveWithReview(ctx context.Context, a adapter.Adapter, spec adapter.RunSpec,
	wo *elk.WorkOrder, wt *worktree.Worktree, repo RepoResolution, clean *cleanupStack, st *driveState) error {

	var pending elk.Usage

	if _, ok := spec.MCPServers[adapter.RunConnectorName]; ok && qr.mcp.connector {
		spec.SystemPrompt += "\n\n" + runConnectorNote
	}

	// Report before spending a single token. The runbook asks for a report
	// immediately after claiming, and it doubles as the cheapest possible
	// check that the claim is still live: a run cancelled between the claim
	// and here costs nothing instead of a whole session.
	// A tool line, not a step: this is Rein's note about the machine, and a step
	// is what a person reads on their Lock Screen as what the agent is doing.
	if err := qr.report(ctx, wo.RunID, elk.ProgressTool, qr.openingReport(repo, wt), "", "", &pending); err != nil {
		if errors.Is(err, elk.ErrCancelled) {
			qr.logf("run %s: cancelled before the session started", wo.RunID)
			qr.logStatus = runlog.StatusCancelled
			qr.log.Runner(runlog.KindNote, "cancelled before the session started")
			return nil
		}
		qr.logf("run %s: %v", wo.RunID, err)
	}

	// sentBack is the deliverable this round handed in, empty in round 1.
	// rec is what the session did, for a deliverable Rein has to write.
	// Later rounds, after Elk asks for revisions, run beside this goroutine
	// (review_detach.go).
	var (
		sentBack string
		rec      runRecord
	)
	const round = 1
	// A reopened revision is running again; a failed attempt must not
	// inherit the earlier round's successful submission.
	qr.logStatus = runlog.StatusInterrupted
	res, err := qr.runSession(ctx, a, spec, wo, wt, repo, &pending)
	if err != nil || res.done {
		return err
	}
	spec.Model = res.model
	rec.add(res.out)

	// Never hand in a blank, a one-liner, or a promise (ark:rein#83).
	res, err = qr.secureDeliverable(ctx, a, spec, wo, wt, repo, res, "", sentBack, &rec, &pending)
	if err != nil || res.done {
		return err
	}

	sub, err := qr.submitWithProof(ctx, a, spec, wo, wt, repo, &res, &rec, &pending)
	if err != nil || sub == nil || !sub.ReviewPending {
		return err
	}
	sentBack = agentAccount(res.out)

	if qr.r.opts.Hosted {
		// A hosted run does not wait for the review (ark:rein#78). Its
		// container is billed by the minute and nobody is working while
		// Elk reviews: on the hosted proof the wait was over 90% of the
		// run's compute. So it exits here. If the review asks for
		// revisions the run reopens, and taking them is a fresh attempt
		// in a fresh container — the session file died with this one, so
		// that attempt starts from the work order and the verdict, not a
		// resume (hosted-rein-queue design, "Idle").
		qr.logf("run %s: submitted; Elk is reviewing it. Hosted, so Rein exits now rather than wait in a "+
			"billed container; a revision is taken by a fresh run", wo.RunID)
		qr.log.Runner(runlog.KindReview, "submitted; hosted, so not waiting for Elk's review — a revision is taken by a fresh run")
		return nil
	}

	qr.logf("run %s: submitted; Elk is reviewing it. Waiting up to %s (round %d), with the slot given back",
		wo.RunID, qr.reviewTimeout(), round)
	qr.log.Runner(runlog.KindReview, "submitted; waiting up to %s for Elk's review (round %d), with the slot given back",
		qr.reviewTimeout(), round)
	// No agent is running while Elk reviews — the session has ended and
	// a revision resumes it — so the slot goes back for the wait and is
	// taken again only if there is more to do (ark:rein#60). Holding it
	// made every reviewed run on a one-slot machine twenty minutes of
	// nobody working, Wrangler cycle included.
	//
	// Giving the slot back is not enough for this queue's own next claim:
	// that claim happens on this goroutine, which is the serve loop. The
	// wait continues beside it (ark:rein#65).
	qr.slot.release()
	qr.detachReview(ctx, reviewCont{
		round:     round,
		a:         a,
		spec:      spec,
		wo:        wo,
		wt:        wt,
		repo:      repo,
		pending:   &pending,
		sentBack:  sentBack,
		rec:       rec,
		sessionID: res.sessionID,
		clean:     clean,
	}, st)
	return nil
}

// retakeSlot gets a slot back for a run Elk's review has reopened, before the
// agent starts again. The run is `running` under this machine's claim while it
// waits, so its lease is kept and the person is told why nothing is happening
// yet: a report on the run itself, and then one every [MaxReportInterval].
//
// stop is true when the run must go no further — a person cancelled it while
// it waited, or Rein is shutting down — and err is then what drive returns.
// Neither submits anything: a cancelled run is not Rein's to write over, and a
// shutdown leaves the claim to lapse, as Ctrl-C does mid-session.
func (qr *queueRunner) retakeSlot(ctx context.Context, wo *elk.WorkOrder, round int, pending *elk.Usage) (stop bool, err error) {
	if qr.slot != nil && !qr.slot.isReleased() {
		return false, nil
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		reported  time.Time
		cancelled bool
	)
	hold, ok := qr.r.gate.enter(waitCtx, slotRequest{queue: qr.r.opts.Config.QueueLabel(qr.q), prio: prioRevision}, func(reason string) {
		if qr.setWaiting(reason) && reported.IsZero() {
			qr.logf("run %s: %s", wo.RunID, reason)
		}
		if !reported.IsZero() && time.Since(reported) < MaxReportInterval {
			return
		}
		reported = time.Now()
		body := fmt.Sprintf("Elk's review asked for revisions (round %d of %d). Rein is %s, and starts on them "+
			"as soon as one is free.", round, qr.maxReviewRounds(), reason)
		if err := qr.report(waitCtx, wo.RunID, elk.ProgressStep, body, "", "", pending); errors.Is(err, elk.ErrCancelled) {
			cancelled = true
			cancel()
		}
	})
	qr.setWaiting("")
	switch {
	case cancelled:
		qr.logf("run %s: cancelled while waiting for a slot to work Elk's revisions — nothing submitted", wo.RunID)
		qr.logStatus = runlog.StatusCancelled
		qr.log.Runner(runlog.KindReview, "cancelled while waiting for a slot to work Elk's revisions")
		return true, nil
	case !ok:
		return true, nil
	}
	qr.slot = hold
	hold.setRun(wo.RunID)
	if hold.waited >= time.Minute {
		qr.logf("run %s: a slot is free after %s waiting", wo.RunID, span(hold.waited))
	}
	return false, nil
}

// awaitReview polls until Elk's review pass says something, or until it has
// been quiet long enough to call it approved.
//
// The poll is `ask_elk` with no question, which is also a heartbeat: it runs
// the same guarded touch `report_progress` does, so a run reopened to
// `running` keeps its lease alive while Rein works out what to do with it.
//
// The trap it exists to avoid: while the run sits at `ready` waiting for the
// pass, `ask_elk` answers "Not recorded: This run is `ready`, not running." —
// plain text, no isError, the same shape a CANCELLED run produces. Reading
// that as a cancellation would discard finished work on every reviewed run.
// [elk.NotRunningError.AwaitingReview] is the discriminator.
func (qr *queueRunner) awaitReview(ctx context.Context, wo *elk.WorkOrder) (string, reviewOutcome) {
	deadline := time.Now().Add(qr.reviewTimeout())
	poll := qr.reviewPollInterval()
	for {
		select {
		case <-ctx.Done():
			// Shutting down. The run is `ready` in Elk, which is a legitimate
			// place to leave it — better than claiming an outcome, and the
			// caller says which this was (ark:rein#81).
			return "", reviewInterrupted
		case <-time.After(poll):
		}

		res, err := qr.elk.AskElk(ctx, wo.RunID, "")
		if err == nil {
			// Elk has advanced its cursor past these: they ride along with
			// the revisions after Elk's verdict, as people's direction rather
			// than part of the review (revisionSpec), or the run's end logs
			// them (eventlog.go).
			qr.holdComments(res.Text)
		}
		switch {
		case err == nil && res.Revisions != "":
			return res.Revisions, reviewRevisions
		case err == nil:
			// An answer, a delivered comment, "no question asked yet" — no
			// verdict either way. Keep waiting.
		default:
			var nr *elk.NotRunningError
			switch {
			case !errors.As(err, &nr):
				qr.logf("run %s: polling Elk's review: %v", wo.RunID, err)
			case nr.Cancelled:
				return "", reviewCancelled
			case nr.ReviewPassed():
				// `ready`, and Elk says the pass is done and the run is with
				// the person: an approval, heard on the next poll instead of
				// at the timeout (ark:rein#78).
				return "", reviewSettled
			case nr.AwaitingReview():
				// `ready`: the pass has not reported yet. The one "not
				// running" that does not mean stop.
			default:
				qr.logf("run %s: the review wait ended — the run is %s", wo.RunID, nr.Status)
				return "", reviewSettled
			}
		}
		if time.Now().After(deadline) {
			return "", reviewSettled
		}
	}
}

// revisionSpec turns Elk's verdict into the next session.
//
// Resuming is much better than starting over — the agent still has the work in
// its head, and a cold start would redo everything to change one thing. An
// adapter that does not declare `resume` gets the original work order back
// alongside the verdict instead, because otherwise it has no idea what it was
// doing.
func (qr *queueRunner) revisionSpec(a adapter.Adapter, spec adapter.RunSpec,
	wo *elk.WorkOrder, sessionID, verdict string) adapter.RunSpec {

	// Comments a person posted meanwhile follow the review under their own
	// header (formatCommentDelivery's), so they do not read as its blockers.
	comments := ""
	if c := qr.takeComments(); c != "" {
		comments = "\n\n" + c
	}
	directive := "Elk reviewed your deliverable and will not show it to the user until this is " +
		"addressed:\n\n" + blockquote(verdict) + comments + "\n\nMake those changes in this same worktree. Do not " +
		"start the task over — only what is asked for above is missing. Do not end your turn while anything " +
		"is still running in the background. Your final message replaces your previous deliverable " +
		"entirely, so end with the whole deliverable: first, in plain English, what was done and what is " +
		"left, answering the review above; then links to the pull requests and commits."

	next := spec
	next.ResumeID = ""
	if a.Manifest().Has(adapter.CapResume) && sessionID != "" {
		next.ResumeID = sessionID
		next.Prompt = directive
		return next
	}
	next.Prompt = wo.Text + "\n\n---\n\n" + directive
	return next
}

func (qr *queueRunner) reviewTimeout() time.Duration {
	if d := qr.r.opts.ReviewTimeout; d > 0 {
		return d
	}
	return DefaultReviewTimeout
}

func (qr *queueRunner) reviewPollInterval() time.Duration {
	if d := qr.r.opts.ReviewPollInterval; d > 0 {
		return d
	}
	return DefaultReviewPollInterval
}

func (qr *queueRunner) maxReviewRounds() int {
	if n := qr.r.opts.MaxReviewRounds; n > 0 {
		return n
	}
	return DefaultMaxReviewRounds
}

// blockquote indents text as markdown so a verdict inside a deliverable or a
// prompt reads as a quotation rather than as more instructions.
func blockquote(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// reap removes the worktree unless the operator asked to keep it. It runs on
// every exit from drive, including a cancellation, because a discarded run
// leaves exactly as much rubbish behind as a finished one.
func (qr *queueRunner) reap(wt *worktree.Worktree) {
	if qr.r.opts.KeepWorktrees {
		qr.logf("keeping worktree %s (--keep-worktrees)", wt.Dir)
		qr.log.Runner(runlog.KindReaped, "worktree kept at %s (--keep-worktrees)", wt.Dir)
		return
	}
	// A fresh context: the run's own may already be cancelled, and the
	// worktree still has to go.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if qr.r.opts.Hosted {
		if err := os.RemoveAll(wt.Dir); err != nil {
			qr.logf("reaping hosted clone: %v", err)
		}
		qr.log.Runner(runlog.KindReaped, "worktree %s removed; branch %s was not", wt.Dir, wt.Branch)
		return
	}
	if err := qr.r.opts.Worktrees.Reap(ctx, wt); err != nil {
		qr.logf("reaping %s: %v", wt.Dir, err)
		qr.log.Runner(runlog.KindReaped, "worktree %s NOT removed: %v", wt.Dir, err)
		if errors.Is(err, worktree.ErrInUse) {
			// Not for ever: the run's meta says a reap is owed, and the
			// runner tries again until the processes are gone (ark:rein#99).
			qr.log.Update(func(m *runlog.Meta) { m.ReapPending = true })
		}
		return
	}
	qr.log.Runner(runlog.KindReaped, "worktree %s removed; branch %s deleted", wt.Dir, wt.Branch)
}

// runSession drives the agent, with Loom's watchdog ladder around it: no
// events for [Options.StallAfter] → one bounded restart with `--resume` if the
// adapter declares it → `stuck`.
//
// The restart is bounded at one on purpose. A stalled agent that stalls again
// after a resume is not going to be rescued by a third attempt; it is going to
// burn the lease and the tokens. Loom's own ladder ends in quarantine for the
// same reason.
func (qr *queueRunner) runSession(ctx context.Context, a adapter.Adapter, spec adapter.RunSpec,
	wo *elk.WorkOrder, wt *worktree.Worktree, repo RepoResolution,
	pending *elk.Usage) (sessionResult, error) {

	restarted := false

	for {
		if qr.r.opts.Hosted {
			spec.Timeouts.Total = time.Until(qr.hostedDeadline)
			if spec.Timeouts.Total < 90*time.Second {
				spec.Timeouts.Startup = spec.Timeouts.Total
			}
			if spec.Timeouts.Total <= 0 {
				return sessionResult{done: true}, qr.submitStuck(ctx, wo, "The run hit the hosted run cap", fmt.Sprintf("hit the hosted run cap of %s", qr.r.opts.Config.Hosted.RunCap()), pending)
			}
		}
		sess, err := a.Start(ctx, spec)
		if err != nil && qr.r.opts.Hosted && !time.Now().Before(qr.hostedDeadline) {
			return sessionResult{done: true}, qr.submitStuck(ctx, wo, "The run hit the hosted run cap", fmt.Sprintf("hit the hosted run cap of %s", qr.r.opts.Config.Hosted.RunCap()), pending)
		}
		if err != nil {
			// The reason goes to both local logs as well as to Elk. Until
			// ark:rein#38 only the title did, so the one machine that could
			// be inspected said "would not start" and nothing about why.
			qr.logf("run %s: %s would not start: %v", wo.RunID, a.Name(), err)
			qr.log.Runner(runlog.KindSession, "%s would not start: %v", a.Name(), err)
			title := "The agent would not start"
			if errors.Is(err, adapter.ErrNotPlanAuth) {
				title = notPlanTitle
			}
			if errors.Is(err, adapter.ErrNotAPIAuth) {
				title = notAPITitle
			}
			return sessionResult{done: true}, qr.stuck(ctx, wo, title,
				stuckBody(err.Error(), qr.runDetails(wt, repo, spec, ""), "", ""))
		}
		if spec.ResumeID != "" {
			qr.log.Runner(runlog.KindSession, "restarted %s, resuming session %s", a.Name(), spec.ResumeID)
		} else {
			qr.log.Runner(runlog.KindSession, "started %s in %s, permission mode %s",
				a.Name(), spec.WorktreeDir, spec.PermissionMode)
		}
		if id := sess.ID(); id != "" {
			qr.log.Update(func(m *runlog.Meta) { m.SessionID = id })
		}
		// Registered for the life of the session, so `rein attach` has
		// something to take over. Unregistered before the outcome is decided:
		// a session that has ended cannot be driven by anybody.
		asks := &sessionAsk{client: qr.elk, runID: wo.RunID}
		qr.live = &liveSession{
			runID: wo.RunID, queue: qr.r.opts.Config.QueueLabel(qr.q), agentKind: a.Name(),
			direction: firstLine(wo.Direction), started: time.Now(),
			sess: sess, manifest: a.Manifest(), log: qr.log,
			logf:      qr.logf,
			onRelease: qr.noteHuman,
			asks:      asks,
			onAttach:  func() error { return asks.retract(ctx, true) },
		}
		qr.r.sessions.add(qr.live)
		out := qr.watch(ctx, sess, wo, spec, pending)
		qr.r.sessions.remove(wo.RunID)
		qr.live = nil
		// What the session last said about the subscription outlives it: it
		// is carried into the idle beats that follow, and if it said the
		// window is spent, the queue stops claiming (subscription.go).
		qr.absorbSession(sess)

		switch {
		case out.cancelled:
			// Elk told us mid-run. Stop the session and submit nothing: a
			// deliverable now would write over a decision a person just made.
			qr.interrupt(sess)
			qr.logf("run %s: cancelled by the user — work discarded", wo.RunID)
			qr.logStatus = runlog.StatusCancelled
			qr.log.Runner(runlog.KindNote, "cancelled by the user — work discarded")
			return sessionResult{done: true}, nil

		case out.switchModel != "":
			// Like the stall restart, but a person asked for it, so it does
			// not use up the one the watchdog has.
			qr.interrupt(sess)
			m := out.switchModel
			spec.ResumeID, spec.Model = sess.ID(), m
			spec.Prompt = "The person switched you to " + m + ". Carry on where you were."
			if c := qr.takeComments(); c != "" {
				spec.Prompt += "\n\n" + c
				qr.replyOpened = time.Now()
			}
			qr.logf("run %s: switching the session to %s, resuming %s", wo.RunID, m, spec.ResumeID)
			if err := qr.report(ctx, wo.RunID, elk.ProgressStep, "Switched to "+m, "", "", pending); errors.Is(err, elk.ErrCancelled) {
				qr.logStatus = runlog.StatusCancelled
				qr.log.Runner(runlog.KindNote, "cancelled by the user — work discarded")
				return sessionResult{done: true}, nil
			}
			continue

		case out.stalled:
			qr.interrupt(sess)
			canResume := a.Manifest().Has(adapter.CapResume) && sess.ID() != ""
			if !restarted && canResume {
				restarted = true
				spec.ResumeID = sess.ID()
				qr.logf("run %s: no events for %s — one restart with resume %s",
					wo.RunID, qr.r.opts.StallAfter, spec.ResumeID)
				_ = qr.report(ctx, wo.RunID, elk.ProgressStep,
					fmt.Sprintf("No output for %s. Restarting the session once, resuming where it stopped.",
						qr.r.opts.StallAfter), "", "", pending)
				continue
			}
			why := fmt.Sprintf("The agent produced no output for %s.", qr.r.opts.StallAfter)
			if restarted {
				why += " It was already restarted once with a resume, and stalled again."
			} else if !canResume {
				why += fmt.Sprintf(" The %s adapter does not declare `resume`, so there was nothing to restart into.",
					a.Name())
			}
			return sessionResult{done: true}, qr.submitStuck(ctx, wo, "The run stalled",
				stoppedBody(why, qr.runDetails(wt, repo, spec, sess.ID()), out), pending)

		case out.capped || qr.r.opts.Hosted && out.result != nil && out.result.Status == adapter.StatusTimedOut && !time.Now().Before(qr.hostedDeadline):
			qr.interrupt(sess)
			why := fmt.Sprintf("hit the hosted run cap of %s", qr.r.opts.Config.Hosted.RunCap())
			return sessionResult{done: true}, qr.submitStuck(ctx, wo, "The run hit the hosted run cap", why, pending)
		case errors.Is(out.err, adapter.ErrLoginRequired):
			qr.telMu.Lock()
			qr.preflightState = setupLogin
			qr.loginBlocked = true
			qr.telMu.Unlock()
			return sessionResult{done: true}, qr.submitStuckCode(ctx, wo, "The agent needs you to sign in again", out.err.Error(), "agent_login", pending)
		case out.err != nil && errors.Is(out.err, adapter.ErrNotAPIAuth):
			return sessionResult{done: true}, qr.submitStuck(ctx, wo, notAPITitle, out.err.Error(), pending)
		case out.err != nil && errors.Is(out.err, adapter.ErrNotPlanAuth):
			// Stopped at its start-up line, before any work: said as such,
			// and never mistaken for an exhausted subscription.
			qr.log.Runner(runlog.KindNote, "stopped: %v", out.err)
			return sessionResult{done: true}, qr.submitStuck(ctx, wo, notPlanTitle,
				stuckBody(out.err.Error(), qr.runDetails(wt, repo, spec, sess.ID()), "", ""), pending)

		case out.err != nil:
			title, why := "The agent failed", out.err.Error()
			if hold := qr.holdSentence(); hold != "" {
				// Said first, because it is the actionable part: this run
				// failed for want of a subscription, not for anything in the
				// work order, and it can be sent again — to another agent now,
				// or here after the reset.
				title, why = "The agent's subscription is exhausted", hold+"\n\n"+why
			}
			return sessionResult{done: true}, qr.submitStuck(ctx, wo, title,
				stoppedBody(why, qr.runDetails(wt, repo, spec, sess.ID()), out), pending)

		case ctx.Err() != nil:
			// The daemon is shutting down. Stop the agent and let the claim
			// lapse rather than reporting an outcome that did not happen.
			qr.interrupt(sess)
			qr.logf("run %s: interrupted by shutdown; the claim will lapse and the run returns to the queue", wo.RunID)
			return sessionResult{done: true}, ctx.Err()
		}

		return sessionResult{out: out, sessionID: sess.ID(), model: spec.Model}, nil
	}
}

// notAPITitle identifies a hosted API-key refusal.
const notAPITitle = "The agent was not on an API key, so Rein stopped it"

// notPlanTitle identifies a subscription-login refusal.
const notPlanTitle = "The agent was not on the plan login, so Rein stopped it"

// sessionResult is what one drive of the agent produced.
type sessionResult struct {
	out       watchOutcome
	sessionID string
	// model is the model the session ended on, which a person may have
	// switched (live.go): what any later session of the run starts on.
	model string
	// done means the run has already been reported terminally — a stuck
	// submit, a cancellation, a shutdown — and there is nothing further to do
	// with it.
	done bool
}

// deliveryInstructions is the run-specific half of the standing prompt: the
// concrete branch, and the Ark tasks, an agent needs in order to land its work.
//
// It goes in the SYSTEM prompt rather than the work order. The work order is
// data — its woven half is generated from captures that are not trusted input —
// so instructions carrying real values belong in the instruction channel, which
// is also the only channel the agent is told to take orders from.
//
// Filling the values in rather than describing where to find them is
// deliberate: an agent that has to go looking for its own branch name in a
// packet is one that will sometimes get it wrong.
//
// The landing policy is restated here for `pr` and `branch` (landing.go). A
// pull request, under `merge` or `pr`, closes nothing (prRule).
func deliveryInstructions(wt *worktree.Worktree, wo *elk.WorkOrder, land string) string {
	var b strings.Builder
	b.WriteString("## This run\n\n")
	fmt.Fprintf(&b, "- Repository: `%s`\n", wt.Repo)
	fmt.Fprintf(&b, "- Branch: `%s` — push it with `git push -u origin %s`\n", wt.Branch, wt.Branch)
	switch land {
	case config.LandPR:
		b.WriteString("- Landing policy: `pr` — open a non-draft pull request from this branch and stop. " +
			"Do not merge it, enable auto-merge on it, or approve it; the repository's owners merge.\n")
		b.WriteString(prRule(wo, "Report the PR URL and its head sha in your deliverable instead; Rein reports it to Elk from there. "+
			"Elk closes the item, and each Ark task it tracks that a merged pull request names, once its review accepts the run."))
	case config.LandBranch:
		b.WriteString("- Landing policy: `branch` — push this branch and stop. " +
			"Open no pull request, draft or otherwise, and merge nothing.\n")
		b.WriteString("- Commit messages must contain no Elk action id and no closing sigil. " +
			"Report the branch and its pushed head sha in your deliverable; Rein reports it to Elk from there.\n")
	default:
		b.WriteString(prRule(wo, "Report the PR URL and the merge sha in your deliverable instead. "+
			"Elk closes the item, and each Ark task it tracks that a merged pull request names, once its review accepts the run."))
	}
	return b.String()
}

// prRule is what a run's pull request body may and must say, for the two
// policies that open one. It is absolute (Elk Scout #1000, ruled (b) on
// 2026-10-17; ark:rein#76 (01M4KW06YJ)): no `Closes`, `Fixes` or `Resolves`
// for anything, Elk item or Ark task, even where the work order asks for one,
// and no Elk action id. A closing line for the Elk item is the 0138 race in
// docs/run-loop.md; one for an Ark task closes the task at the merge, before
// the review has read the work. Each Ark task the run advances is named with
// `Implements` instead, which records the landing without closing anything.
// Accepting the run marks the item done, and Elk's retire sweep (scout
// gh-connector retire.ts, migration 0429) then closes each Ark task the item
// tracks that a merged pull request names; docs/run-loop.md has the evidence.
//
// The tasks are the ones the work order lists under its Work heading, filled
// in for the same reason the branch is. Elk names them by number
// (`ark:rein#76`), and a landing is matched by ULID alone, so the line keeps a
// slot for the ULID unless Elk sent one.
func prRule(wo *elk.WorkOrder, report string) string {
	var b strings.Builder
	b.WriteString("- The pull request closes nothing: no `Closes`, `Fixes` or `Resolves` line, for an Elk item or an Ark task, " +
		"even where the work order asks for one, and no Elk action id. " + report + "\n")
	tasks := workOrderArkTasks(wo)
	if len(tasks) == 0 {
		b.WriteString("- Name each Ark task the pull request advances on a line of its own in its body, " +
			"`Implements ark:<repo>#<N> (<ULID>)`, with the task's full ULID (`ark task view <N>` prints it); " +
			"a line without one records nothing.\n")
		return b.String()
	}
	b.WriteString("- Name each Ark task the pull request advances on a line of its own in its body, " +
		"with the task's full ULID; a line without one records nothing. This run's work order lists:\n")
	for _, t := range tasks {
		fmt.Fprintf(&b, "  - `%s`\n", t.implementsLine())
	}
	b.WriteString("  Fill in a `<ULID>` or `<repo>#<N>` from `ark task view <N or ULID>`, which reads this repository's tasks, " +
		"and check the title it prints is the task's. A task whose ULID you cannot find gets no line; name it in your deliverable instead.\n")
	return b.String()
}

// arkTask is one Ark task a work order lists under its Work heading.
type arkTask struct {
	// Ref is how Elk names it, `ark:<repo>#<N>`. Empty when Elk had only the
	// task's id to show (`ark:<repository ULID>#task/<ULID>`).
	Ref string
	// ULID is the task's id, or the prefix of ten or more characters Elk
	// sent; empty when Elk sent none.
	ULID string
}

// implementsLine is the line the pull request body carries for the task,
// with a slot for whatever Elk did not say.
func (t arkTask) implementsLine() string {
	ref, ulid := t.Ref, t.ULID
	if ref == "" {
		ref = "ark:<repo>#<N>"
	}
	if ulid == "" {
		ulid = "<ULID>"
	}
	return "Implements " + ref + " (" + ulid + ")"
}

var (
	// workHeadingRE is the heading Elk's claim text puts over an item's
	// children (elk-mcp/queue.ts formatClaim: "## Work 1/3").
	workHeadingRE = regexp.MustCompile(`^## Work \d+/\d+`)
	// workArkItemRE is one Ark child under it: "2. ark:rein#76 · title ·
	// state", with an optional "(<ULID>)" after the reference, or the id-only
	// form Elk falls back to before it has synced the task's number. A
	// GitHub child (gh:…) does not match and is left to the PR's own text.
	workArkItemRE = regexp.MustCompile(`^\d+\. (?:(ark:[A-Za-z0-9._-]+#\d+)(?: \(([0-9A-Za-z]{10,26})\))?|ark:[0-9A-Za-z]{26}#task/([0-9A-Za-z]{26})) ·`)
)

// workOrderArkTasks reads the Ark tasks out of the work order's Work section:
// the first one only, ended by the next heading, and only lines in Elk's own
// shape, because this lifts text from the data channel into the instruction
// channel.
func workOrderArkTasks(wo *elk.WorkOrder) []arkTask {
	if wo == nil {
		return nil
	}
	var out []arkTask
	seen := map[arkTask]bool{}
	in := false
	for _, line := range strings.Split(wo.Text, "\n") {
		if !in {
			in = workHeadingRE.MatchString(line)
			continue
		}
		if strings.HasPrefix(line, "#") {
			break
		}
		m := workArkItemRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		t := arkTask{Ref: m[1], ULID: strings.ToUpper(m[2])}
		if m[3] != "" {
			t = arkTask{ULID: strings.ToUpper(m[3])}
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// baseRefFor picks the ref this run is cut from: a repository-specific
// override, then the queue's, then nothing — which lets internal/worktree
// resolve the remote default branch, the right answer almost everywhere.
func (qr *queueRunner) baseRefFor(repo RepoResolution) string {
	if repo.Name != "" {
		if ref, ok := qr.r.opts.Config.BaseRefs[repo.Name]; ok && ref != "" {
			return ref
		}
	}
	return qr.q.BaseRef
}

// openingReport is the first thing Elk hears about a run, and it says what the
// agent is about to be looking at: where, based on what, and whether the
// repository's work records are reachable.
//
// The Ark line is here because of how the second dogfood failed. A Codex run
// found no `.ark`, correctly refused to `ark init`, and submitted `ready`
// having changed nothing — and from Elk there was no way to see why. Whatever
// the provisioning did, it is now said out loud before the agent starts.
func (qr *queueRunner) openingReport(repo RepoResolution, wt *worktree.Worktree) string {
	var b strings.Builder
	if wt == nil {
		fmt.Fprintf(&b, "Claimed by Rein on queue %s. Starting %s in an empty folder: this worker has no code and the task names none, so there is nothing to land.\n",
			qr.q.Name, qr.q.AgentKind)
		return b.String()
	}
	fmt.Fprintf(&b, "Claimed by Rein on queue %s. Starting %s in a worktree of %s.\n",
		qr.q.Name, qr.q.AgentKind, repo.Path)
	if wt.Detached {
		fmt.Fprintf(&b, "Detached at %s (%s), no branch.\n", shortSHA(wt.BaseSHA), wt.BaseNote)
	} else {
		fmt.Fprintf(&b, "Branch `%s`, based on `%s` (%s) at %s.\n",
			wt.Branch, wt.BaseRef, wt.BaseNote, shortSHA(wt.BaseSHA))
	}
	fmt.Fprintf(&b, "Landing policy: %s.\n", landingSummary(qr.q.LandOrDefault()))
	if wt.Ark.Wanted {
		fmt.Fprintf(&b, "Ark: %s.\n", wt.Ark.Note)
	}
	if wt.Initialised {
		fmt.Fprintf(&b, "Ran `%s` in the worktree first.\n", worktree.InitScript)
	}
	// Said on the run itself, so that a person who saw it sit queued can
	// read here what it was waiting for (ark:rein#59).
	if h := qr.slot; h != nil && h.waited >= time.Minute {
		fmt.Fprintf(&b, "Waited %s for a slot on this machine (%s).\n",
			span(h.waited), strings.TrimPrefix(h.waitedOn, slotWaitPrefix))
	}
	return b.String()
}

func (qr *queueRunner) interrupt(sess adapter.Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sess.Interrupt(ctx); err != nil && !errors.Is(err, adapter.ErrSessionClosed) {
		qr.logf("interrupting the session: %v", err)
	}
	drain(sess)
}

// drain empties an event channel so an adapter blocked on delivery can finish.
// An adapter that cannot deliver an event blocks, and a run loop that stops
// reading stalls the agent it is watching — the one obligation the adapter
// contract puts on the consumer.
func drain(sess adapter.Session) {
	go func() {
		for range sess.Events() {
		}
	}()
}

// watchOutcome is what one session run produced.
type watchOutcome struct {
	emptyResult bool // no commits and no stated reason after the deliverable retry

	capped    bool
	result    *adapter.Result
	text      string // assistant text, for a deliverable an adapter did not summarise
	err       error
	cancelled bool
	stalled   bool
	// switchModel is the model a person switched the run to mid-session.
	switchModel string
	events      int
	tools       []string
	// commands and prs are the shell commands the agent ran and the pull
	// requests it named, for a deliverable Rein has to write itself.
	commands []string
	prs      []string
	// deliverable, when set, replaces the agent's account: Rein's own,
	// written because the agent would not write one (deliverable.go).
	deliverable string
}

// notePRs records the pull requests s names.
func (o *watchOutcome) notePRs(s string) {
	for _, u := range prRefs(s) {
		if !containsString(o.prs, u) {
			o.prs = append(o.prs, u)
		}
	}
}

// watch consumes a session's events and reports to Elk as it goes.
//
// Reporting is on two clocks. [MaxReportInterval] is the floor set by Elk's
// lease — a run that says nothing for fifteen minutes is handed to somebody
// else. [MinReportInterval] is the ceiling on the event-driven half, so a
// chatty agent does not turn every tool call into a round trip.
func (qr *queueRunner) watch(ctx context.Context, sess adapter.Session, wo *elk.WorkOrder, spec adapter.RunSpec,
	pending *elk.Usage) watchOutcome {

	var (
		out       watchOutcome
		text      strings.Builder
		lastRep   = time.Now()
		stall     = time.NewTimer(qr.r.opts.StallAfter)
		lease     = time.NewTicker(MaxReportInterval)
		lastToolT time.Time
		live      = qr.live
		// What ran since the last tool report, for its receipt.
		didTools, didCmds, didFiles []string
		// didPaths are the same edits, repository-relative, for the
		// receipt's files.
		didPaths []string
		// whyFrom is where the text the next receipt's Why and Next come
		// from starts: only what the agent wrote since the last one.
		whyFrom int
		// questionOpen is true from the agent's question until it resumes.
		questionOpen bool
		// refusedModel is a model Elk named that is not a model id, logged
		// once rather than on every poll.
		refusedModel string
		// unsaid is the agent's text not yet sent as a say; sayC fires the
		// flush inside a reply window.
		unsaid   strings.Builder
		sayTimer *time.Timer
		sayC     <-chan time.Time
		// heldPlan is the agent's latest plan not yet sent, held while
		// planEvery has not passed since lastPlan.
		heldPlan []elk.PlanItem
		lastPlan time.Time
	)
	var capC <-chan time.Time
	if qr.r.opts.Hosted {
		timer := time.NewTimer(time.Until(qr.hostedDeadline))
		defer timer.Stop()
		capC = timer.C
	}
	defer stall.Stop()
	defer lease.Stop()
	defer func() {
		if sayTimer != nil {
			sayTimer.Stop()
		}
	}()
	// Elk's inbox, for a session that can take a message mid-run: comments
	// go in as they are posted rather than with the next report's reply.
	var news chan elk.Inbox
	if live != nil && live.manifest.Has(adapter.CapSteer) {
		news = make(chan elk.Inbox)
		inboxCtx, stopInbox := context.WithCancel(ctx)
		polling := make(chan struct{})
		go func() {
			defer close(polling)
			qr.pollInbox(inboxCtx, wo.RunID, news)
		}()
		defer func() {
			stopInbox()
			<-polling
		}()
	}

	// post reports and folds a cancellation into the outcome, so every call
	// site is one line and none of them can forget the check.
	post := func(p elk.Progress) bool {
		p.RunID = wo.RunID
		if err := qr.reportProgress(ctx, p, pending); err != nil {
			if errors.Is(err, elk.ErrCancelled) {
				out.cancelled = true
				return false
			}
			qr.logf("run %s: %v", wo.RunID, err)
		}
		lastRep = time.Now()
		return true
	}
	send := func(kind elk.ProgressKind, body, why, next string) bool {
		return post(elk.Progress{Kind: kind, Body: body, Why: why, Next: next})
	}
	// say sends the agent's text written since the last say, redacted whole
	// before it is cut so no secret straddles two events.
	say := func() bool {
		text := unsaid.String()
		unsaid.Reset()
		for _, part := range sayParts(qr.redactString(text)) {
			if !send(elk.ProgressSay, part, "", "") {
				return false
			}
		}
		return true
	}
	// plan sends the held plan, when there is one and force or planEvery
	// allows it.
	plan := func(force bool) bool {
		if heldPlan == nil || !force && time.Since(lastPlan) < planEvery {
			return true
		}
		p := heldPlan
		heldPlan, lastPlan = nil, time.Now()
		return post(elk.Progress{Kind: elk.ProgressStep, Body: "Updated the plan", Plan: p})
	}
	// deliver hands the session the task comments waiting in qr.unsent, the
	// way `rein attach` input goes in. Not while the agent's question is
	// open: an adapter may take input then as the answer (Codex does). A
	// session that cannot take them gets a step line and the text goes in the
	// local log, so nothing vanishes.
	deliver := func() bool {
		if live == nil || questionOpen {
			return true
		}
		c := qr.takeComments()
		if c == "" {
			return true
		}
		if err := live.input(ctx, c); err != nil {
			qr.logf("run %s: passing task comments to the session: %v", wo.RunID, err)
			qr.log.Runner(runlog.KindNote, "task comments that reached no session: %s", c)
			return send(elk.ProgressStep, "Couldn't pass your message to the session", "", "")
		}
		// The person is waiting on an answer: the agent's text goes out as
		// it comes for a while.
		qr.replyOpened = time.Now()
		return true
	}
	receipt := func(kind elk.ProgressKind, body, why, next string) bool {
		return say() && plan(false) && send(kind, body, why, next) && deliver()
	}
	report := func(kind elk.ProgressKind, body string) bool { return receipt(kind, body, "", "") }

	for {
		select {
		case <-capC:
			out.capped = true
			out.text = text.String()
			return out
		case <-ctx.Done():
			out.text = text.String()
			return out

		case in := <-news:
			if in.Status != "" && in.Status != "running" {
				qr.logf("run %s: Elk says the run is %s", wo.RunID, in.Status)
				out.cancelled = true
				out.text = text.String()
				return out
			}
			// A switch first: comments in the same answer are held on qr and
			// go in with the resume prompt, not into a session about to stop.
			if m := in.Model; m != "" && m != spec.Model && qr.canSwitchModel(sess) {
				if config.ValidModel(m) {
					out.switchModel = m
					out.text = text.String()
					return out
				}
				if m != refusedModel {
					refusedModel = m
					qr.logf("run %s: not switching to %q: it is not a model id", wo.RunID, m)
				}
			}
			if in.Comments != "" && !deliver() {
				out.text = text.String()
				return out
			}

		case <-sayC:
			sayC = nil
			if !say() || !deliver() {
				out.text = text.String()
				return out
			}

		case <-lease.C:
			why, next := whyNextSince(text.String(), &whyFrom)
			if !receipt(elk.ProgressStep, qr.statusLine(out), why, next) {
				out.text = text.String()
				return out
			}

		case <-stall.C:
			if live.paused() {
				// A person is attached. Silence from the agent is them
				// thinking, not the agent dying, and killing a session
				// somebody is in the middle of driving would be the worst
				// possible reading of the same signal. The Elk lease keeps
				// being renewed above, so the run stays alive meanwhile.
				qr.logf("run %s: no output for %s, but a person is attached — not stalling",
					wo.RunID, qr.r.opts.StallAfter)
				stall.Reset(qr.r.opts.StallAfter)
				continue
			}
			out.stalled = true
			out.text = text.String()
			return out

		case ev, ok := <-sess.Events():
			if !ok {
				out.text = text.String()
				if out.result == nil && out.err == nil {
					out.err = errors.New("the session ended without a result")
				}
				return out
			}
			out.events++
			// Every event, verbatim, before anything decides what to do with
			// it. The log is the only complete account there is: Elk sees a
			// throttled sample, and the deliverable sees a summary.
			qr.log.Event(ev)
			if qr.r.opts.Hosted {
				if blob, err := json.Marshal(ev); err == nil {
					qr.logf("event %s", blob)
				}
			}
			if out.events == 1 {
				// Most adapters learn their vendor session id from the first
				// message rather than at Start, and it is what `rein attach`
				// and a `--resume` both need.
				if id := sess.ID(); id != "" {
					qr.log.Update(func(m *runlog.Meta) { m.SessionID = id })
				}
			}
			// EventIdle is the watchdog's input, so it must NOT reset the
			// watchdog. Everything else is evidence of life.
			if ev.Kind != adapter.EventIdle {
				stall.Reset(qr.r.opts.StallAfter)
			}
			// And it is what tells the fleet reading whether the agent is
			// still waiting on the answer to a question: anything else
			// happening means it is not.
			qr.noteEvent(ev.Kind)
			if live != nil && resumesQuestion(ev) {
				if err := live.asks.retract(ctx, false); err != nil {
					if errors.Is(err, elk.ErrCancelled) {
						out.cancelled = true
						return out
					}
					qr.logf("run %s: retracting ask: %v", wo.RunID, err)
				}
				// Comments held for the question go in now, unless the
				// session is ending and could not take them.
				if questionOpen {
					questionOpen = false
					if ev.Kind != adapter.EventDone && !deliver() {
						out.text = text.String()
						return out
					}
				}
			}

			switch ev.Kind {
			case adapter.EventText:
				if ev.Text != "" {
					text.WriteString(ev.Text)
					text.WriteString("\n")
					out.notePRs(ev.Text)
					unsaid.WriteString(ev.Text)
					unsaid.WriteString("\n")
					if sayC == nil && inReplyWindow(qr.replyOpened, time.Now()) {
						sayTimer = time.NewTimer(sayCoalesce)
						sayC = sayTimer.C
					}
				}

			case adapter.EventToolUse:
				name := "a tool"
				if ev.Tool != nil && ev.Tool.Name != "" {
					name = ev.Tool.Name
				}
				out.tools = append(out.tools, name)
				if p, ok := todoPlan(ev.Tool); ok {
					// The plan has a report of its own, not a receipt line.
					heldPlan = p
					if !plan(false) {
						out.text = text.String()
						return out
					}
					break
				}
				cmd, file := toolCommand(ev.Tool), editedFile(ev.Tool)
				switch {
				case cmd != "":
					out.commands = append(out.commands, cmd)
					out.notePRs(cmd)
					didCmds = append(didCmds, cmd)
				case file != "":
					didFiles = append(didFiles, file)
					if p := repoPath(spec.WorktreeDir, editedPath(ev.Tool)); p != "" {
						didPaths = append(didPaths, p)
					}
				default:
					didTools = append(didTools, name)
				}
				// Throttled: named, but not one round trip per call.
				if time.Since(lastToolT) >= MinReportInterval || time.Since(lastRep) >= MinReportInterval {
					lastToolT = time.Now()
					body := orDefault(didLine(didTools, didCmds, didFiles), "Ran "+name+".")
					files := distinct(didPaths)
					didTools, didCmds, didFiles, didPaths = nil, nil, nil, nil
					why, next := whyNextSince(text.String(), &whyFrom)
					if !(say() && plan(false) && post(elk.Progress{Kind: elk.ProgressTool, Body: body, Why: why, Next: next,
						Files: files}) && deliver()) {
						out.text = text.String()
						return out
					}
				}

			case adapter.EventProgress:
				if ev.Text != "" && !vendorNoise(ev.Text) && time.Since(lastRep) >= MinReportInterval {
					if !report(elk.ProgressStep, ev.Text) {
						out.text = text.String()
						return out
					}
				}

			case adapter.EventUsage:
				if ev.Usage != nil {
					pending.Add(elk.Usage{
						InputTokens:      ev.Usage.InputTokens,
						OutputTokens:     ev.Usage.OutputTokens,
						CacheReadTokens:  ev.Usage.CacheReadTokens,
						CacheWriteTokens: ev.Usage.CacheWriteTokens,
						ReasoningTokens:  ev.Usage.ReasoningTokens,
						Model:            ev.Usage.Model,
						Provider:         qr.provider(ev.Usage.Model),
					})
					// The run's cumulative spend, for the fleet reading. It
					// spans every turn, review round and stall-restart, which
					// `pending` cannot: pending is a DELTA and is emptied at
					// each report.
					qr.noteUsage(*ev.Usage)
				}

			case adapter.EventQuestion:
				// A question is about the work and goes to a person. Never
				// throttled: it is the whole reason the run is waiting.
				// Serialize publication with attach so an in-flight question
				// cannot appear after the attachment's retraction.
				questionOpen = true
				if live != nil && live.asks != nil {
					live.asks.mu.Lock()
					live.asks.pending = true
				}
				ok := report(elk.ProgressQuestion, ev.Text)
				if live != nil && live.asks != nil {
					live.asks.mu.Unlock()
				}
				if !ok {
					out.text = text.String()
					return out
				}

			case adapter.EventPermissionRequest:
				if !qr.denyPermission(ctx, sess, wo, ev, report) {
					out.text = text.String()
					return out
				}

			case adapter.EventIdle:
				// Recorded by the watchdog above; nothing else to do.

			case adapter.EventDone:
				// A reply the person is waiting on goes out even when the
				// turn ends inside the coalesce delay. Otherwise the last
				// text is the deliverable, which Elk shows on its own.
				if sayC != nil && !say() || !plan(true) {
					out.text = text.String()
					return out
				}
				out.result = ev.Result
				if ev.Text != "" {
					text.WriteString(ev.Text)
				}
				out.text = text.String()
				return out

			case adapter.EventError:
				out.err = ev.Err
				if out.err == nil {
					out.err = errors.New(orDefault(ev.Text, "the session failed without saying why"))
				}
				out.text = text.String()
				return out
			}
		}
	}
}

// denyPermission answers a permission request the run's mode did not already
// cover, and records it as a gate.
//
// It denies by default. A permission request reaching the loop means the agent
// wants authority the queue's [config.Queue.PermissionMode] did not grant, and
// there is nobody at the keyboard to grant it — so the honest answer is no,
// with a reason the agent can read, and a `gate` event so the person who
// queued the run can see what was asked for. Granting it would make the mode a
// suggestion, which is the single failure the adapter contract exists to
// prevent.
//
// The one exception is the one that dissolves the premise: while a person is
// attached through `rein attach`, there IS somebody at the keyboard, and the
// request is offered to them for [AttachApprovalGrace] before falling back to
// the deny. The mode is not weakened — nothing is auto-approved — it is a
// human answering a question that was always meant for a human.
func (qr *queueRunner) denyPermission(ctx context.Context, sess adapter.Session, wo *elk.WorkOrder,
	ev adapter.Event, report func(elk.ProgressKind, string) bool) bool {

	what := "something"
	id := ""
	if ev.Permission != nil {
		id = ev.Permission.ID
		what = orDefault(ev.Permission.Summary, ev.Permission.Tool)
	}

	if resp, ok := qr.live.awaitDecision(ctx, id, AttachApprovalGrace); ok {
		resp.ID = id
		if err := sess.Respond(ctx, resp); err != nil {
			qr.logf("run %s: delivering the person's answer for %q: %v", wo.RunID, what, err)
		}
		verdict := "denied"
		if resp.Allow {
			verdict = "allowed"
		}
		qr.logf("run %s: the attached person %s %s", wo.RunID, verdict, what)
		qr.log.Runner(runlog.KindAttach, "the attached person %s: %s", verdict, what)
		return report(elk.ProgressGate, "The agent asked for permission to "+what+
			", which this queue's permission mode ("+string(qr.mode)+") does not grant. A person was "+
			"attached to the session and "+verdict+" it"+reasonSuffix(resp.Reason)+".")
	}

	const reason = "Rein denied this: a queue-driven run has nobody at the keyboard to approve it, " +
		"and the queue's permission mode did not cover it. Work around it, or stop and say in your " +
		"deliverable what you needed and why."
	if err := sess.Respond(ctx, adapter.PermissionResponse{ID: id, Allow: false, Reason: reason}); err != nil {
		qr.logf("run %s: denying %q: %v", wo.RunID, what, err)
	}
	qr.logf("run %s: denied a permission request for %s", wo.RunID, what)
	return report(elk.ProgressGate, "The agent asked for permission to "+what+
		", which this queue's permission mode ("+string(qr.mode)+") does not grant. Rein denied it and "+
		"the agent was told to work around it or stop. Re-run with a wider permission mode if it should be allowed.")
}

func (qr *queueRunner) statusLine(out watchOutcome) string {
	if qr.live.paused() {
		// The lease still has to be renewed while a person drives, but the
		// report must not claim progress Rein did not make.
		return fmt.Sprintf("A person has attached to this session and is driving it directly. "+
			"Rein has stepped back and is keeping the run's lease alive. %d events so far.", out.events)
	}
	if len(out.tools) == 0 {
		return fmt.Sprintf("Still working: %d events so far, no tool calls yet.", out.events)
	}
	return fmt.Sprintf("Still working: %d events, %d tool calls, most recently %s.",
		out.events, len(out.tools), out.tools[len(out.tools)-1])
}

// provider guesses the provider from a model id, because Elk requires one and
// an adapter reports only the model. A wrong guess is visible in Elk's own
// pricing; a missing one silently drops the whole usage record.
func (qr *queueRunner) provider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "claude"):
		return "anthropic"
	case strings.Contains(m, "gpt"), strings.Contains(m, "codex"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"):
		return "openai"
	case strings.Contains(m, "grok"):
		return "xai"
	case strings.Contains(m, "gemini"):
		return "google"
	}
	switch qr.q.AgentKind {
	case "claude":
		return "anthropic"
	case "codex":
		return "openai"
	case "grok":
		return "xai"
	case geminiadapter.Kind:
		return "google"
	}
	return "other"
}

// report sends one progress event and hands back the pending usage delta.
//
// Usage is reported as a DELTA and zeroed on success, because Elk sums. Zeroing
// only on success means a failed report's tokens are carried into the next one
// rather than lost.
//
// Task comments Elk delivered in its reply ([elk.CommentsIn]) join
// qr.unsent, whoever called: only watch can hand them to a session.
func (qr *queueRunner) report(ctx context.Context, runID string, kind elk.ProgressKind, body, why, next string,
	pending *elk.Usage) error {
	return qr.reportProgress(ctx, elk.Progress{RunID: runID, Kind: kind, Body: body, Why: why, Next: next}, pending)
}

// reportProgress is [queueRunner.report] for a whole [elk.Progress]: it
// redacts what leaves the machine and carries the pending usage.
func (qr *queueRunner) reportProgress(ctx context.Context, p elk.Progress, pending *elk.Usage) error {
	if strings.TrimSpace(p.Body) == "" {
		p.Body = "Working."
	}
	p.Body = truncate(qr.redactString(p.Body), 4000)
	p.Why, p.Next = qr.redactString(p.Why), qr.redactString(p.Next)
	if p.Plan != nil {
		plan := make([]elk.PlanItem, len(p.Plan))
		for i, item := range p.Plan {
			plan[i] = elk.PlanItem{Content: qr.redactString(item.Content), Status: item.Status}
		}
		p.Plan = plan
	}
	if pending.Valid() && !pending.Empty() {
		u := *pending
		p.Usage = &u
	}
	res, err := qr.elk.ReportProgress(ctx, p)
	if err != nil {
		return err
	}
	qr.holdComments(res.Text)
	qr.log.Runner(runlog.KindReport, "report_progress %s: %s", p.Kind, firstLine(p.Body))
	if p.Usage != nil {
		*pending = elk.Usage{}
	}
	return nil
}

// holdComments queues the task comments in an Elk reply, if any.
func (qr *queueRunner) holdComments(reply string) {
	if c := elk.CommentsIn(reply); c != "" {
		qr.unsentMu.Lock()
		qr.unsent = append(qr.unsent, c)
		qr.unsentMu.Unlock()
	}
}

// takeComments empties qr.unsent, joined, or "" when nothing waits.
func (qr *queueRunner) takeComments() string {
	qr.unsentMu.Lock()
	defer qr.unsentMu.Unlock()
	c := strings.Join(qr.unsent, "\n\n")
	qr.unsent = nil
	return c
}

// submitReady finishes a successful run.
//
// `ready`, never `done`: Rein does not own the List item, and Elk would coerce
// a `done` to `ready` anyway because the caller is not its owner. Asking for
// what will happen is better than being overruled. For the same reason
// `completed_actions` is empty — closing somebody's List item is not a
// runner's call.
func (qr *queueRunner) submitReady(ctx context.Context, wo *elk.WorkOrder, out watchOutcome,
	wt *worktree.Worktree, repo RepoResolution, spec adapter.RunSpec, sessionID string,
	pending *elk.Usage, noProof string) (*elk.SubmitResult, error) {

	if out.result != nil && sessionID == "" {
		sessionID = out.result.SessionID
	}
	summary := agentAccount(out)
	if summary == "" {
		summary = "The agent finished without producing a written summary. " +
			"This run's details name the branch it left behind."
	}

	status := elk.StatusReady
	code := ""
	if declaresWrongRepo(summary, repo) {
		status = elk.StatusStuck
		code = "wrong_repo"
	}
	if out.emptyResult {
		status = elk.StatusStuck
	}
	title := firstLine(wo.Direction)
	if out.result != nil && !out.result.OK() {
		status = elk.StatusStuck
		title = "Did not finish: " + title
	}
	details := qr.runDetails(wt, repo, spec, sessionID)
	banner := ""

	// The post-run landing check (landing.go): a `pr` or `branch` queue's
	// branch is held to its policy before anyone is told the run is done. A
	// breach does not change the status — the work is what it is, and `ready`
	// already puts it in front of a person — but it headlines both the title
	// and the deliverable, so nobody accepts a merge they did not know about.
	if verdict, checked := qr.checkLanding(ctx, wt); checked {
		details += verdict.footer()
		land := qr.q.LandOrDefault()
		if len(verdict.Breaches) > 0 {
			title = "Landing policy breached: " + title
			banner = verdict.banner(land)
			_ = qr.report(ctx, wo.RunID, elk.ProgressStep, "Landing check: the run went past its queue's policy ("+
				land+"). "+verdict.Breaches[0], "", "", pending)
		}
	}

	// The deliverable is the agent's own text, and the run details go on the
	// run's timeline as its last step (ark:rein#77). Elk's review reads the
	// deliverable and nothing else, and reads its end as closely as its
	// start: a work order that says "end with the Wrangler step" is checked
	// against the last line, so a block of Rein's after it sent every such
	// run back until the round limit made it stuck. Its start is the agent's
	// too, because Elk asks for a plain-English lead and the app's collapsed
	// card shows the first few lines. Only if the timeline refuses do the
	// details go into the deliverable, at the top, so the branch is never
	// lost.
	body := banner + summary
	if !qr.reportDetails(ctx, wo.RunID, details) {
		body = banner + detailsThen(details, summary)
	}

	return qr.submit(ctx, wo, elk.Submission{
		RunID:       wo.RunID,
		Deliverable: body,
		Status:      status,
		StopCode:    code,
		Title:       truncate(title, 200),
		NoProof:     noProof,
	}, pending)
}

// noProofNote is what Rein tells Elk when the agent, asked once, still
// attached no screenshot and wrote no Tests section.
const noProofNote = "The agent attached no screenshot or tests"

// submitWithProof submits, and when Elk refuses a finish that carries no
// proof (a screenshot or a Tests section, scout 0647) passes Elk's words to
// the session once and submits what comes back. Still none, it submits with
// noProofNote so the review says so. res becomes the session that was
// submitted.
func (qr *queueRunner) submitWithProof(ctx context.Context, a adapter.Adapter, spec adapter.RunSpec,
	wo *elk.WorkOrder, wt *worktree.Worktree, repo RepoResolution, res *sessionResult, rec *runRecord,
	pending *elk.Usage) (*elk.SubmitResult, error) {

	sub, err := qr.submitReady(ctx, wo, res.out, wt, repo, spec, res.sessionID, pending, "")
	refusal, refused := elk.ProofRefusal(err)
	if !refused {
		return sub, err
	}
	if a.Manifest().Has(adapter.CapResume) && res.sessionID != "" {
		qr.logf("run %s: Elk asked for a screenshot or test results; asking the agent once", wo.RunID)
		qr.log.Runner(runlog.KindNote, "Elk asked for a screenshot or test results; asking the agent once")
		_ = qr.report(ctx, wo.RunID, elk.ProgressStep,
			"Elk asked for a screenshot or test results before this can finish. Rein asked the agent for them.",
			"", "", pending)
		next := spec
		next.ResumeID = res.sessionID
		next.Prompt = refusal + "\n\nDo that now, then end with your deliverable again. Do not start new work."
		again, err := qr.runSession(ctx, a, next, wo, wt, repo, pending)
		if err != nil || again.done {
			*res = again
			return nil, err
		}
		rec.add(again.out)
		// "Attached the screenshot." must not replace the deliverable it
		// was asked to finish.
		if prev := agentAccount(res.out); unusable(agentAccount(again.out), prev) {
			again.out.deliverable = strings.TrimSpace(prev + "\n\n" + agentAccount(again.out))
		}
		*res = again
		sub, err = qr.submitReady(ctx, wo, res.out, wt, repo, spec, res.sessionID, pending, "")
		if _, still := elk.ProofRefusal(err); !still {
			return sub, err
		}
	}
	qr.log.Runner(runlog.KindNote, "no screenshot or tests after asking; submitted saying so")
	return qr.submitReady(ctx, wo, res.out, wt, repo, spec, res.sessionID, pending, noProofNote)
}

// stuck submits a blocker before any session started. Its own function because
// every pre-flight refusal takes it and they all owe the same shape: what
// stopped it, and what to do about it.
func (qr *queueRunner) stuck(ctx context.Context, wo *elk.WorkOrder, title, why string) error {
	return qr.stuckCode(ctx, wo, title, why, "")
}

// stuckCode is stuck with a stop code naming the cause (stopcode.go).
func (qr *queueRunner) stuckCode(ctx context.Context, wo *elk.WorkOrder, title, why, code string) error {
	var pending elk.Usage
	err := qr.submitStuckCode(ctx, wo, title, why, code, &pending)
	if qr.r.opts.Hosted && err == nil {
		return errors.New(qr.redactString("hosted refusal: " + title))
	}
	return err
}

func (qr *queueRunner) submitStuck(ctx context.Context, wo *elk.WorkOrder, title, why string, pending *elk.Usage) error {
	return qr.submitStuckCode(ctx, wo, title, why, "", pending)
}

func (qr *queueRunner) submitStuckCode(ctx context.Context, wo *elk.WorkOrder, title, why, code string, pending *elk.Usage) error {
	qr.logf("run %s: stuck — %s", wo.RunID, title)
	_, err := qr.submit(ctx, wo, elk.Submission{
		RunID:       wo.RunID,
		Deliverable: "## " + title + "\n\n" + why,
		Status:      elk.StatusStuck,
		Title:       truncate(title, 200),
		StopCode:    code,
	}, pending)
	return err
}

// submit sends the deliverable and returns what Elk recorded — including
// whether it has parked the run for its own review pass, which is the caller's
// cue to stay rather than exit.
//
// A nil result with a nil error means the run was cancelled before the
// deliverable saved; nothing persisted and there is nothing to wait for.
func (qr *queueRunner) submit(ctx context.Context, wo *elk.WorkOrder, s elk.Submission,
	pending *elk.Usage) (*elk.SubmitResult, error) {

	// Every deliverable leaves through here, so this is where a scoped run's
	// values are taken out of whatever the agent wrote (secrets.go) — and out
	// of the copy reportLostSubmit logs if Elk refuses it.
	s.Deliverable = qr.redactString(s.Deliverable)
	qr.resultSummary = s.Deliverable
	s.Title = qr.redactString(s.Title)
	s.Summary = qr.redactString(s.Summary)
	for i := range s.Links {
		s.Links[i].Title = qr.redactString(s.Links[i].Title)
		s.Links[i].URL = qr.redactString(s.Links[i].URL)
	}
	if pending.Valid() && !pending.Empty() {
		u := *pending
		s.Usage = &u
	}
	res, err := qr.elk.SubmitDeliverable(ctx, s)
	switch {
	case errors.Is(err, elk.ErrCancelled):
		qr.logStatus = runlog.StatusCancelled
		qr.log.Runner(runlog.KindSubmit, "Elk refused the deliverable: the run is no longer running")
		qr.reportLostSubmit(ctx, wo, s)
		return nil, nil
	case err != nil:
		qr.log.Runner(runlog.KindSubmit, "submitting failed: %v", err)
		return nil, fmt.Errorf("submitting run %s: %w", wo.RunID, err)
	}
	// The log's own outcome mirrors what Elk recorded rather than what was
	// asked for: Elk coerces a status it disagrees with, and a log that said
	// otherwise would be a second, quieter version of the truth.
	// Record it before a hosted refusal can return.
	if res.Status == elk.StatusStuck || s.Status == elk.StatusStuck {
		qr.logStatus = runlog.StatusStuck
	} else {
		qr.logStatus = runlog.StatusSubmitted
	}
	qr.hostedSubmitted = true
	if qr.r.opts.Hosted && s.Status == elk.StatusStuck && s.Title != "The run hit the hosted run cap" {
		if s.Title == notAPITitle {
			return res, adapter.ErrNotAPIAuth
		}
		return res, errors.New("hosted run refused or failed: " + s.Title)
	}
	*pending = elk.Usage{}
	qr.logf("run %s: submitted %s", wo.RunID, res.Status)
	qr.log.Runner(runlog.KindSubmit, "submitted %s — %s", res.Status, firstLine(s.Title))
	return res, nil
}

// reportLostSubmit works out WHY a deliverable was refused, and says so.
//
// A zero-row submit reads as a cancellation, and usually is one. But there is
// a second cause that looks identical and means the opposite: the run's own
// merged pull request resolved its Elk item, and Elk cancelled the run for
// being work on a finished action (scout migration 0138). The work survived;
// only the record was lost. Reported as "cancelled — nothing persisted", that
// case sends the next person looking for a user who never pressed anything.
//
// Rein can tell them apart because it holds the action id from its own claim:
// an action it was mid-run on cannot be unknown, so "not live" means resolved.
// And in that case the deliverable in hand is the only surviving copy of what
// the agent did, so it goes into the log rather than being dropped.
func (qr *queueRunner) reportLostSubmit(ctx context.Context, wo *elk.WorkOrder, s elk.Submission) {
	const plain = "cancelled before the deliverable saved — nothing persisted"

	if wo.ActionID == "" {
		qr.logf("run %s: %s", wo.RunID, plain)
		return
	}
	state, err := qr.elk.ActionDetail(ctx, qr.workspace, wo.ActionID)
	switch {
	case err != nil:
		qr.logf("run %s: %s (could not check whether action %s is still open: %v)",
			wo.RunID, plain, wo.ActionID, err)
	case state.Live:
		qr.logf("run %s: %s", wo.RunID, plain)
	default:
		qr.logf("run %s: ACTION RESOLVED EXTERNALLY WHILE RUNNING. Action %s is no longer on the List, "+
			"so Elk cancelled this run and refused the deliverable. The WORK IS NOT LOST — only the run "+
			"record is. The usual cause is something resolving the Elk item mid-run, such as a merged pull "+
			"request whose body carried a closing sigil; Rein no longer asks agents to write one.",
			wo.RunID, wo.ActionID)
		qr.logf("run %s: the deliverable Elk refused, which is now its only copy:\n%s",
			wo.RunID, truncate(s.Deliverable, 4000))
	}
}

// runDetails is the block every finished run carries: where the work happened,
// so a person can go and look at it. The branch is the important line — under
// the default the worktree directory is gone by the time anyone reads this,
// and the branch is not.
//
// A successful run's details go on its timeline (reportDetails); a stuck
// one's sit in the deliverable between Rein's explanation and the agent's
// text (stuckBody). Either way nothing of Rein's follows the agent's last
// line (ark:rein#77).
//
// It says what actually happened to the worktree rather than what usually
// does: under `--keep-worktrees` the directory is still there, and details
// claiming it had been removed sent people looking in the wrong place.
func (qr *queueRunner) runDetails(wt *worktree.Worktree, repo RepoResolution, spec adapter.RunSpec, sessionID string) string {
	var b strings.Builder
	b.WriteString("**Run details**\n\n")
	fmt.Fprintf(&b, "- Repository: `%s` — chosen from %s\n", repo.Path, repo.Source)
	if wt != nil {
		if wt.Detached {
			fmt.Fprintf(&b, "- Commit: `%s`, detached, no branch\n", wt.BaseSHA)
		} else {
			fmt.Fprintf(&b, "- Branch: `%s`, based on `%s` at %s\n", wt.Branch, wt.BaseRef, shortSHA(wt.BaseSHA))
		}
		if qr.r.opts.KeepWorktrees {
			fmt.Fprintf(&b, "- Worktree: kept at `%s` (--keep-worktrees)\n", wt.Dir)
		} else {
			fmt.Fprintf(&b, "- Worktree: `%s`, removed once this deliverable saved; the branch was not\n", wt.Dir)
		}
		if wt.Ark.Wanted && !wt.Ark.OK {
			fmt.Fprintf(&b, "- Ark: %s\n", wt.Ark.Note)
		}
		if wt.Initialised {
			fmt.Fprintf(&b, "- Ran `%s` in the worktree first\n", worktree.InitScript)
		}
	}
	fmt.Fprintf(&b, "- Permission mode: `%s`\n", spec.PermissionMode)
	fmt.Fprintf(&b, "- Landing policy: %s\n", landingSummary(qr.q.LandOrDefault()))
	if sessionID != "" {
		fmt.Fprintf(&b, "- Agent session: `%s`\n", sessionID)
	}
	if spec.ResumeID != "" {
		fmt.Fprintf(&b, "- Restarted once, resuming session `%s`\n", spec.ResumeID)
	}
	if note := qr.humanSummary(); note != "" {
		// One line, and never what they typed: the transcript of a takeover
		// is in this machine's run log, and a deliverable is read by whoever
		// asked for the work.
		fmt.Fprintf(&b, "- %s\n", note)
	}
	return b.String()
}

// reportDetails puts a successful run's details on its timeline, as the last
// step before the deliverable, and says whether they got there. Elk's review
// reads the deliverable alone, so details here cannot be mistaken for the end
// of the agent's work. A tool line, not a step: the Lock Screen card reads the
// newest step as what the agent is doing, and these details reached it as
// "Run details Repository: [path]" (Elk #1504). The task's timeline still shows
// them, one tap from the branch.
//
// No usage rides on it: the final delta stays with the submit, where Elk's
// contract asks for it.
func (qr *queueRunner) reportDetails(ctx context.Context, runID, details string) bool {
	err := qr.report(ctx, runID, elk.ProgressTool, details, "", "", &elk.Usage{})
	if err != nil {
		qr.logf("run %s: the run details did not reach the timeline (%v); they go at the top of the deliverable instead",
			runID, err)
	}
	return err == nil
}

// detailsThen is Rein's run details, a rule, then the agent's text: the order
// that keeps the agent's last line the deliverable's last line (ark:rein#77).
func detailsThen(details, agentText string) string {
	return details + "\n---\n\n" + agentText
}

// stuckBody lays out one of Rein's own stuck deliverables: the explanation,
// then the run details, then — when the agent wrote anything — its text under
// heading. The agent's text goes last because it is the agent's: a reader, or
// Elk, looking for how the agent ended finds the agent's own last line, not
// Rein's.
func stuckBody(why, details, heading, agentText string) string {
	if agentText == "" {
		return why + "\n\n" + details
	}
	return why + "\n\n" + detailsThen(details, heading+"\n\n"+truncate(agentText, 20000))
}

// stoppedBody is stuckBody for a run that stalled or failed: whatever the agent
// managed to say before it stopped is kept, because it is usually the only
// account of how far the work got.
func stoppedBody(why, details string, out watchOutcome) string {
	text := strings.TrimSpace(out.text)
	if text == "" {
		why += "\n\nThe agent produced no output before it stopped."
	}
	return stuckBody(why, details, "### What the agent had said before it stopped", text)
}

// agentAccount is what the agent wrote to finish its session: its result's
// summary, or failing that the text it streamed. Empty when it wrote nothing.
// When Rein had to write the deliverable itself (deliverable.go), it is that.
func agentAccount(out watchOutcome) string {
	if out.deliverable != "" {
		return out.deliverable
	}
	summary := ""
	if out.result != nil {
		summary = strings.TrimSpace(out.result.Summary)
	}
	if summary == "" {
		summary = strings.TrimSpace(out.text)
	}
	return summary
}

// capabilityHelp explains the two lists a packet's required_capabilities are
// checked against, and what this machine has in each.
//
// It goes into every capability refusal because the useful thing to tell a
// person is not "you are missing X" but "these are the two lists I checked,
// here is what is in them, and here is which one your name belongs in".
// Without it the two namespaces are invisible, and the dogfood failure
// (ark:rein#17) read as "the claude adapter is broken" when the truth was that
// an environment name had been asked of the wrong list.
//
// host is this run's view of the machine, which on a Wrangler queue depends on
// whether the run is a Wrangler cycle (drive).
func capabilityHelp(m adapter.Manifest, host *HostCapabilities) string {
	var b strings.Builder
	b.WriteString("Elk's `required_capabilities` are NAMES of tools and credentials \u2014 Elk never passes\n")
	b.WriteString("values. Rein checks each name against whichever of two lists can answer it:\n\n")
	fmt.Fprintf(&b, "- **What the %s agent can do** \u2014 the adapter contract's closed vocabulary: %s\n",
		m.Kind, orDefault(strings.Join(m.Declared(), ", "), "nothing declared"))
	fmt.Fprintf(&b, "- **What this machine holds** \u2014 every other name: %s\n\n",
		orDefault(strings.Join(host.Describe(), ", "), "nothing"))
	return b.String()
}

// wranglerOnlyNote explains a refusal that only the Wrangler opt-in could have
// answered: on a Wrangler queue, `mcp:elk` is the owner's connector, and a run
// that is not a Wrangler cycle does not get it (ark:rein#50). Empty otherwise.
func (qr *queueRunner) wranglerOnlyNote(cycle bool, missing []string) string {
	if cycle || !qr.q.IsWrangler() || qr.mcp.connector {
		return ""
	}
	for _, name := range missing {
		if canonicalCapability(name) == "mcp:elk" {
			return "Queue " + qr.q.Name + " is a Wrangler queue. The `mcp:elk` it declares is its owner's\n" +
				"Elk connector, which only a Wrangler cycle \u2014 a packet that requires `pm` \u2014 is\n" +
				"given. Every other run here gets the ordinary MCP setup, which on a Wrangler queue\n" +
				"is no servers at all.\n\n"
		}
	}
	return ""
}

// quoteList renders names as the contents of a TOML array: `"a", "b"`.
func quoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	return strings.Join(quoted, ", ")
}

// shortSHA abbreviates a commit for a log line or a footer.
func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 8 {
		return sha[:8]
	}
	if sha == "" {
		return "an unknown commit"
	}
	return sha
}

// reasonSuffix renders a person's stated reason, or nothing when they gave
// none. A gate event that ended in "and allowed it because " reads worse than
// one that ends in "and allowed it".
func reasonSuffix(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return ""
	}
	return " — " + firstLine(reason)
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "queued Elk run"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n\n…truncated by Rein at " + fmt.Sprint(n) + " characters."
}
