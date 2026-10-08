package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// A fake `codex app-server` that replays a transcript recorded from the real
// one.
//
// The fixtures in testdata/ are verbatim recordings — every frame the binary
// sent, in the order it sent them, with only this machine's paths and one
// bearer token replaced (see testdata/README.md). The replay engine derives
// its script from the recording rather than from a hand-written table, so a
// test asserts against what Codex actually said. A hand-written script would
// drift the moment the protocol did, and drift silently, which is the failure
// this whole package exists to avoid.

type recordedFrame struct {
	Dir   string          `json:"dir"`
	Frame json.RawMessage `json:"frame"`
}

// opKind is what the peer does at one step of the script.
type opKind int

const (
	// opReply answers the next client request naming opStep.method with a
	// recorded result.
	opReply opKind = iota
	// opNotify pushes a recorded notification.
	opNotify
	// opRequest pushes a recorded server request and blocks until the client
	// answers it.
	opRequest
)

type opStep struct {
	kind    opKind
	method  string
	params  json.RawMessage
	result  json.RawMessage
	rpcErr  json.RawMessage
	wireID  json.RawMessage
	rawLine string
}

// loadScript turns a recording into the peer's script. A recorded inbound
// frame is a reply, a notification or a request depending on which members it
// carries — the same discrimination the real client makes, which is the point:
// if the rule were wrong, these tests would replay wrongly too.
func loadScript(t *testing.T, path string) []opStep {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer f.Close()

	methodByID := map[string]string{}
	var script []opStep
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec recordedFrame
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("fixture line: %v", err)
		}
		var m wireMessage
		if err := json.Unmarshal(rec.Frame, &m); err != nil {
			t.Fatalf("fixture frame: %v", err)
		}
		if rec.Dir == "out" {
			if m.Method != "" && len(m.ID) > 0 {
				methodByID[string(m.ID)] = m.Method
			}
			continue
		}
		switch {
		case m.isRequest():
			script = append(script, opStep{kind: opRequest, method: m.Method,
				params: m.Params, wireID: m.ID, rawLine: line})
		case m.isNotification():
			script = append(script, opStep{kind: opNotify, method: m.Method,
				params: m.Params, rawLine: line})
		default:
			method, ok := methodByID[string(m.ID)]
			if !ok {
				t.Fatalf("fixture has a response to id %s with no recorded request", m.ID)
			}
			var errRaw json.RawMessage
			if m.Error != nil {
				errRaw, _ = json.Marshal(m.Error)
			}
			script = append(script, opStep{kind: opReply, method: method,
				result: m.Result, rpcErr: errRaw, rawLine: line})
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	if len(script) == 0 {
		t.Fatalf("fixture %s produced an empty script", path)
	}
	return script
}

// peer is the in-process stand-in for `codex app-server`.
type peer struct {
	t      *testing.T
	script []opStep

	toPeer   *io.PipeReader // what the adapter wrote
	toClient *io.PipeWriter // what the peer writes

	dec *json.Decoder

	mu       sync.Mutex
	sent     []wireMessage // every frame the adapter sent
	answers  []wireMessage // the adapter's answers to server requests
	scriptOK bool
}

func newPeer(t *testing.T, script []opStep) (*peer, *process) {
	t.Helper()
	adapterStdinR, adapterStdinW := io.Pipe() // adapter writes → peer reads
	peerStdoutR, peerStdoutW := io.Pipe()     // peer writes → adapter reads

	p := &peer{t: t, script: script, toPeer: adapterStdinR, toClient: peerStdoutW,
		dec: json.NewDecoder(adapterStdinR)}

	// The process only ever "exits" when the session kills it. Returning from
	// wait early would race the translator into reporting a crash it did not
	// have.
	procDone := make(chan struct{})
	var killOnce sync.Once
	proc := &process{
		stdin:      adapterStdinW,
		stdout:     peerStdoutR,
		stderrTail: newTail(4),
		wait: func() error {
			<-procDone
			return nil
		},
		kill: func() {
			killOnce.Do(func() {
				close(procDone)
				_ = peerStdoutW.Close()
				_ = adapterStdinR.Close()
			})
		},
		exitCode: func() int { return 0 },
	}
	go p.run()
	t.Cleanup(func() { proc.kill() })
	return p, proc
}

func (p *peer) run() {
	defer func() { _ = p.toClient.Close() }()
	for _, op := range p.script {
		switch op.kind {
		case opReply:
			m, ok := p.awaitRequest(op.method)
			if !ok {
				return
			}
			frame := wireMessage{ID: m.ID, Result: op.result}
			if len(op.rpcErr) > 0 {
				_ = json.Unmarshal(op.rpcErr, &frame.Error)
				frame.Result = nil
			}
			if !p.write(frame) {
				return
			}
		case opNotify:
			if !p.write(wireMessage{Method: op.method, Params: op.params}) {
				return
			}
		case opRequest:
			if !p.write(wireMessage{Method: op.method, ID: op.wireID, Params: op.params}) {
				return
			}
			if !p.awaitAnswer(op.wireID) {
				return
			}
		}
	}
	p.mu.Lock()
	p.scriptOK = true
	p.mu.Unlock()
}

// awaitRequest reads client frames until one is a request naming method,
// dropping the client notifications in between.
func (p *peer) awaitRequest(method string) (wireMessage, bool) {
	for {
		m, ok := p.read()
		if !ok {
			return wireMessage{}, false
		}
		if m.Method == method && len(m.ID) > 0 {
			return m, true
		}
	}
}

// awaitAnswer reads client frames until one answers the server request id.
func (p *peer) awaitAnswer(id json.RawMessage) bool {
	for {
		m, ok := p.read()
		if !ok {
			return false
		}
		if m.Method == "" && string(m.ID) == string(id) {
			p.mu.Lock()
			p.answers = append(p.answers, m)
			p.mu.Unlock()
			return true
		}
	}
}

func (p *peer) read() (wireMessage, bool) {
	var m wireMessage
	if err := p.dec.Decode(&m); err != nil {
		return wireMessage{}, false
	}
	p.mu.Lock()
	p.sent = append(p.sent, m)
	p.mu.Unlock()
	return m, true
}

func (p *peer) write(m wireMessage) bool {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return false
	}
	if _, err := p.toClient.Write(append(b, '\n')); err != nil {
		return false
	}
	return true
}

// clientCall returns the first frame the adapter sent for a method.
func (p *peer) clientCall(method string) (wireMessage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.sent {
		if m.Method == method {
			return m, true
		}
	}
	return wireMessage{}, false
}

func (p *peer) approvalAnswers() []wireMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]wireMessage(nil), p.answers...)
}

// peerRef hands a test the peer the adapter will build when it starts. The
// peer cannot exist before Start, because Start is what spawns it; this is the
// handle the test holds in the meantime.
type peerRef struct {
	mu sync.Mutex
	p  *peer
}

func (r *peerRef) set(p *peer) {
	r.mu.Lock()
	r.p = p
	r.mu.Unlock()
}

func (r *peerRef) get(t *testing.T) *peer {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.p == nil {
		t.Fatal("no peer was spawned; Start refused the spec before reaching one")
	}
	return r.p
}

func (r *peerRef) clientCall(t *testing.T, method string) (wireMessage, bool) {
	return r.get(t).clientCall(method)
}

func (r *peerRef) approvalAnswers(t *testing.T) []wireMessage {
	return r.get(t).approvalAnswers()
}

func (r *peerRef) methodsSeen(t *testing.T) string { return r.get(t).methodsSeen() }

// replayAdapter wires an Adapter to a fixture instead of a real binary.
func replayAdapter(t *testing.T, fixture string) (*Adapter, *peerRef) {
	t.Helper()
	script := loadScript(t, "testdata/"+fixture)
	a := New()
	// A long heartbeat, so no coalesced delta event can land between the ones
	// a test is asserting on and shift every sequence number after it.
	a.heartbeat = time.Hour
	ref := &peerRef{}
	a.spawn = func(ctx context.Context, spec adapter.RunSpec) (*process, error) {
		p, proc := newPeer(t, script)
		ref.set(p)
		return proc, nil
	}
	return a, ref
}

// drain collects every event until the channel closes, with a bound so a
// wedged session fails the test instead of hanging the suite.
func drain(t *testing.T, sess adapter.Session) []adapter.Event {
	t.Helper()
	var events []adapter.Event
	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-deadline:
			t.Fatalf("timed out draining events after %d", len(events))
		}
	}
}

func kinds(events []adapter.Event) []adapter.EventKind {
	out := make([]adapter.EventKind, len(events))
	for i, ev := range events {
		out[i] = ev.Kind
	}
	return out
}

func firstOfKind(events []adapter.Event, k adapter.EventKind) (adapter.Event, bool) {
	for _, ev := range events {
		if ev.Kind == k {
			return ev, true
		}
	}
	return adapter.Event{}, false
}

func baseSpec(mode adapter.PermissionMode) adapter.RunSpec {
	return adapter.RunSpec{
		RunID:          "run-1",
		WorktreeDir:    "/tmp/rein-fixture/worktree",
		Prompt:         "Reply with exactly the word pong and nothing else.",
		SystemPrompt:   "You are running under Rein.",
		PermissionMode: mode,
	}
}

func TestReplayPongSession(t *testing.T) {
	a, _ := replayAdapter(t, "pong.jsonl")
	sess, err := a.Start(context.Background(), baseSpec(adapter.PermissionFull))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := sess.ID(); got == "" {
		t.Fatal("session has no id before it finishes; resume and attach both need one")
	}

	events := drain(t, sess)
	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// Exactly one terminal event, last, and the channel closed after it.
	terminal := 0
	for i, ev := range events {
		if ev.Kind.Terminal() {
			terminal++
			if i != len(events)-1 {
				t.Errorf("terminal event %s at %d of %d", ev.Kind, i, len(events)-1)
			}
		}
		if ev.Seq != uint64(i+1) {
			t.Errorf("event %d has Seq %d; sequence must count from 1 without gaps", i, ev.Seq)
		}
	}
	if terminal != 1 {
		t.Errorf("got %d terminal events, want exactly 1 (kinds: %v)", terminal, kinds(events))
	}

	if res.Status != adapter.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", res.Status)
	}
	if res.Summary != "pong" {
		t.Errorf("Summary = %q, want %q", res.Summary, "pong")
	}
	if res.SessionID != sess.ID() {
		t.Errorf("Result.SessionID = %q, session ID = %q; they must agree", res.SessionID, sess.ID())
	}
	if !strings.Contains(res.TranscriptPath, "rollout-") {
		t.Errorf("TranscriptPath = %q, want the rollout jsonl thread/started carried", res.TranscriptPath)
	}

	text, ok := firstOfKind(events, adapter.EventText)
	if !ok || text.Text != "pong" {
		t.Errorf("no text event carrying the agent's answer; kinds: %v", kinds(events))
	}
	usage, ok := firstOfKind(events, adapter.EventUsage)
	if !ok {
		t.Fatalf("no usage event; kinds: %v", kinds(events))
	}
	if usage.Usage.InputTokens != 19745 || usage.Usage.OutputTokens != 5 {
		t.Errorf("usage increment = %+v, want the recorded 19745 in / 5 out", *usage.Usage)
	}
	if usage.Usage.CacheReadTokens != 11008 {
		t.Errorf("cache read = %d, want 11008", usage.Usage.CacheReadTokens)
	}
	if res.Usage.InputTokens != 19745 {
		t.Errorf("Result.Usage = %+v, want the accumulated total", res.Usage)
	}
	if _, ok := firstOfKind(events, adapter.EventIdle); !ok {
		t.Errorf("no idle event; thread/status/changed idle is the watchdog's signal")
	}
	if _, ok := firstOfKind(events, adapter.EventPermissionRequest); ok {
		t.Errorf("a full-mode session raised a permission request; approvalPolicy never should prevent it")
	}

	// Wait is idempotent.
	res2, err2 := sess.Wait()
	if res2.Status != res.Status || err2 != nil {
		t.Errorf("second Wait disagreed: %+v / %v", res2, err2)
	}
}

func TestReplaySendsTheMappedPermissionFlags(t *testing.T) {
	a, p := replayAdapter(t, "pong.jsonl")
	spec := baseSpec(adapter.PermissionAcceptEdits)
	spec.Model = "gpt-5.1-codex"
	sess, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain(t, sess)

	call, ok := p.clientCall(t, methodThreadStart)
	if !ok {
		t.Fatal("the adapter never called thread/start")
	}
	var got threadStartParams
	if err := json.Unmarshal(call.Params, &got); err != nil {
		t.Fatalf("thread/start params: %v", err)
	}
	if got.Sandbox != sandboxWorkspaceWrite {
		t.Errorf("sandbox = %q, want workspace-write for accept_edits", got.Sandbox)
	}
	if got.ApprovalPolicy != approvalOnRequest {
		t.Errorf("approvalPolicy = %q, want on-request for accept_edits", got.ApprovalPolicy)
	}
	if got.Cwd != spec.WorktreeDir {
		t.Errorf("cwd = %q, want the worktree %q", got.Cwd, spec.WorktreeDir)
	}
	if got.Model != spec.Model {
		t.Errorf("model = %q, want it passed through verbatim", got.Model)
	}
	if got.DeveloperInstructions != spec.SystemPrompt {
		t.Errorf("developerInstructions = %q, want the system prompt", got.DeveloperInstructions)
	}
}

func TestReplayApprovalIsAnsweredByTheRunLoop(t *testing.T) {
	a, p := replayAdapter(t, "approval.jsonl")
	spec := baseSpec(adapter.PermissionAsk)
	spec.Prompt = "Run the shell command: echo ok > probe.txt"
	sess, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var events []adapter.Event
	var sawPermission bool
	deadline := time.After(30 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				break loop
			}
			events = append(events, ev)
			if ev.Kind == adapter.EventPermissionRequest {
				sawPermission = true
				if ev.Permission == nil || ev.Permission.ID == "" {
					t.Fatalf("permission event carries no request to answer: %+v", ev)
				}
				if ev.Permission.Tool != "shell" {
					t.Errorf("permission tool = %q, want shell", ev.Permission.Tool)
				}
				if !strings.Contains(ev.Permission.Summary, "probe.txt") {
					t.Errorf("permission summary %q does not name the command", ev.Permission.Summary)
				}
				if err := sess.Respond(context.Background(), adapter.PermissionResponse{
					ID: ev.Permission.ID, Allow: true,
				}); err != nil {
					t.Fatalf("Respond: %v", err)
				}
			}
		case <-deadline:
			t.Fatalf("timed out after %d events: %v", len(events), kinds(events))
		}
	}

	if !sawPermission {
		t.Fatalf("no permission request surfaced; kinds: %v", kinds(events))
	}
	answers := p.approvalAnswers(t)
	if len(answers) != 1 {
		t.Fatalf("peer received %d answers, want 1", len(answers))
	}
	var dec decisionResponse
	if err := json.Unmarshal(answers[0].Result, &dec); err != nil {
		t.Fatalf("answer was not a decision: %s", answers[0].Result)
	}
	if dec.Decision != decisionAccept {
		t.Errorf("decision = %q, want accept", dec.Decision)
	}

	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Status != adapter.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", res.Status)
	}
	if _, ok := firstOfKind(events, adapter.EventToolUse); !ok {
		t.Errorf("the shell command did not surface as a tool_use; kinds: %v", kinds(events))
	}
}

func TestReplayApprovalIsDecidedByTheModeWhenNobodyPromisedToAnswer(t *testing.T) {
	// read_only starts the thread with approvalPolicy `never`, so Codex should
	// never ask. If a future release asks anyway, the adapter must decide
	// rather than wait for an answer the run loop never agreed to give — a
	// hang is worse than either decision.
	a, p := replayAdapter(t, "approval.jsonl")
	sess, err := a.Start(context.Background(), baseSpec(adapter.PermissionReadOnly))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := drain(t, sess)
	if _, ok := firstOfKind(events, adapter.EventPermissionRequest); ok {
		t.Errorf("read_only surfaced a permission request it cannot have answered")
	}
	answers := p.approvalAnswers(t)
	if len(answers) != 1 {
		t.Fatalf("peer received %d answers, want 1", len(answers))
	}
	var dec decisionResponse
	if err := json.Unmarshal(answers[0].Result, &dec); err != nil {
		t.Fatalf("answer was not a decision: %s", answers[0].Result)
	}
	if dec.Decision != decisionDecline {
		t.Errorf("decision = %q, want decline in read_only", dec.Decision)
	}
}

func TestReplayFullModeAutoApproves(t *testing.T) {
	a, p := replayAdapter(t, "approval.jsonl")
	sess, err := a.Start(context.Background(), baseSpec(adapter.PermissionFull))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain(t, sess)
	answers := p.approvalAnswers(t)
	if len(answers) != 1 {
		t.Fatalf("peer received %d answers, want 1", len(answers))
	}
	var dec decisionResponse
	_ = json.Unmarshal(answers[0].Result, &dec)
	if dec.Decision != decisionAccept {
		t.Errorf("decision = %q, want accept in full", dec.Decision)
	}
}

func TestRespondRejectsAnIDNobodyIsWaitingOn(t *testing.T) {
	// A fixture with no approval in it: the point is what Respond does with an
	// id it never issued, not what a real approval does.
	a, _ := replayAdapter(t, "pong.jsonl")
	sess, err := a.Start(context.Background(), baseSpec(adapter.PermissionAsk))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer drain(t, sess)
	err = sess.Respond(context.Background(), adapter.PermissionResponse{ID: "nope", Allow: true})
	if err == nil {
		t.Error("Respond accepted an id that is not pending")
	}
	if err := sess.Respond(context.Background(), adapter.PermissionResponse{Allow: true}); err == nil {
		t.Error("Respond accepted a response with no id")
	}
}

func TestSendAfterTheSessionEnds(t *testing.T) {
	a, _ := replayAdapter(t, "pong.jsonl")
	sess, err := a.Start(context.Background(), baseSpec(adapter.PermissionFull))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain(t, sess)
	if _, err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := sess.Send(context.Background(), "hello?"); err != adapter.ErrSessionClosed {
		t.Errorf("Send after close = %v, want ErrSessionClosed", err)
	}
	if err := sess.Respond(context.Background(), adapter.PermissionResponse{ID: "1", Allow: true}); err != adapter.ErrSessionClosed {
		t.Errorf("Respond after close = %v, want ErrSessionClosed", err)
	}
}

func TestMCPServersReachThreadStart(t *testing.T) {
	a, p := replayAdapter(t, "pong.jsonl")
	spec := baseSpec(adapter.PermissionFull)
	spec.MCPServers = map[string]any{
		"elk": map[string]any{"command": "npx", "args": []any{"-y", "elk-mcp"}},
	}
	sess, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain(t, sess)

	call, _ := p.clientCall(t, methodThreadStart)
	var got map[string]any
	if err := json.Unmarshal(call.Params, &got); err != nil {
		t.Fatalf("thread/start params: %v", err)
	}
	cfg, ok := got["config"].(map[string]any)
	if !ok {
		t.Fatalf("thread/start carried no config overlay: %s", call.Params)
	}
	servers, ok := cfg["mcp_servers"].(map[string]any)
	if !ok || servers["elk"] == nil {
		t.Fatalf("mcp servers did not reach config.mcp_servers: %v", cfg)
	}
}

func TestResumeUsesThreadResume(t *testing.T) {
	// The pong recording answers thread/start; a resume asks a different
	// method, so the script never matches and the peer stalls. What is being
	// asserted is which method the adapter chose, which is visible either way.
	a, p := replayAdapter(t, "pong.jsonl")
	spec := baseSpec(adapter.PermissionFull)
	spec.ResumeID = "01a04fd8-a5d3-7121-b424-95d125bd49b1"
	spec.Timeouts.Startup = 2 * time.Second
	if _, err := a.Start(context.Background(), spec); err == nil {
		t.Fatal("expected the resume to fail against a fixture that only answers thread/start")
	}
	if _, ok := p.clientCall(t, methodThreadResume); !ok {
		t.Errorf("a spec with a ResumeID did not call thread/resume; frames: %s", p.methodsSeen(t))
	}
	if _, ok := p.clientCall(t, methodThreadStart); ok {
		t.Errorf("a spec with a ResumeID started a new thread instead of resuming")
	}
}

func (p *peer) methodsSeen() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, m := range p.sent {
		if m.Method != "" {
			out = append(out, m.Method)
		}
	}
	return fmt.Sprint(out)
}
