package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// session is one `grok agent stdio` process running one ACP prompt turn.
//
// The turn *is* the `session/prompt` call: it is a request that does not
// return until the agent has finished, and its result carries both the stop
// reason and the turn's token accounting. So unlike the Codex adapter, whose
// terminal signal is a notification, this one's terminal signal is a reply —
// which is why the prompt is issued on its own goroutine and never from the
// read loop.
type session struct {
	a       *Adapter
	spec    adapter.RunSpec
	proc    *process
	conn    *conn
	mode    permissionMode
	servers []any
	started time.Time

	events chan adapter.Event
	done   chan struct{}

	// emitMu serialises everything reaching the event channel: sequence
	// numbering, the pre-Start buffer, the close.
	emitMu       sync.Mutex
	seq          uint64
	buf          []adapter.Event
	opened       bool
	eventsClosed bool
	lastEmit     time.Time
	chunkTail    strings.Builder

	finishOnce sync.Once

	mu sync.Mutex
	// replaying suppresses the history session/load replays before it
	// returns. See loadSession.
	replaying   bool
	replayed    int
	sessionID   string
	model       string
	message     strings.Builder // the agent's message text for this turn
	lastSummary string
	usage       adapter.Usage
	pending     map[string]json.RawMessage
	interrupted bool
	result      adapter.Result
	err         error

	// rateLimit is set when the turn ended on a rate limit, and nil
	// otherwise — Grok says nothing about its window at any other time.
	rateLimit *adapter.RateLimit

	// stopFailures is every StopFailure report the turn left behind — kept
	// so the integration test can prove the hook fires under ACP at all.
	stopFailures []stopFailure
}

// Telemetry implements [adapter.Telemeter]: the model, and whether the turn
// ended on a rate limit. Nothing else — Grok reports no context fill and no
// window, and a guessed one would be worse than none.
func (s *session) Telemetry() adapter.SessionTelemetry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return adapter.SessionTelemetry{Model: s.model, RateLimit: s.rateLimit}
}

// settleRateLimit decides, as the turn ends, whether it ended on a rate limit
// (ratelimit.go).
//
// With Rein's StopFailure hook in the home, it first lets Grok finish its
// turn-end reporting: stdin is closed, which is how the agent is told the
// client is done, and the process gets [hookSettle] to exit — Grok flushes
// queued turn-end hooks at teardown. finish would close stdin a moment later
// anyway; doing it first, and waiting, is what makes the report readable
// before the terminal event goes out, which is when the run loop reads it.
func (s *session) settleRateLimit(ferr error) {
	var fails []stopFailure
	if h := s.proc.home; h != nil && h.stopFailureLog != "" {
		_ = s.conn.close()
		exited := make(chan struct{})
		go func() { _ = s.proc.wait(); close(exited) }()
		select {
		case <-exited:
		case <-time.After(hookSettle):
		}
		fails = readStopFailures(h.stopFailureLog)
	}
	errText := ""
	if ferr != nil {
		errText = ferr.Error()
	}
	rl := rateLimitFrom(fails, errText, time.Now())
	s.mu.Lock()
	s.stopFailures = fails
	if rl != nil {
		s.rateLimit = rl
	}
	s.mu.Unlock()
}

func newSession(a *Adapter, spec adapter.RunSpec, proc *process, mode permissionMode, servers []any) *session {
	return &session{
		a:       a,
		spec:    spec,
		proc:    proc,
		conn:    newConn(proc.stdout, proc.stdin),
		mode:    mode,
		servers: servers,
		started: time.Now(),
		events:  make(chan adapter.Event),
		done:    make(chan struct{}),
		pending: map[string]json.RawMessage{},
	}
}

// handshake gets to a live session: initialize, then either a new session or a
// loaded one. The prompt is *not* sent here — it runs for the whole turn, and
// Start must return once the session is running.
func (s *session) handshake(ctx context.Context) error {
	go func() { _ = s.conn.serve(s) }()

	var initRes initializeResponse
	if err := s.conn.call(ctx, methodInitialize, initializeParams{
		ProtocolVersion: protocolVersion,
		// Rein offers the agent neither filesystem tool. An adapter that
		// offered them would have to police the paths itself, and the sandbox
		// mapping exists precisely so the OS does that instead.
		ClientCapabilities: clientCapabilities{
			FS:       fsCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
	}, &initRes); err != nil {
		return s.startupError("initialize", err)
	}
	if initRes.ProtocolVersion != protocolVersion {
		return fmt.Errorf("grok: agent negotiated ACP version %d; this adapter speaks %d",
			initRes.ProtocolVersion, protocolVersion)
	}
	// The plan-login check, before any session exists (planauth.go).
	if err := checkPlanAuthMethod(initRes.Meta.DefaultAuthMethodID); err != nil {
		return err
	}

	// Grok's own documentation says a built-in sandbox profile that fails to
	// apply warns and carries on unprotected. Reading and continuing would
	// turn read_only into read-write silently.
	if s.mode.sandboxProfile != "" {
		if line, bad := s.proc.stderrTail.sandboxFailed(); bad {
			return fmt.Errorf("grok: the %q sandbox did not apply, so %s cannot be enforced: %s",
				s.mode.sandboxProfile, s.spec.PermissionMode, line)
		}
	}

	if s.spec.ResumeID != "" {
		if !initRes.AgentCapabilities.LoadSession {
			return fmt.Errorf("%w: this Grok build does not advertise loadSession, so a resume cannot be honoured",
				adapter.ErrNotSupported)
		}
		return s.loadSession(ctx)
	}

	meta := &sessionNewMeta{YoloMode: s.mode.yolo, Rules: s.spec.SystemPrompt}
	var res sessionNewResponse
	if err := s.conn.call(ctx, methodSessionNew, sessionNewParams{
		Cwd: s.spec.WorktreeDir, MCPServers: s.servers, Meta: meta,
	}, &res); err != nil {
		return s.startupError("session/new", err)
	}
	if res.SessionID == "" {
		return fmt.Errorf("grok: session/new returned no session id")
	}
	s.mu.Lock()
	s.sessionID = res.SessionID
	s.model = res.Models.CurrentModelID
	s.mu.Unlock()
	return nil
}

// loadSession resumes by id.
//
// **`session/load` replays the entire prior conversation as `session/update`
// notifications before it returns.** That is ACP working as specified, and it
// is a trap: replayed history is not news. Emitting it would re-report every
// message the run loop already saw on the earlier session, and would leave the
// replayed text in the buffer that becomes this turn's summary. So the
// translator is muted for the duration of the call and reports one progress
// event saying how much it skipped.
func (s *session) loadSession(ctx context.Context) error {
	s.mu.Lock()
	s.sessionID = s.spec.ResumeID
	s.replaying = true
	s.mu.Unlock()

	err := s.conn.call(ctx, methodSessionLoad, sessionLoadParams{
		SessionID: s.spec.ResumeID, Cwd: s.spec.WorktreeDir, MCPServers: s.servers,
	}, nil)

	s.mu.Lock()
	s.replaying = false
	replayed := s.replayed
	s.message.Reset()
	s.mu.Unlock()

	if err != nil {
		return s.startupError("session/load "+s.spec.ResumeID, err)
	}
	s.emit(adapter.Event{Kind: adapter.EventProgress,
		Text: fmt.Sprintf("resumed session %s; skipped %d replayed history updates",
			s.spec.ResumeID, replayed)})
	return nil
}

func (s *session) startupError(stage string, err error) error {
	if tail := s.proc.stderrTail.String(); tail != "" {
		return fmt.Errorf("grok: %s failed: %w; agent stderr:\n%s", stage, err, tail)
	}
	return fmt.Errorf("grok: %s failed: %w", stage, err)
}

func (s *session) abort() {
	_ = s.conn.close()
	s.proc.kill()
	go func() {
		_ = s.proc.wait()
		s.proc.home.remove()
	}()
}

// run opens the event channel, sends the prompt, and watches the process and
// the clock.
func (s *session) run() {
	go s.openEvents()

	go s.prompt()

	go func() {
		err := s.proc.wait()
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

// prompt runs the turn. The call returns when the agent is finished, which is
// this session's terminal event.
func (s *session) prompt() {
	text := s.spec.Prompt
	if text == "" {
		text = "Continue the work already in progress in this session."
	}
	s.mu.Lock()
	sid := s.sessionID
	s.mu.Unlock()

	var res sessionPromptResponse
	err := s.conn.call(context.Background(), methodSessionPrompt, sessionPromptParams{
		SessionID: sid, Prompt: textPrompt(text),
	}, &res)
	if err != nil {
		s.mu.Lock()
		interrupted := s.interrupted
		s.mu.Unlock()
		status := adapter.StatusFailed
		if interrupted {
			status = adapter.StatusInterrupted
		}
		s.finish(status, s.summary(), s.startupError("session/prompt", err))
		return
	}
	s.finishTurn(res)
}

// ---------------------------------------------------------------- events ---

func (s *session) emit(ev adapter.Event) {
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
	s.chunkTail.Reset()
	if !s.opened {
		s.buf = append(s.buf, ev)
		return
	}
	s.events <- ev
}

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

// heartbeat coalesces a chunk stream into at most one progress event per
// [Adapter.heartbeat]. Grok streams a notification per token, for both the
// message and the reasoning; forwarding them one for one would make Seq
// meaningless and give a run loop thousands of events per turn.
func (s *session) heartbeat(chunk string) {
	s.emitMu.Lock()
	if s.eventsClosed {
		s.emitMu.Unlock()
		return
	}
	s.chunkTail.WriteString(chunk)
	if s.a.heartbeat <= 0 || time.Since(s.lastEmit) < s.a.heartbeat {
		s.emitMu.Unlock()
		return
	}
	text := s.chunkTail.String()
	s.emitMu.Unlock()
	s.emit(adapter.Event{Kind: adapter.EventProgress, Text: text})
}

// ------------------------------------------------------- handler plumbing ---

func (s *session) onNotification(method string, params json.RawMessage) {
	// Everything session/load replays before it returns is history, not news.
	s.mu.Lock()
	if s.replaying {
		s.replayed++
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	switch method {
	case notifySessionUpdate, notifyXSessionNotification:
		var n sessionUpdateNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		s.onUpdate(n.Update, params)

	case notifyXSessionsChanged:
		var n sessionsChangedNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		s.mu.Lock()
		sid := s.sessionID
		s.mu.Unlock()
		for _, u := range n.Upserted {
			if u.SessionID == sid && u.Activity == "idle" {
				// The agent's own account of what it is doing — the honest
				// idle signal, not a spinner scraped from a terminal
				// (docs/adapters.md).
				s.emit(adapter.Event{Kind: adapter.EventIdle, Text: "session idle", Raw: params})
			}
		}

	case notifyXPromptComplete:
		// The prompt's own reply is the terminal signal; this is the same news
		// arriving early, and useful only as progress.
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "prompt complete", Raw: params})

	default:
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: method, Raw: params})
	}
}

func (s *session) onUpdate(u updatePayload, raw json.RawMessage) {
	switch u.SessionUpdate {
	case updAgentMessageChunk:
		if u.Content != nil {
			s.mu.Lock()
			s.message.WriteString(u.Content.Text)
			s.mu.Unlock()
			s.heartbeat(u.Content.Text)
		}

	case updAgentThoughtChunk:
		if u.Content != nil {
			s.heartbeat(u.Content.Text)
		}

	case updUserMessageChunk:
		// Rein's own prompt, echoed back. Relaying it would put the packet in
		// the transcript twice.

	case updToolCall:
		s.emit(adapter.Event{Kind: adapter.EventToolUse, Text: u.Title, Raw: raw,
			Tool: &adapter.ToolUse{ID: u.ToolCallID, Name: u.Title, Input: u.RawInput}})

	case updToolCallUpdate:
		if u.Status == "" {
			return // an in-flight refinement of the same call; the start already said it
		}
		s.emit(adapter.Event{Kind: adapter.EventProgress,
			Text: fmt.Sprintf("tool %s: %s", orString(u.Title, u.ToolCallID), u.Status), Raw: raw})

	case updPlan:
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "plan updated", Raw: raw})

	case updResponseCompleted:
		// One model call finished. Flush what it said as text, and report its
		// tokens as an increment.
		s.flushMessage(raw)
		if u.Usage != nil {
			s.onUsage(*u.Usage, raw)
		}

	case updTurnCompleted:
		s.emit(adapter.Event{Kind: adapter.EventProgress,
			Text: "turn completed: " + orString(u.StopReason, "(no stop reason)"), Raw: raw})

	case updLastTurnSummary:
		s.mu.Lock()
		s.lastSummary = u.Summary
		s.mu.Unlock()
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: u.Summary, Raw: raw})

	case updPendingInteract:
		// Grok announcing that a tool call is sitting at a permission gate.
		// **This is the one to watch**: on every probe so far the gate was
		// resolved inside the agent, without a session/request_permission ever
		// reaching Rein, which is why approvals is declared `partial`. If this
		// event is ever followed by a real request, the declaration can be
		// promoted. See docs/adapter-grok.md.
		s.emit(adapter.Event{Kind: adapter.EventProgress,
			Text: "waiting on a " + orString(u.Kind, "permission") + " gate inside the agent", Raw: raw})

	case updInteractResolved:
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "gate resolved by the agent", Raw: raw})

	case updModelChanged:
		if u.ModelID != "" {
			s.mu.Lock()
			s.model = u.ModelID
			s.mu.Unlock()
		}
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "model: " + u.ModelID, Raw: raw})

	case updAvailableCommands, updHookExecution, updSessionInfo, updSessionSummary, updCurrentMode:
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: u.SessionUpdate, Raw: raw})

	default:
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: u.SessionUpdate, Raw: raw})
	}
}

// flushMessage turns the accumulated chunks into one text event, the way a
// completed message would arrive from an adapter whose vendor sends whole
// messages.
func (s *session) flushMessage(raw json.RawMessage) {
	s.mu.Lock()
	text := s.message.String()
	s.message.Reset()
	if text != "" {
		s.lastSummary = text
	}
	s.mu.Unlock()
	if strings.TrimSpace(text) == "" {
		return
	}
	s.emit(adapter.Event{Kind: adapter.EventText, Text: text, Raw: raw})
}

// onUsage reports one model call's tokens as an increment.
//
// The normalisation is the interesting part. Grok's per-response
// `input_tokens` **excludes** the cached read, while the turn total includes
// it; summing the raw field would under-report a cached turn by an order of
// magnitude. Adding the cached read back in makes the increments sum to the
// turn total exactly, and matches how every other adapter here reports input
// tokens (Codex's `inputTokens` includes its `cachedInputTokens` too), so a
// consumer comparing two agents is comparing the same quantity.
func (s *session) onUsage(u responseUsage, raw json.RawMessage) {
	s.mu.Lock()
	inc := adapter.Usage{
		InputTokens:      u.InputTokens + u.CacheReadInputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputToken,
		Model:            s.model,
	}
	s.usage.Add(inc)
	s.mu.Unlock()
	if inc == (adapter.Usage{Model: inc.Model}) {
		return
	}
	s.emit(adapter.Event{Kind: adapter.EventUsage, Usage: &inc, Raw: raw})
}

// onRequest implements [handler]. Every agent request is answered, including
// the refusals: an unanswered one leaves the turn waiting forever.
func (s *session) onRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case reqRequestPermission:
		var p requestPermissionParams
		_ = json.Unmarshal(params, &p)
		s.permissionRequested(id, p, params)
	default:
		s.emit(adapter.Event{Kind: adapter.EventProgress,
			Text: "declined an unsupported agent request: " + method, Raw: params})
		_ = s.conn.replyError(id, codeMethodNotFound, "rein: "+method+" is not implemented by this client")
	}
}

// permissionRequested handles ACP's `session/request_permission`.
//
// **Written to the specification, never observed.** In four probes on
// 2026-08-29 Grok resolved every permission gate internally rather than asking
// the client, which is why the manifest says `approvals: partial` — and why
// `ask` and `accept_edits` never reach this adapter at all. What survives is
// the branch below for the modes that do: both set yoloMode, so a request
// arriving anyway is answered from the mode rather than left to hang.
func (s *session) permissionRequested(id json.RawMessage, p requestPermissionParams, raw json.RawMessage) {
	title := "a tool call"
	toolName := "unknown"
	var input json.RawMessage
	if p.ToolCall != nil {
		title = orString(p.ToolCall.Title, title)
		toolName = orString(p.ToolCall.Title, toolName)
		input = p.ToolCall.RawInput
	}

	if !s.spec.PermissionMode.NeedsApprovals() {
		allow := s.spec.PermissionMode == adapter.PermissionFull ||
			s.spec.PermissionMode == adapter.PermissionReadOnly
		// read_only allows too, and the reason is the sandbox: the kernel has
		// already refused the writes, so the approval decides nothing, and a
		// denial here would only make the agent retry.
		s.answerPermission(id, p.Options, allow)
		verb := "denied"
		if allow {
			verb = "auto-approved"
		}
		s.emit(adapter.Event{Kind: adapter.EventProgress,
			Text: fmt.Sprintf("%s %s (permission mode %s)", verb, title, s.spec.PermissionMode), Raw: raw})
		return
	}

	reqID := string(id)
	s.mu.Lock()
	s.pending[reqID] = mustJSON(p.Options)
	s.mu.Unlock()
	s.emit(adapter.Event{Kind: adapter.EventPermissionRequest, Text: title, Raw: raw,
		Permission: &adapter.PermissionRequest{ID: reqID, Tool: toolName, Summary: title, Input: input}})
}

func (s *session) answerPermission(id json.RawMessage, options []permissionOption, allow bool) {
	want := []string{optRejectOnce, optRejectAlways}
	if allow {
		want = []string{optAllowOnce, optAllowAlways}
	}
	if opt, ok := pickOption(options, want...); ok {
		_ = s.conn.reply(id, requestPermissionResponse{
			Outcome: permissionOutcome{Outcome: outcomeSelected, OptionID: opt.OptionID}})
		return
	}
	// No option of the kind we wanted. Cancelling is ACP's way of saying "no
	// choice was made", and it is the safe answer in both directions: it never
	// grants something by accident.
	_ = s.conn.reply(id, requestPermissionResponse{Outcome: permissionOutcome{Outcome: outcomeCancelled}})
}

// ------------------------------------------------------------- interface ---

// ID implements [adapter.Session]. The Grok session id, available as soon as
// Start returns.
func (s *session) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Events implements [adapter.Session].
func (s *session) Events() <-chan adapter.Event { return s.events }

// Send implements [adapter.Session] by refusing.
//
// ACP has no mid-turn steer: a second `session/prompt` is *queued* as another
// turn, which is a different thing from answering the turn in flight, and
// would give this session two terminal signals. The contract's provision for
// exactly this is [adapter.ErrNotSupported] — an adapter that cannot take
// input after start says so, and does not declare a multi-turn channel.
func (s *session) Send(ctx context.Context, input string) error {
	select {
	case <-s.done:
		return adapter.ErrSessionClosed
	default:
	}
	return fmt.Errorf("%w: ACP has no mid-turn steer; a follow-up is a new session against the same session id",
		adapter.ErrNotSupported)
}

// Respond implements [adapter.Session].
func (s *session) Respond(ctx context.Context, resp adapter.PermissionResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.done:
		return adapter.ErrSessionClosed
	default:
	}
	if resp.ID == "" {
		return fmt.Errorf("grok: PermissionResponse has no ID")
	}
	s.mu.Lock()
	rawOptions, ok := s.pending[resp.ID]
	delete(s.pending, resp.ID)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("grok: no permission request %q is waiting", resp.ID)
	}
	var options []permissionOption
	_ = json.Unmarshal(rawOptions, &options)
	s.answerPermission(json.RawMessage(resp.ID), options, resp.Allow)
	return nil
}

// Interrupt implements [adapter.Session]. `session/cancel` is ACP's own
// interrupt and a notification, not a request: the acknowledgement is the
// prompt call returning with `stopReason: "cancelled"`. Killing the process is
// the backstop for an agent that does not.
func (s *session) Interrupt(ctx context.Context) error {
	select {
	case <-s.done:
		return nil
	default:
	}
	s.mu.Lock()
	s.interrupted = true
	sid := s.sessionID
	s.mu.Unlock()

	var err error
	if sid != "" {
		err = s.conn.notify(methodSessionCancel, sessionCancelParams{SessionID: sid})
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

func (s *session) finishTurn(res sessionPromptResponse) {
	// Anything the agent said after the last response_completed is still in
	// the buffer.
	s.flushMessage(nil)

	if res.Meta.ModelID != "" {
		s.mu.Lock()
		s.model = res.Meta.ModelID
		s.mu.Unlock()
	}
	// The turn total is authoritative where it exists — the increments are
	// reconstructed from per-response payloads and the vendor's own figure is
	// not.
	if u := res.Meta.Usage; u != nil {
		s.mu.Lock()
		s.usage = adapter.Usage{
			InputTokens:      u.InputTokens,
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  u.CachedReadTokens,
			CacheWriteTokens: u.CacheCreationToks,
			Model:            s.model,
		}
		s.mu.Unlock()
	}

	status := adapter.StatusSucceeded
	var err error
	switch res.StopReason {
	case stopEndTurn:
	case stopCancelled:
		status = adapter.StatusInterrupted
	case stopMaxTokens:
		status = adapter.StatusFailed
		err = fmt.Errorf("grok: the turn stopped at the token limit")
	case stopMaxTurnRequests:
		status = adapter.StatusFailed
		err = fmt.Errorf("grok: the turn stopped at the request limit")
	case stopRefusal:
		status = adapter.StatusFailed
		err = fmt.Errorf("grok: the agent refused the request")
	case "":
		status = adapter.StatusFailed
		err = fmt.Errorf("grok: the turn ended with no stop reason")
	default:
		status = adapter.StatusFailed
		err = fmt.Errorf("grok: the turn ended with stop reason %q", res.StopReason)
	}
	s.mu.Lock()
	if s.interrupted && status == adapter.StatusSucceeded {
		status = adapter.StatusInterrupted
	}
	s.mu.Unlock()

	s.finish(status, s.summary(), err)
}

func (s *session) finishProcessExit(waitErr error) {
	select {
	case <-s.done:
		return
	default:
	}
	msg := "grok: `grok agent stdio` exited before the turn completed"
	if waitErr != nil {
		msg = fmt.Sprintf("grok: `grok agent stdio` exited: %v", waitErr)
	}
	if tail := s.proc.stderrTail.String(); tail != "" {
		msg += "\nagent stderr:\n" + tail
	}
	s.mu.Lock()
	interrupted := s.interrupted
	s.mu.Unlock()
	status := adapter.StatusFailed
	if interrupted {
		status = adapter.StatusInterrupted
	}
	s.finish(status, s.summary(), fmt.Errorf("%s", msg))
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
	s.finish(adapter.StatusTimedOut, s.summary(),
		fmt.Errorf("grok: session exceeded its total timeout of %s", total))
}

// summary is the agent's own account of the turn: the text it produced, or —
// when it produced none — the one-line summary Grok generates for itself.
func (s *session) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := strings.TrimSpace(s.message.String()); t != "" {
		return t
	}
	return s.lastSummary
}

// finish emits exactly one terminal event, closes the channel and unblocks
// Wait — once, whichever path reaches it first.
func (s *session) finish(status adapter.ResultStatus, summary string, ferr error) {
	s.finishOnce.Do(func() {
		s.settleRateLimit(ferr)

		s.mu.Lock()
		res := adapter.Result{
			Status:    status,
			Summary:   summary,
			SessionID: s.sessionID,
			Usage:     s.usage,
			Duration:  time.Since(s.started),
			ExitCode:  -1,
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

		_ = s.conn.close()
		go func() {
			<-time.After(10 * time.Second)
			s.proc.kill()
		}()
		go func() {
			_ = s.proc.wait()
			s.mu.Lock()
			s.result.ExitCode = s.proc.exitCode()
			s.mu.Unlock()
			// Only once the process is gone: it writes its session state right
			// up to exit, and session/load needs that.
			s.proc.home.remove()
		}()
	})
}

func orString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
