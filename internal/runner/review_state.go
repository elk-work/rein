package runner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/elk-work/rein/internal/adapter"
	geminiadapter "github.com/elk-work/rein/internal/adapter/gemini"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/worktree"
)

// Credentials and process-local services are deliberately absent. Reconstruct
// them from the queue on restart; an old proxy address is not a connector.
type pendingReview struct {
	AgentKind                           string
	Queue                               string
	Round                               int
	WorkOrder                           *elk.WorkOrder
	Worktree                            *worktree.Worktree
	Repo                                RepoResolution
	SessionID, SentBack, Verdict        string
	Prompt, SystemPrompt, Model, Effort string
	WorkDir                             string
	Commands, PRs                       []string
}

func reviewFile(dir, runID string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "reviews", fmt.Sprintf("%x.json", sha256.Sum256([]byte(runID))))
}

func (qr *queueRunner) saveReview(c reviewCont) error {
	path := reviewFile(qr.r.opts.StateDir, c.wo.RunID)
	if path == "" {
		return nil
	}
	p := pendingReview{Queue: qr.r.opts.Config.QueueLabel(qr.q), AgentKind: qr.q.AgentKind, Round: c.round,
		WorkOrder: c.wo, Worktree: c.wt, Repo: c.repo, SessionID: c.sessionID,
		SentBack: c.sentBack, Verdict: c.verdict, Prompt: c.spec.Prompt, SystemPrompt: c.spec.SystemPrompt,
		Model: c.spec.Model, Effort: c.spec.Effort, WorkDir: c.spec.WorktreeDir, Commands: c.rec.commands, PRs: c.rec.prs}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	// Redact structured string values before encoding, so escaped secrets are
	// covered as well as ordinary text. No environment or MCP config is saved.
	var values any
	if err = json.Unmarshal(b, &values); err != nil {
		return err
	}
	var redact func(any) any
	redact = func(v any) any {
		switch x := v.(type) {
		case string:
			return qr.redactString(x)
		case map[string]any:
			for k, v := range x {
				x[k] = redact(v)
			}
		case []any:
			for i, v := range x {
				x[i] = redact(v)
			}
		}
		return v
	}
	b, err = json.Marshal(redact(values))
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".review-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func (qr *queueRunner) resumeReviews(ctx context.Context) {
	if qr.r.opts.StateDir == "" || qr.r.opts.Hosted {
		return
	}
	paths, err := filepath.Glob(filepath.Join(qr.r.opts.StateDir, "reviews", "*.json"))
	if err != nil {
		qr.logf("loading pending reviews: %v", err)
		return
	}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		var p pendingReview
		if err != nil || json.Unmarshal(b, &p) != nil || p.WorkOrder == nil || p.Round < 1 {
			qr.logf("could not read pending review %s", filepath.Base(path))
			continue
		}
		if p.Queue != qr.r.opts.Config.QueueLabel(qr.q) {
			continue
		}
		if p.AgentKind != qr.q.AgentKind {
			qr.logf("run %s: pending review needs its original agent kind %s", p.WorkOrder.RunID, p.AgentKind)
			continue
		}
		child := qr.forkReview(p.WorkOrder.RunID)
		a, err := adapter.Get(qr.q.AgentKind)
		if err != nil {
			qr.logf("resuming pending review: %v", err)
			continue
		}
		child.openLog(p.WorkOrder)
		child.logStatus = runlog.StatusSubmitted
		child.log.Runner(runlog.KindReview, "resuming pending review after restart (round %d)", p.Round)
		child.log.Update(func(m *runlog.Meta) { m.SessionID = p.SessionID })
		if p.Worktree != nil {
			child.recordWorktree(p.Worktree, p.Repo)
		}
		c := reviewCont{round: p.Round, a: a, wo: p.WorkOrder, wt: p.Worktree, repo: p.Repo,
			sessionID: p.SessionID, sentBack: p.SentBack, verdict: p.Verdict,
			rec: runRecord{commands: p.Commands, prs: p.PRs}, pending: &elk.Usage{}, clean: &cleanupStack{}, restored: true}
		runtime, _ := SplitRequirements(p.WorkOrder.RequiredCapabilities)
		c.spec = adapter.RunSpec{RunID: p.WorkOrder.RunID, AgentKind: qr.q.AgentKind,
			Prompt: p.Prompt, SystemPrompt: p.SystemPrompt, Model: p.Model, Effort: p.Effort,
			WorktreeDir: p.WorkDir, PermissionMode: qr.mode, RequiredCapabilities: runtime, Queue: qr.q.Name,
			Settings: qr.q.Settings, Timeouts: adapter.Timeouts{Idle: qr.r.opts.StallAfter}}
		if p.Worktree != nil {
			c.spec.WorktreeDir = p.Worktree.Dir
		}
		c.clean.add(child.closeLog)
		c.clean.add(child.holdAcrossSleep(ctx, c.wo.RunID))
		c.clean.add(func() {
			if !child.keepReview {
				child.reap(c.wt)
				if c.wt == nil && !child.r.opts.KeepWorktrees {
					_ = os.RemoveAll(c.spec.WorktreeDir)
				}
				child.reapAdapter(a, c.wo.RunID, &c.spec)
			}
		})
		c.clean.add(func() {
			if !child.keepReview {
				child.finishArkRuns(ctx, c.wt)
			}
		})
		untrack := qr.trackReview(c.wo.RunID)
		qr.reviews.Add(1)
		go func() { defer qr.reviews.Done(); defer untrack(); child.continueReview(ctx, c) }()
	}
}

// Only rebuild runtime resources when the review actually requests a revision.
func (qr *queueRunner) restoreReviewRuntime(c *reviewCont) error {
	spec := &c.spec
	spec.MCPServers = qr.mcpServersForRun()
	cycle := wranglerCycle(qr.q, c.wo.RequiredCapabilities)
	if cycle {
		spec.MCPServers = nil
		spec.WranglerConnectorAccount = qr.q.WranglerAccount()
		if spec.WranglerConnectorAccount == "" {
			spec.WranglerConnectorAccount = qr.workspace + "/" + qr.q.Name
		}
	} else if qr.mcp.connector {
		spec.DeniedTools = append(spec.DeniedTools, qr.mcp.denied...)
	}
	values := map[string]string{}
	if qr.q.Scoped() {
		env, err := qr.resolveSecrets()
		if err != nil {
			return err
		}
		spec.Env = env
		for k, v := range env {
			values[k] = v
		}
	} else {
		spec.PassEnv = qr.passEnv(spec.MCPServers != nil)
	}
	if qr.q.AgentKind == geminiadapter.Kind {
		key, err := qr.resolveGeminiKey()
		if err != nil {
			return err
		}
		spec.ProviderKey = key
		values[geminiadapter.KeyName] = key
	}
	qr.applySecrets(qr.withOwnToken(values))
	if qr.mcp.connector && !cycle {
		proxy, err := qr.startRunConnector(spec)
		if err != nil {
			return err
		}
		c.clean.add(proxy.Close)
	}
	c.clean.add(qr.giveCapture(spec))
	c.clean.add(qr.giveChrome(spec))
	return nil
}
