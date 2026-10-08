package grok

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

// A fake `grok agent stdio` that replays a transcript recorded from the real
// one.
//
// The fixtures in testdata/ are verbatim recordings (see testdata/README.md),
// and the replay script is *derived* from the recording rather than
// hand-written — so these tests assert against what Grok actually said, and a
// protocol change shows up as a replay that no longer matches rather than as a
// table nobody remembered to update.
//
// The ordering rule is what makes it faithful. Grok's terminal signal is the
// **reply** to `session/prompt`, and every session update arrives between that
// request and its answer. So each recorded frame remembers which client
// request preceded it, and the peer will not push a frame until the client has
// actually sent that request. Without it the whole turn would replay before
// the adapter had asked for anything.

type recordedFrame struct {
	Dir   string          `json:"dir"`
	Frame json.RawMessage `json:"frame"`
}

type opKind int

const (
	opReply opKind = iota
	opNotify
	opRequest
)

type opStep struct {
	kind   opKind
	method string
	// after is the client method that must have arrived before this frame is
	// pushed — the request that was in flight when the agent sent it.
	after  string
	params json.RawMessage
	result json.RawMessage
	rpcErr json.RawMessage
	wireID json.RawMessage
}

func loadScript(t *testing.T, path string) []opStep {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer f.Close()

	methodByID := map[string]string{}
	lastClientMethod := ""
	var script []opStep
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
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
			if m.Method != "" {
				lastClientMethod = m.Method
				if len(m.ID) > 0 {
					methodByID[string(m.ID)] = m.Method
				}
			}
			continue
		}
		switch {
		case m.isRequest():
			script = append(script, opStep{kind: opRequest, method: m.Method,
				after: lastClientMethod, params: m.Params, wireID: m.ID})
		case m.isNotification():
			script = append(script, opStep{kind: opNotify, method: m.Method,
				after: lastClientMethod, params: m.Params})
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
				after: method, result: m.Result, rpcErr: errRaw})
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

type peer struct {
	script []opStep

	toClient *io.PipeWriter
	dec      *json.Decoder

	// idByMethod remembers the id the client used for each request, so a
	// recorded reply can be re-addressed to the id this run actually chose.
	mu         sync.Mutex
	idByMethod map[string]json.RawMessage
	sent       []wireMessage
	answers    []wireMessage
}

func newPeer(t *testing.T, script []opStep) (*peer, *process) {
	t.Helper()
	agentStdinR, agentStdinW := io.Pipe() // adapter writes → peer reads
	peerStdoutR, peerStdoutW := io.Pipe() // peer writes → adapter reads

	p := &peer{script: script, toClient: peerStdoutW,
		dec: json.NewDecoder(agentStdinR), idByMethod: map[string]json.RawMessage{}}

	procDone := make(chan struct{})
	var killOnce sync.Once
	proc := &process{
		stdin:      agentStdinW,
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
				_ = agentStdinR.Close()
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
		if op.after != "" {
			if _, ok := p.awaitClientMethod(op.after); !ok {
				return
			}
		}
		switch op.kind {
		case opReply:
			id, ok := p.idFor(op.method)
			if !ok {
				return
			}
			frame := wireMessage{ID: id, Result: op.result}
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
}

// awaitClientMethod reads until the client has sent method, remembering the id
// it used.
func (p *peer) awaitClientMethod(method string) (json.RawMessage, bool) {
	if id, ok := p.idFor(method); ok {
		return id, true
	}
	for {
		m, ok := p.read()
		if !ok {
			return nil, false
		}
		if m.Method == method {
			return m.ID, true
		}
	}
}

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
	if m.Method != "" {
		if _, seen := p.idByMethod[m.Method]; !seen {
			p.idByMethod[m.Method] = m.ID
		}
	}
	p.mu.Unlock()
	return m, true
}

func (p *peer) idFor(method string) (json.RawMessage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.idByMethod[method]
	return id, ok
}

func (p *peer) write(m wireMessage) bool {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return false
	}
	_, err = p.toClient.Write(append(b, '\n'))
	return err == nil
}

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

// peerRef hands a test the peer the adapter builds when it starts.
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

func replayAdapter(t *testing.T, fixture string) (*Adapter, *peerRef) {
	t.Helper()
	script := loadScript(t, "testdata/"+fixture)
	a := New()
	// A long heartbeat: Grok streams one notification per token, and a
	// coalesced progress event landing mid-test would shift every sequence
	// number after it.
	a.heartbeat = time.Hour
	ref := &peerRef{}
	a.spawn = func(ctx context.Context, spec adapter.RunSpec) (*process, error) {
		p, proc := newPeer(t, script)
		ref.set(p)
		return proc, nil
	}
	return a, ref
}

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
			t.Fatalf("timed out draining events after %d: %v", len(events), kinds(events))
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

func countOfKind(events []adapter.Event, k adapter.EventKind) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == k {
			n++
		}
	}
	return n
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
	if sess.ID() == "" {
		t.Fatal("no session id before the session finished; resume and attach both need one")
	}

	events := drain(t, sess)
	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}

	terminal := 0
	for i, ev := range events {
		if ev.Kind.Terminal() {
			terminal++
			if i != len(events)-1 {
				t.Errorf("terminal event %s at %d of %d", ev.Kind, i, len(events)-1)
			}
		}
		if ev.Seq != uint64(i+1) {
			t.Errorf("event %d has Seq %d; the sequence must count from 1 without gaps", i, ev.Seq)
		}
	}
	if terminal != 1 {
		t.Fatalf("got %d terminal events, want exactly 1 (kinds: %v)", terminal, kinds(events))
	}

	if res.Status != adapter.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", res.Status)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "pong") {
		t.Errorf("Summary = %q, want it to contain the agent's answer", res.Summary)
	}
	if res.SessionID != sess.ID() {
		t.Errorf("Result.SessionID = %q, session ID = %q; they must agree", res.SessionID, sess.ID())
	}

	text, ok := firstOfKind(events, adapter.EventText)
	if !ok || !strings.Contains(strings.ToLower(text.Text), "pong") {
		t.Errorf("no text event carrying the agent's answer; kinds: %v", kinds(events))
	}
	if _, ok := firstOfKind(events, adapter.EventUsage); !ok {
		t.Errorf("no usage event; kinds: %v", kinds(events))
	}
	if res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
		t.Errorf("Result.Usage = %+v, want the turn total from the prompt reply", res.Usage)
	}
	if res.Usage.Model == "" {
		t.Errorf("usage carries no model; Elk prices by model")
	}
	if _, ok := firstOfKind(events, adapter.EventIdle); !ok {
		t.Errorf("no idle event; the agent's own activity flag is the watchdog's signal")
	}

	res2, err2 := sess.Wait()
	if res2.Status != res.Status || err2 != nil {
		t.Errorf("second Wait disagreed: %+v / %v", res2, err2)
	}
}

func TestReplaySessionNewCarriesTheModeAndTheSystemPrompt(t *testing.T) {
	a, ref := replayAdapter(t, "pong.jsonl")
	sess, err := a.Start(context.Background(), baseSpec(adapter.PermissionFull))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain(t, sess)

	call, ok := ref.get(t).clientCall(methodSessionNew)
	if !ok {
		t.Fatalf("the adapter never called session/new; methods: %s", ref.get(t).methodsSeen())
	}
	var got sessionNewParams
	if err := json.Unmarshal(call.Params, &got); err != nil {
		t.Fatalf("session/new params: %v", err)
	}
	if got.Cwd != "/tmp/rein-fixture/worktree" {
		t.Errorf("cwd = %q, want the worktree", got.Cwd)
	}
	if got.Meta == nil || !got.Meta.YoloMode {
		t.Errorf("_meta.yoloMode was not set for full; got %+v", got.Meta)
	}
	if got.Meta == nil || got.Meta.Rules != "You are running under Rein." {
		t.Errorf("_meta.rules = %q, want the system prompt", got.Meta.Rules)
	}
	if got.MCPServers == nil {
		t.Errorf("mcpServers must be present even when empty; ACP requires the member")
	}

	init, ok := ref.get(t).clientCall(methodInitialize)
	if !ok {
		t.Fatal("no initialize")
	}
	var ip initializeParams
	_ = json.Unmarshal(init.Params, &ip)
	if ip.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %d, want %d", ip.ProtocolVersion, protocolVersion)
	}
	if ip.ClientCapabilities.FS.WriteTextFile || ip.ClientCapabilities.FS.ReadTextFile {
		t.Errorf("Rein must not offer the agent filesystem tools: %+v", ip.ClientCapabilities)
	}
}

func TestReplayReadOnlySetsTheSandbox(t *testing.T) {
	a, ref := replayAdapter(t, "pong.jsonl")
	var gotEnv map[string]string
	inner := a.spawn
	a.spawn = func(ctx context.Context, spec adapter.RunSpec) (*process, error) {
		gotEnv = spec.Env
		return inner(ctx, spec)
	}
	sess, err := a.Start(context.Background(), baseSpec(adapter.PermissionReadOnly))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain(t, sess)

	if gotEnv[sandboxEnv] != sandboxReadOnly {
		t.Fatalf("%s = %q, want %q — read_only means nothing without the sandbox",
			sandboxEnv, gotEnv[sandboxEnv], sandboxReadOnly)
	}
	call, _ := ref.get(t).clientCall(methodSessionNew)
	var got sessionNewParams
	_ = json.Unmarshal(call.Params, &got)
	if got.Meta == nil || !got.Meta.YoloMode {
		// Deliberate: with approvals `partial` nobody has promised to answer a
		// request, and the sandbox is what makes the mode read-only.
		t.Errorf("read_only should still auto-approve inside the sandbox; got %+v", got.Meta)
	}
}

func TestReplayReadOnlyRefusesAnUnappliedSandbox(t *testing.T) {
	// Grok's own docs say a built-in profile that will not apply "warns and
	// continues without enforcement". Continuing would turn read_only into
	// read-write, silently.
	a, _ := replayAdapter(t, "pong.jsonl")
	inner := a.spawn
	a.spawn = func(ctx context.Context, spec adapter.RunSpec) (*process, error) {
		proc, err := inner(ctx, spec)
		if err != nil {
			return nil, err
		}
		proc.stderrTail.lines = []string{
			"warning: sandbox could not be applied: hook source path contains a symlink component",
		}
		return proc, nil
	}
	_, err := a.Start(context.Background(), baseSpec(adapter.PermissionReadOnly))
	if err == nil {
		t.Fatal("Start accepted read_only with a sandbox that did not apply")
	}
	if !strings.Contains(err.Error(), "sandbox") {
		t.Errorf("the error does not say what went wrong: %v", err)
	}
}

func TestReplayToolCallSession(t *testing.T) {
	a, _ := replayAdapter(t, "toolcall.jsonl")
	spec := baseSpec(adapter.PermissionFull)
	spec.Prompt = "Run this exact shell command and show me its output: curl …"
	sess, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := drain(t, sess)
	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Status != adapter.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", res.Status)
	}

	tool, ok := firstOfKind(events, adapter.EventToolUse)
	if !ok {
		t.Fatalf("no tool_use event; kinds: %v", kinds(events))
	}
	if tool.Tool == nil || tool.Tool.ID == "" {
		t.Errorf("tool_use carries no call id to correlate a result: %+v", tool)
	}
	if !strings.Contains(string(tool.Tool.Input), "curl") {
		t.Errorf("tool input does not carry the command: %s", tool.Tool.Input)
	}

	// The gate Grok resolved on its own is the evidence behind the `partial`
	// approvals declaration, so it has to reach the run loop as *something*.
	var sawGate bool
	for _, ev := range events {
		if ev.Kind == adapter.EventProgress && strings.Contains(ev.Text, "gate") {
			sawGate = true
		}
	}
	if !sawGate {
		t.Errorf("the pending_interaction gate was not relayed; kinds: %v", kinds(events))
	}
	if _, ok := firstOfKind(events, adapter.EventPermissionRequest); ok {
		t.Errorf("a permission request surfaced in full mode, which cannot happen with yoloMode")
	}

	// Two model calls in this recording, so two usage increments, and they
	// must sum to the turn total the prompt reply reported.
	if n := countOfKind(events, adapter.EventUsage); n != 2 {
		t.Errorf("got %d usage events, want 2 (one per model call)", n)
	}
	var sum adapter.Usage
	for _, ev := range events {
		if ev.Kind == adapter.EventUsage {
			sum.Add(*ev.Usage)
		}
	}
	if sum.InputTokens != res.Usage.InputTokens || sum.OutputTokens != res.Usage.OutputTokens {
		t.Errorf("usage increments sum to %+v but the turn total is %+v; "+
			"per-response input_tokens excludes the cached read and must have it added back",
			sum, res.Usage)
	}
}

func TestReplayResumeLoadsAndSuppressesTheReplayedHistory(t *testing.T) {
	// The pong recording answers session/new; a resume asks session/load, so
	// the script never matches and the handshake times out. What is being
	// asserted is which method the adapter chose.
	a, ref := replayAdapter(t, "pong.jsonl")
	spec := baseSpec(adapter.PermissionFull)
	spec.ResumeID = "01a04ff4-1dc0-76a1-9208-45bc771e1fca"
	spec.Timeouts.Startup = 2 * time.Second
	if _, err := a.Start(context.Background(), spec); err == nil {
		t.Fatal("expected the resume to fail against a fixture that only answers session/new")
	}
	p := ref.get(t)
	if _, ok := p.clientCall(methodSessionLoad); !ok {
		t.Errorf("a spec with a ResumeID did not call session/load; methods: %s", p.methodsSeen())
	}
	if _, ok := p.clientCall(methodSessionNew); ok {
		t.Errorf("a spec with a ResumeID started a new session instead of loading one")
	}
}

func TestSendIsRefusedRatherThanQueued(t *testing.T) {
	a, _ := replayAdapter(t, "pong.jsonl")
	sess, err := a.Start(context.Background(), baseSpec(adapter.PermissionFull))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	err = sess.Send(context.Background(), "actually, stop")
	if err == nil {
		t.Fatal("Send was accepted; ACP has no mid-turn steer and a queued prompt is a second turn")
	}
	drain(t, sess)
	if err := sess.Send(context.Background(), "hello?"); err != adapter.ErrSessionClosed {
		t.Errorf("Send after close = %v, want ErrSessionClosed", err)
	}
}

func TestRespondRejectsAnIDNobodyIsWaitingOn(t *testing.T) {
	a, _ := replayAdapter(t, "pong.jsonl")
	sess, err := a.Start(context.Background(), baseSpec(adapter.PermissionFull))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer drain(t, sess)
	if err := sess.Respond(context.Background(), adapter.PermissionResponse{ID: "nope", Allow: true}); err == nil {
		t.Error("Respond accepted an id that is not pending")
	}
	if err := sess.Respond(context.Background(), adapter.PermissionResponse{Allow: true}); err == nil {
		t.Error("Respond accepted a response with no id")
	}
}
