package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// session is one `codex app-server` process running one turn.
//
// **One turn per session, deliberately.** Rein's unit of work is an Elk run:
// one packet in, one deliverable out. Codex threads outlive turns, so the
// thread id is what Result.SessionID reports and what a later resume takes —
// but the session this adapter hands back ends when its turn does. A follow-up
// is a new session against the same thread, not a second turn smuggled into
// the first.
type session struct {
	connectorURL string
	a            *Adapter
	spec         adapter.RunSpec
	proc         *process
	conn         *conn
	sandbox      sandboxMode
	approval     askForApproval
	started      time.Time

	events chan adapter.Event
	done   chan struct{}

	// emitMu serialises everything that reaches the event channel: sequence
	// numbering, the pre-Start buffer, and the close. Exactly one goroutine
	// sends at a time and Seq is assigned in send order, so a consumer can
	// trust that a gap means a bug rather than a race.
	emitMu       sync.Mutex
	seq          uint64
	buf          []adapter.Event
	opened       bool
	eventsClosed bool
	lastEmit     time.Time
	deltaTail    strings.Builder

	finishOnce sync.Once

	mu             sync.Mutex
	threadID       string
	turnID         string
	model          string
	transcriptPath string
	reportedTotal  tokenUsageBreakdown
	usage          adapter.Usage
	finalText      string
	lastText       string
	pending        map[string]pendingApproval
	interrupted    bool
	result         adapter.Result
	err            error

	// rateLimit is the subscription window as the last
	// `account/rateLimits/updated` described it, for [session.Telemetry].
	rateLimit *adapter.RateLimit
}

// pendingApproval is a server request waiting on [adapter.Session.Respond].
type pendingApproval struct {
	id     json.RawMessage
	method string
	// profile is the requested permission profile, echoed back verbatim as the
	// grant when the run loop allows it. Request and grant are the same shape
	// in the schema (`RequestPermissionProfile` and `GrantedPermissionProfile`
	// are both `{fileSystem?, network?}`), so echoing is exact rather than a
	// guess at what was asked for.
	profile json.RawMessage
}

func newSession(a *Adapter, spec adapter.RunSpec, proc *process, sb sandboxMode, ap askForApproval) *session {
	return &session{
		a:        a,
		spec:     spec,
		proc:     proc,
		conn:     newConn(proc.stdout, proc.stdin),
		sandbox:  sb,
		approval: ap,
		started:  time.Now(),
		events:   make(chan adapter.Event),
		done:     make(chan struct{}),
		pending:  map[string]pendingApproval{},
	}
}

// handshake brings the app server up to a running turn: initialize, the
// `initialized` notification, a thread, and the turn carrying the packet.
//
// The read loop starts first, because every one of those is a request whose
// answer only arrives through it.
func (s *session) handshake(ctx context.Context) error {
	go func() { _ = s.conn.serve(s) }()

	var initRes initializeResponse
	if err := s.conn.call(ctx, methodInitialize, initializeParams{
		ClientInfo: clientInfo{Name: "rein", Version: "0", Title: "Rein runner"},
	}, &initRes); err != nil {
		return s.startupError("initialize", err)
	}
	if err := s.conn.notify(methodInitialized, struct{}{}); err != nil {
		return s.startupError("initialized", err)
	}

	// The plan-login check, before any thread exists: a session on an API
	// key or a cloud provider never gets a turn (planauth.go).
	var acct accountReadResponse
	if err := s.conn.call(ctx, methodAccountRead, accountReadParams{}, &acct); err != nil {
		return s.startupError(methodAccountRead, err)
	}
	if err := checkPlanAccount(acct); err != nil {
		return err
	}

	var thr threadResponse
	if s.spec.ResumeID != "" {
		err := s.conn.call(ctx, methodThreadResume, threadResumeParams{
			ThreadID:              s.spec.ResumeID,
			Cwd:                   s.spec.WorktreeDir,
			Sandbox:               s.sandbox,
			ApprovalPolicy:        s.approval,
			Model:                 s.spec.Model,
			DeveloperInstructions: s.spec.SystemPrompt,
			Config:                s.threadConfig(),
		}, &thr)
		if err != nil {
			return s.startupError("thread/resume "+s.spec.ResumeID, err)
		}
	} else {
		err := s.conn.call(ctx, methodThreadStart, threadStartParams{
			Cwd:                   s.spec.WorktreeDir,
			Sandbox:               s.sandbox,
			ApprovalPolicy:        s.approval,
			Model:                 s.spec.Model,
			DeveloperInstructions: s.spec.SystemPrompt,
			Config:                s.threadConfig(),
		}, &thr)
		if err != nil {
			return s.startupError("thread/start", err)
		}
	}

	s.mu.Lock()
	s.threadID = thr.Thread.ID
	s.model = thr.Model
	s.transcriptPath = thr.Thread.Path
	s.mu.Unlock()

	prompt := s.spec.Prompt
	if prompt == "" {
		// A resume with no prompt is a legitimate "carry on"; Codex needs
		// *something* in the turn, so say what the run loop means.
		prompt = "Continue the work already in progress in this thread."
	}
	var turnRes turnStartResponse
	if err := s.conn.call(ctx, methodTurnStart, turnStartParams{
		ThreadID: thr.Thread.ID,
		Input:    textInput(prompt),
		Model:    s.spec.Model,
		Effort:   s.spec.Effort,
	}, &turnRes); err != nil {
		return s.startupError("turn/start", err)
	}
	s.mu.Lock()
	s.turnID = turnRes.Turn.ID
	s.mu.Unlock()
	return nil
}

// threadConfig turns [adapter.RunSpec.MCPServers] into the `config` overlay
// `thread/start` accepts — the same dotted overrides `codex -c` takes, as a
// nested object. Verified against the running binary: a server passed this way
// appears in `mcpServer/startupStatus/updated`.
//
// The developer's own `~/.codex/config.toml` servers do not load beside it:
// the session runs under a private CODEX_HOME with no config.toml (home.go,
// ark:rein#21). The built-in `codex_apps` still does (ark:rein#22).
func (s *session) threadConfig() map[string]any {
	if len(s.spec.MCPServers) == 0 {
		return nil
	}
	servers := make(map[string]any, len(s.spec.MCPServers))
	for name, cfg := range s.spec.MCPServers {
		servers[name] = cfg
	}
	return map[string]any{"mcp_servers": servers}
}

// startupError is what a run that never started reports, and it ends up in the
// `stuck` deliverable and the local log — so it has to answer "why" on its
// own. Before ark:rein#38 it said only "initialize: context deadline
// exceeded", which is how a start-up that had been re-indexing 64k rollouts
// for weeks looked exactly like a protocol break after a Codex upgrade.
//
// It says which step stalled and after how long, what the app server had said
// by then (those events are otherwise dropped with the session), the one
// start-up cost this package knows how to cause, and stderr — including when
// stderr is empty, because "Codex printed nothing" is itself a finding.
func (s *session) startupError(stage string, err error) error {
	timedOut := errors.Is(err, context.DeadlineExceeded)
	var head string
	if timedOut {
		head = fmt.Sprintf("codex: `%s app-server` did not answer %s within the %s startup timeout",
			s.a.binary, stage, orDefault(s.spec.Timeouts.Startup, defaultStartupTimeout))
	} else {
		head = fmt.Sprintf("codex: %s failed after %s", stage, time.Since(s.started).Round(100*time.Millisecond))
	}

	var detail strings.Builder
	detail.WriteString("\nwhat the app server said before that: " + s.handshakeTrail())
	if timedOut && stage == methodInitialize {
		if hint := s.proc.home.startupHint(); hint != "" {
			detail.WriteString("\n" + hint)
		}
	}
	if tail := s.proc.stderrTail.String(); tail != "" {
		detail.WriteString("\napp-server stderr:\n" + tail)
	} else {
		detail.WriteString("\napp-server stderr: (nothing)")
	}
	return fmt.Errorf("%s: %w%s", head, err, detail.String())
}

// handshakeTrail summarises the events buffered while Start was running: the
// notifications Codex sent before the step that failed. "nothing" is the
// useful answer when it is the answer — it means Codex was busy before it
// would talk at all.
func (s *session) handshakeTrail() string {
	const keep = 10
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if len(s.buf) == 0 {
		return "nothing"
	}
	var parts []string
	for _, ev := range s.buf {
		text := ev.Text
		if text == "mcpServer/startupStatus/updated" {
			var n struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			}
			if json.Unmarshal(ev.Raw, &n) == nil && n.Name != "" {
				text += " (" + n.Name + ": " + n.Status + ")"
			}
		}
		parts = append(parts, text)
	}
	if len(parts) > keep {
		parts = append([]string{fmt.Sprintf("…%d earlier", len(parts)-keep)}, parts[len(parts)-keep:]...)
	}
	return strings.Join(parts, "; ")
}

// abort tears down a session that never started.
func (s *session) abort() {
	_ = s.conn.close()
	s.proc.kill()
	go func() {
		_ = s.proc.wait()
		s.proc.home.remove()
	}()
}

// run opens the event channel and starts the two watchers a live session
// needs: the process, and the total-time bound.
func (s *session) run() {
	go s.openEvents()

	go func() {
		err := s.proc.wait()
		// A process that exits before turn/completed took the session with it.
		s.finishProcessExit(err)
	}()

	if total := s.spec.Timeouts.Total; total > 0 {
		go func() {
			t := time.NewTimer(total)
			defer t.Stop()
			select {
			case <-s.done:
			case <-t.C:
				s.timeOut(total)
			}
		}()
	}
}

// ---------------------------------------------------------------- events ---

// emit is the only path onto the event channel. Sequence numbers are assigned
// here, in send order, from 1.
func (s *session) emit(ev adapter.Event) {
	ev = s.redactEvent(ev)
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.eventsClosed {
		return
	}
	s.seq++
	ev.Seq = s.seq
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	s.lastEmit = time.Now()
	s.deltaTail.Reset()
	if !s.opened {
		s.buf = append(s.buf, ev)
		return
	}
	s.events <- ev
}

// openEvents releases the events observed during the handshake and switches
// [session.emit] to sending directly. Everything the app server said while
// Start was still running is delivered, in order, before anything that
// happened after it.
func (s *session) openEvents() {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.eventsClosed {
		s.buf = nil
		return
	}
	for _, ev := range s.buf {
		s.events <- ev
	}
	s.buf = nil
	s.opened = true
}

// heartbeat coalesces the agent-message delta stream into at most one progress
// event per [Adapter.heartbeat]. One event per token would be useless; none at
// all would read to the watchdog as a stalled agent.
func (s *session) heartbeat(delta string) {
	s.emitMu.Lock()
	if s.eventsClosed {
		s.emitMu.Unlock()
		return
	}
	s.deltaTail.WriteString(delta)
	if s.a.heartbeat <= 0 || time.Since(s.lastEmit) < s.a.heartbeat {
		s.emitMu.Unlock()
		return
	}
	text := s.deltaTail.String()
	s.emitMu.Unlock()
	s.emit(adapter.Event{Kind: adapter.EventProgress, Text: text})
}

// ------------------------------------------------------- handler plumbing ---

// onNotification implements [handler]. It runs on the read goroutine, so it
// blocks the stream while an event is being delivered — which is the
// back-pressure the contract asks for.
func (s *session) onNotification(method string, params json.RawMessage) {
	switch method {
	case notifyThreadStarted:
		var n threadStartedNotification
		if json.Unmarshal(params, &n) == nil {
			s.mu.Lock()
			if s.transcriptPath == "" {
				s.transcriptPath = n.Thread.Path
			}
			s.mu.Unlock()
		}
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "codex thread started", Raw: params})

	case notifyTurnStarted:
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "turn started", Raw: params})

	case notifyTurnCompleted:
		var n turnNotification
		if err := json.Unmarshal(params, &n); err != nil {
			s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "turn/completed (undecodable)", Raw: params})
			return
		}
		s.finishTurn(n.Turn)

	case notifyItemStarted:
		var n itemNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		s.onItemStarted(n.Item, params)

	case notifyItemCompleted:
		var n itemNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		s.onItemCompleted(n.Item, params)

	case notifyAgentMsgDelta:
		var n agentMessageDeltaNotification
		if json.Unmarshal(params, &n) == nil {
			s.heartbeat(n.Delta)
		}

	case notifyThreadTokenUsage:
		var n threadTokenUsageNotification
		if json.Unmarshal(params, &n) == nil {
			s.onUsage(n.TokenUsage, params)
		}

	case notifyThreadStatus:
		var n threadStatusChangedNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		switch n.Status.Type {
		case "idle":
			// A real signal, not a scraped spinner: this is what Rein means by
			// idle (docs/adapters.md).
			s.emit(adapter.Event{Kind: adapter.EventIdle, Text: "thread idle", Raw: params})
		case "active":
			if len(n.Status.ActiveFlags) > 0 {
				s.emit(adapter.Event{Kind: adapter.EventProgress,
					Text: "thread active: " + strings.Join(n.Status.ActiveFlags, ", "), Raw: params})
			}
		case "systemError":
			s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "thread reported a system error", Raw: params})
		}

	case notifyError:
		var n errorNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		text := n.Error.Message
		if n.Error.AdditionalDetails != "" {
			text += ": " + n.Error.AdditionalDetails
		}
		if n.WillRetry {
			// Codex reports a retriable failure and then retries it. Treating
			// one as terminal ends sessions that were about to succeed.
			s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "retrying after error: " + text, Raw: params})
			return
		}
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "error: " + text, Raw: params})

	case notifyRateLimits:
		// Structured for the fleet reading and the back-off, and still relayed
		// as progress — the run log is where a person reads it.
		var n rateLimitsUpdatedNotification
		if json.Unmarshal(params, &n) == nil {
			s.onRateLimits(n)
		}
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: method, Raw: params})

	default:
		// The vendor emits a great deal Rein has no kind for — MCP startup,
		// rate limits, hooks, realtime. Relay it rather than inventing a kind
		// (docs/adapters.md).
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: method, Raw: params})
	}
}

func (s *session) onItemStarted(item threadItem, raw json.RawMessage) {
	switch item.Type {
	case itemCommandExecution:
		s.emit(adapter.Event{Kind: adapter.EventToolUse, Text: item.Command, Raw: raw,
			Tool: &adapter.ToolUse{ID: item.ID, Name: "shell", Input: jsonObject("command", item.Command, "cwd", item.Cwd)}})
	case itemFileChange:
		s.emit(adapter.Event{Kind: adapter.EventToolUse, Text: "applying a patch", Raw: raw,
			Tool: &adapter.ToolUse{ID: item.ID, Name: "apply_patch", Input: item.Changes}})
	case itemMCPToolCall:
		s.emit(adapter.Event{Kind: adapter.EventToolUse, Text: item.Server + "/" + item.Tool, Raw: raw,
			Tool: &adapter.ToolUse{ID: item.ID, Name: item.Server + "/" + item.Tool, Input: item.Arguments}})
	case itemDynamicToolCall:
		s.emit(adapter.Event{Kind: adapter.EventToolUse, Text: item.Tool, Raw: raw,
			Tool: &adapter.ToolUse{ID: item.ID, Name: item.Tool, Input: item.Arguments}})
	case itemWebSearch:
		s.emit(adapter.Event{Kind: adapter.EventToolUse, Text: item.Query, Raw: raw,
			Tool: &adapter.ToolUse{ID: item.ID, Name: "web_search", Input: jsonObject("query", item.Query)}})
	case itemReasoning:
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "reasoning", Raw: raw})
	case itemUserMessage, itemAgentMessage:
		// The text arrives on item/completed; announcing an empty one twice
		// would just be noise.
	default:
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "item started: " + item.Type, Raw: raw})
	}
}

func (s *session) onItemCompleted(item threadItem, raw json.RawMessage) {
	switch item.Type {
	case itemAgentMessage:
		if item.Text == "" {
			return
		}
		s.mu.Lock()
		s.lastText = item.Text
		if item.Phase == "final_answer" {
			s.finalText = item.Text
		}
		s.mu.Unlock()
		s.emit(adapter.Event{Kind: adapter.EventText, Text: item.Text, Raw: raw})
	case itemPlan:
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: item.Text, Raw: raw})
	case itemCommandExecution:
		text := "command finished"
		if item.ExitCode != nil {
			text = fmt.Sprintf("command exited %d", *item.ExitCode)
		}
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: text, Raw: raw})
	case itemFileChange, itemMCPToolCall, itemDynamicToolCall, itemWebSearch:
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: item.Type + " " + item.Status, Raw: raw})
	}
}

// onUsage differences Codex's running total against what has already been
// reported. Rein's usage events are increments (docs/adapters.md), Codex's
// notification is cumulative, and `last` is per model call rather than per
// notification — so neither field can be forwarded as-is.
func (s *session) onUsage(u threadTokenUsage, raw json.RawMessage) {
	s.mu.Lock()
	prev := s.reportedTotal
	s.reportedTotal = u.Total
	inc := adapter.Usage{
		InputTokens:      u.Total.InputTokens - prev.InputTokens,
		OutputTokens:     u.Total.OutputTokens - prev.OutputTokens,
		CacheReadTokens:  u.Total.CachedInputTokens - prev.CachedInputTokens,
		CacheWriteTokens: u.Total.CacheWriteInputTokens - prev.CacheWriteInputTokens,
		Model:            s.model,
	}
	s.usage.Add(inc)
	s.mu.Unlock()
	if inc.InputTokens == 0 && inc.OutputTokens == 0 &&
		inc.CacheReadTokens == 0 && inc.CacheWriteTokens == 0 {
		return
	}
	s.emit(adapter.Event{Kind: adapter.EventUsage, Usage: &inc, Raw: raw})
}

// onRequest implements [handler]. Every server request gets an answer, even
// the ones this adapter refuses: an unanswered request wedges the turn.
func (s *session) onRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case reqCommandApproval:
		var p commandApprovalParams
		_ = json.Unmarshal(params, &p)
		s.approvalRequested(id, method, nil, adapter.PermissionRequest{
			Tool:    "shell",
			Summary: approvalSummary(p.Command, p.Reason),
			Input:   params,
		})

	case reqFileChangeApproval:
		var p fileChangeApprovalParams
		_ = json.Unmarshal(params, &p)
		s.approvalRequested(id, method, nil, adapter.PermissionRequest{
			Tool:    "apply_patch",
			Summary: approvalSummary("write files under "+orString(p.GrantRoot, s.spec.WorktreeDir), p.Reason),
			Input:   params,
		})

	case reqPermissionsApproval:
		var p permissionsApprovalParams
		_ = json.Unmarshal(params, &p)
		s.approvalRequested(id, method, p.Permissions, adapter.PermissionRequest{
			Tool:    "permissions",
			Summary: approvalSummary("widen the sandbox", p.Reason),
			Input:   params,
		})

	case reqExecCommandApproval, reqApplyPatchApproval:
		// The v1 spellings. They take a different decision vocabulary
		// (`approved` / `{denied: …}`), and this adapter has never seen the
		// running binary send one — 0.150.1 uses the v2 methods above. Decline
		// rather than answer in a shape that has not been verified.
		s.declineRequest(id, method, codeMethodNotFound,
			"rein: this adapter answers the v2 approval methods; see docs/adapter-codex.md")

	case reqToolUserInput, reqMCPElicitation:
		// A question about the *work*, not about authority. Rein has a place
		// for that — EventQuestion, answered with Send — but wiring an answer
		// back needs a per-question response shape this adapter has not
		// verified, so v0 declines and says so rather than pretending.
		s.emit(adapter.Event{Kind: adapter.EventProgress,
			Text: "codex asked for interactive input; rein declined it (" + method + ")", Raw: params})
		s.declineRequest(id, method, codeRequestFailed,
			"rein: this session has no interactive user; answer nothing and continue or stop")

	case reqAuthTokensRefresh:
		// Never. Rein shells out to the vendor binary on the developer's own
		// login and does not read, mint, refresh or replay a credential
		// (docs/adapters.md, elk docs/rein.md §3).
		s.declineRequest(id, method, codeRequestFailed,
			"rein: never handles vendor credentials; refresh the Codex login with `codex login`")

	default:
		s.emit(adapter.Event{Kind: adapter.EventProgress,
			Text: "declined an unsupported server request: " + method, Raw: params})
		s.declineRequest(id, method, codeMethodNotFound, "rein: "+method+" is not implemented by this client")
	}
}

// approvalRequested routes one approval. In a mode that promised approvals it
// becomes an [adapter.EventPermissionRequest] the run loop must answer; in a
// mode that did not, it is answered here from the mode itself — because a
// request nobody promised to answer is a hang, and a hang is worse than either
// decision.
//
// In practice the second branch is unreachable: `read_only` and `full` both
// start the thread with approvalPolicy `never`, under which Codex raises no
// approvals at all. It exists so that a Codex release which starts raising
// them anyway cannot wedge a run.
func (s *session) approvalRequested(id json.RawMessage, method string, profile json.RawMessage, req adapter.PermissionRequest) {
	req.ID = string(id)

	if !s.spec.PermissionMode.NeedsApprovals() {
		allow := s.spec.PermissionMode == adapter.PermissionFull
		s.answerApproval(id, method, profile, allow)
		verb := "denied"
		if allow {
			verb = "auto-approved"
		}
		s.emit(adapter.Event{Kind: adapter.EventProgress,
			Text: fmt.Sprintf("%s %s (permission mode %s)", verb, req.Summary, s.spec.PermissionMode)})
		return
	}

	s.mu.Lock()
	s.pending[req.ID] = pendingApproval{id: id, method: method, profile: profile}
	s.mu.Unlock()
	s.emit(adapter.Event{Kind: adapter.EventPermissionRequest, Text: req.Summary,
		Permission: &req, Raw: req.Input})
}

// answerApproval writes the vendor-shaped answer for one approval method.
func (s *session) answerApproval(id json.RawMessage, method string, profile json.RawMessage, allow bool) {
	switch method {
	case reqPermissionsApproval:
		// The response grants a profile; there is no "decline" in the shape.
		// Allowing echoes the requested profile back — request and grant are
		// the same schema — and denying grants nothing, which is what "you may
		// do nothing extra" means here.
		if allow && len(profile) > 0 {
			_ = s.conn.reply(id, map[string]json.RawMessage{"permissions": profile})
			return
		}
		_ = s.conn.reply(id, permissionsApprovalResponse{Permissions: map[string]any{}})
	default:
		d := decisionDecline
		if allow {
			d = decisionAccept
		}
		_ = s.conn.reply(id, decisionResponse{Decision: d})
	}
}

func (s *session) declineRequest(id json.RawMessage, method string, code int, msg string) {
	_ = s.conn.replyError(id, code, msg)
}

// ------------------------------------------------------------- interface ---

// ID implements [adapter.Session]. It is the Codex thread id, available from
// the moment Start returns — which is what `rein attach` and a later
// [adapter.RunSpec.ResumeID] need.
func (s *session) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.redact(s.threadID)
}

// Events implements [adapter.Session].
func (s *session) Events() <-chan adapter.Event { return s.events }

// Send implements [adapter.Session]. Mid-turn text is a steer, which is
// Codex's own word for it: the running turn takes the input into account
// rather than a second turn being queued behind it.
func (s *session) Send(ctx context.Context, input string) (retErr error) {
	defer func() { retErr = s.redactError(retErr) }()
	select {
	case <-s.done:
		return adapter.ErrSessionClosed
	default:
	}
	s.mu.Lock()
	threadID, turnID := s.threadID, s.turnID
	s.mu.Unlock()
	if turnID == "" {
		return fmt.Errorf("codex: no turn is running")
	}
	return s.conn.call(ctx, methodTurnSteer, turnSteerParams{
		ThreadID: threadID, ExpectedTurnID: turnID, Input: textInput(input),
	}, nil)
}

// Respond implements [adapter.Session].
func (s *session) Respond(ctx context.Context, resp adapter.PermissionResponse) (retErr error) {
	defer func() { retErr = s.redactError(retErr) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.done:
		return adapter.ErrSessionClosed
	default:
	}
	if resp.ID == "" {
		return fmt.Errorf("codex: PermissionResponse has no ID")
	}
	s.mu.Lock()
	p, ok := s.pending[resp.ID]
	delete(s.pending, resp.ID)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("codex: no permission request %q is waiting", resp.ID)
	}
	s.answerApproval(p.id, p.method, p.profile, resp.Allow)
	if !resp.Allow && resp.Reason != "" {
		// The decision shapes carry no reason field, so the only way the agent
		// learns why is to be told. An agent that does not know why retries the
		// same thing (docs/adapters.md).
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = s.Send(ctx, "The previous request was denied. Reason: "+resp.Reason)
		}()
	}
	return nil
}

// Interrupt implements [adapter.Session]. It asks Codex to stop the turn — its
// own interrupt — and only escalates to killing the process if the app server
// does not report the turn ending.
func (s *session) Interrupt(ctx context.Context) (retErr error) {
	defer func() { retErr = s.redactError(retErr) }()
	select {
	case <-s.done:
		return nil
	default:
	}
	s.mu.Lock()
	s.interrupted = true
	threadID, turnID := s.threadID, s.turnID
	s.mu.Unlock()

	var err error
	if turnID != "" {
		err = s.conn.call(ctx, methodTurnInterrupt, turnInterruptParams{
			ThreadID: threadID, TurnID: turnID,
		}, nil)
	}

	go func() {
		select {
		case <-s.done:
		case <-time.After(15 * time.Second):
			s.proc.kill()
		}
	}()
	return err
}

// Wait implements [adapter.Session]. Idempotent.
func (s *session) Wait() (adapter.Result, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.err
}

// -------------------------------------------------------------- finishing ---

// finishTurn ends the session on `turn/completed`, the normal exit.
func (s *session) finishTurn(t turn) {
	s.mu.Lock()
	status := adapter.StatusSucceeded
	var err error
	switch t.Status {
	case turnCompleted:
	case turnInterrupt:
		status = adapter.StatusInterrupted
	case turnFailed:
		status = adapter.StatusFailed
		msg := "codex: the turn failed"
		if t.Error != nil && t.Error.Message != "" {
			msg = "codex: " + t.Error.Message
		}
		err = fmt.Errorf("%s", msg)
	default:
		status = adapter.StatusFailed
		err = fmt.Errorf("codex: turn ended in state %q", t.Status)
	}
	if s.interrupted && status == adapter.StatusSucceeded {
		status = adapter.StatusInterrupted
	}
	summary := s.finalText
	if summary == "" {
		summary = s.lastText
	}
	if summary == "" {
		summary = summaryFromTurn(t)
	}
	s.mu.Unlock()

	s.finish(status, summary, err)
}

// finishProcessExit ends a session whose app server died before its turn did.
func (s *session) finishProcessExit(waitErr error) {
	select {
	case <-s.done:
		return
	default:
	}
	msg := "codex: `codex app-server` exited before the turn completed"
	if waitErr != nil {
		msg = fmt.Sprintf("codex: `codex app-server` exited: %v", waitErr)
	}
	if tail := s.proc.stderrTail.String(); tail != "" {
		msg += "\napp-server stderr:\n" + tail
	}
	s.mu.Lock()
	interrupted := s.interrupted
	s.mu.Unlock()
	status := adapter.StatusFailed
	if interrupted {
		status = adapter.StatusInterrupted
	}
	s.finish(status, "", fmt.Errorf("%s", msg))
}

func (s *session) timeOut(total time.Duration) {
	select {
	case <-s.done:
		return
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = s.Interrupt(ctx)
	cancel()
	s.finish(adapter.StatusTimedOut, "",
		fmt.Errorf("codex: session exceeded its total timeout of %s", total))
}

// finish emits exactly one terminal event, closes the channel, and unblocks
// Wait — once, whichever of the three paths above gets here first.
func (s *session) finish(status adapter.ResultStatus, summary string, ferr error) {
	s.finishOnce.Do(func() {
		summary = s.redact(summary)
		ferr = s.redactError(ferr)

		s.mu.Lock()
		res := adapter.Result{
			Status:    status,
			Summary:   summary,
			SessionID: s.redact(s.threadID),
			Usage:     s.usage,
			Duration:  time.Since(s.started),
			// Codex names the rollout file relative to its own CODEX_HOME,
			// which for a Rein session is the private one about to be removed.
			// The file itself lives under the real home through the `sessions`
			// symlink, so the path only needs putting back.
			TranscriptPath: s.redact(s.proc.home.transcriptPath(s.transcriptPath)),
			ExitCode:       -1,
		}
		s.result = res
		s.err = ferr
		s.mu.Unlock()

		ev := adapter.Event{Kind: adapter.EventDone, Result: &res, Text: res.Summary}
		if ferr != nil {
			ev = adapter.Event{Kind: adapter.EventError, Err: ferr, Text: ferr.Error(), Result: &res}
		}

		// A rotated login has to be said out loud before the channel closes:
		// the private home will be left on disk holding a newer credential
		// than the real one, and nobody would think to look for it.
		if warning := s.proc.home.rotatedLogin(); warning != "" {
			s.emit(adapter.Event{Kind: adapter.EventProgress, Text: warning})
		}

		s.emitMu.Lock()
		if !s.eventsClosed {
			if !s.opened {
				for _, buffered := range s.buf {
					s.events <- buffered
				}
				s.buf = nil
				s.opened = true
			}
			s.seq++
			ev.Seq = s.seq
			ev.At = time.Now()
			s.events <- ev
			s.eventsClosed = true
			close(s.events)
		}
		s.emitMu.Unlock()

		close(s.done)

		// Stopping stdin is how the app server is told the client is done; the
		// kill is the backstop for one that does not take the hint.
		_ = s.conn.close()
		go func() {
			select {
			case <-time.After(10 * time.Second):
				s.proc.kill()
			}
		}()

		// The exit code is only knowable after the process has gone, and Wait
		// has already been unblocked, so record it for a later Wait rather
		// than holding the first one up for a process that may linger.
		go func() {
			_ = s.proc.wait()
			s.mu.Lock()
			s.result.ExitCode = s.proc.exitCode()
			s.mu.Unlock()
			// Only once the process is gone: it writes its rollout and its
			// history right up to exit.
			s.proc.home.remove()
		}()
	})
}

// summaryFromTurn falls back to the last agent message carried on the
// completed turn, for a session whose item notifications were missed.
func summaryFromTurn(t turn) string {
	for i := len(t.Items) - 1; i >= 0; i-- {
		if t.Items[i].Type == itemAgentMessage && t.Items[i].Text != "" {
			return t.Items[i].Text
		}
	}
	return ""
}

func approvalSummary(what, reason string) string {
	switch {
	case what != "" && reason != "":
		return what + " — " + reason
	case what != "":
		return what
	case reason != "":
		return reason
	}
	return "an approval was requested"
}

func orString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// jsonObject builds a small JSON object from alternating key/value strings. It
// exists so a ToolUse.Input is always valid JSON even when the vendor gave the
// adapter loose fields rather than an argument blob.
func jsonObject(kv ...string) json.RawMessage {
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			m[kv[i]] = kv[i+1]
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}
