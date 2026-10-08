package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// The fixtures in testdata/ are real `claude -p --output-format stream-json
// --verbose` recordings from 2.1.251 on darwin/arm64, 2026-08-29. The only
// edits are redactions: the operator's tool/skill/plugin/MCP inventory is
// replaced with a short placeholder list in the `system/init` line, and the
// home directory is rewritten. Every event shape is the vendor's own.

// decoded is the outcome of replaying one fixture through the decoder, in the
// same order and with the same "first result wins" rule the session uses.
type decoded struct {
	events  []adapter.Event
	first   *adapter.Result
	results int
	dec     *decoder
}

func replay(t *testing.T, name string) decoded {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("opening the fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	out := decoded{dec: &decoder{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxStreamLine)
	for sc.Scan() {
		evs, res, err := out.dec.decode(sc.Bytes())
		if err != nil {
			t.Fatalf("decoding a line of %s: %v", name, err)
		}
		out.events = append(out.events, evs...)
		if res != nil {
			out.results++
			if out.first == nil {
				out.first = res
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning %s: %v", name, err)
	}
	return out
}

func kinds(evs []adapter.Event) []adapter.EventKind {
	out := make([]adapter.EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func TestDecodeHelloSession(t *testing.T) {
	got := replay(t, "hello.jsonl")

	// hook_started and a successful hook_response are deliberately dropped, so
	// a trivial session reads as: started, said something, spent tokens, rate
	// limit, done.
	want := []adapter.EventKind{
		adapter.EventProgress, // system/init
		adapter.EventText,     // assistant text
		adapter.EventUsage,    // that message's tokens
		adapter.EventProgress, // rate_limit_event
	}
	if diff := kindsDiff(kinds(got.events), want); diff != "" {
		t.Errorf("event kinds: %s", diff)
	}

	if got.results != 1 {
		t.Errorf("result lines = %d, want 1", got.results)
	}
	if got.first == nil {
		t.Fatal("no terminal result decoded")
	}
	if got.first.Status != adapter.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", got.first.Status)
	}
	if got.first.SessionID == "" {
		t.Error("the result carries no session id")
	}
	if !strings.Contains(got.first.Summary, "Hi!") {
		t.Errorf("summary = %q, want the assistant's reply", got.first.Summary)
	}
	if got.first.Usage.Model == "" {
		t.Error("the result's usage names no model; Elk prices by model")
	}
	if got.first.Usage.CacheReadTokens == 0 {
		t.Error("cache read tokens were dropped; the recording has 10022")
	}
}

func TestDecodeCapturesSessionIDFromInit(t *testing.T) {
	got := replay(t, "hello.jsonl")
	if got.dec.sessionID == "" {
		t.Fatal("the decoder learned no session id")
	}
	// Session.ID must be available long before the result, because it is what
	// a resume and `rein attach` take.
	if !strings.Contains(got.events[0].Text, got.dec.sessionID) {
		t.Errorf("the first progress event should name the session; got %q", got.events[0].Text)
	}
}

func TestDecodeToolUse(t *testing.T) {
	got := replay(t, "tools.jsonl")

	var tools []*adapter.ToolUse
	for _, e := range got.events {
		if e.Kind == adapter.EventToolUse {
			if e.Tool == nil {
				t.Fatal("a tool_use event carries no Tool")
			}
			tools = append(tools, e.Tool)
		}
	}
	if len(tools) != 2 {
		t.Fatalf("tool calls = %d, want 2 (Read then Write)", len(tools))
	}
	if tools[0].Name != "Read" || tools[1].Name != "Write" {
		t.Errorf("tool names = %q, %q; want Read, Write", tools[0].Name, tools[1].Name)
	}
	for _, tu := range tools {
		if !strings.HasPrefix(tu.ID, "toolu_") {
			t.Errorf("tool call id %q does not look like a vendor id", tu.ID)
		}
		// The arguments travel verbatim: the run loop transcribes them and
		// never interprets them.
		var args map[string]any
		if err := json.Unmarshal(tu.Input, &args); err != nil {
			t.Errorf("tool %s input is not valid JSON: %v", tu.Name, err)
		}
		if _, ok := args["file_path"]; !ok {
			t.Errorf("tool %s input lost file_path: %s", tu.Name, tu.Input)
		}
	}

	// Rein has no tool_result kind, so results are progress carrying the
	// correlation id — a transcript can still pair them with the call.
	var paired int
	for _, e := range got.events {
		if e.Kind == adapter.EventProgress && strings.HasPrefix(e.Text, "tool result ") {
			paired++
			if !strings.Contains(e.Text, "toolu_") {
				t.Errorf("tool result event drops the correlation id: %q", e.Text)
			}
		}
	}
	if paired != 2 {
		t.Errorf("tool result events = %d, want 2", paired)
	}
}

// TestDecodePermissionDeniedIsNotARequest pins the decision that matters most
// to the run loop: `claude -p` auto-denies, so the denial is reported, but it
// is never an EventPermissionRequest — that kind obliges a Respond this
// adapter cannot honour.
func TestDecodePermissionDeniedIsNotARequest(t *testing.T) {
	got := replay(t, "permission_denied.jsonl")

	for _, e := range got.events {
		if e.Kind == adapter.EventPermissionRequest {
			t.Fatal("emitted a permission_request while declaring approvals=partial")
		}
	}

	var denials int
	for _, e := range got.events {
		if e.Kind == adapter.EventProgress && strings.Contains(e.Text, "permission denied") {
			denials++
		}
	}
	if denials != 1 {
		t.Errorf("permission denial events = %d, want 1", denials)
	}
	if got.dec.denials != 1 {
		t.Errorf("decoder counted %d denials, want 1", got.dec.denials)
	}

	// The vendor calls an auto-denied run a success. Rein reports what the
	// vendor said; the denial is visible in the event stream, not by
	// second-guessing the status.
	if got.first == nil || got.first.Status != adapter.StatusSucceeded {
		t.Errorf("status = %v, want the vendor's own succeeded", got.first)
	}
}

// TestDecodeMultiturnHasTwoResults is the recording behind the single most
// important behaviour in this package: under --input-format stream-json a
// `result` line ends a TURN, not the session.
func TestDecodeMultiturnHasTwoResults(t *testing.T) {
	got := replay(t, "multiturn.jsonl")

	if got.results != 2 {
		t.Fatalf("result lines = %d, want 2 — the fixture is two turns down one stdin", got.results)
	}
	if got.first == nil {
		t.Fatal("no first result")
	}
	if !strings.Contains(got.first.Summary, "FIRST") {
		t.Errorf("the first result should end turn one; got %q", got.first.Summary)
	}

	// Both turns report the same vendor session id, which is why one Rein run
	// maps onto one Claude Code session however many turns it takes.
	var inits int
	for _, e := range got.events {
		if e.Kind == adapter.EventProgress && strings.Contains(e.Text, "claude session ") {
			inits++
		}
	}
	if inits != 2 {
		t.Errorf("init events = %d, want 2 (one per turn)", inits)
	}
}

func TestDecodeUsageIsAnIncrementWithAModel(t *testing.T) {
	got := replay(t, "tools.jsonl")

	var seen int
	for _, e := range got.events {
		if e.Kind != adapter.EventUsage {
			continue
		}
		seen++
		if e.Usage == nil {
			t.Fatal("a usage event carries no Usage")
		}
		if e.Usage.Model == "" {
			t.Error("a usage increment names no model")
		}
	}
	if seen == 0 {
		t.Fatal("no usage events decoded")
	}
}

func TestDecodeUnknownAndUnparsableLines(t *testing.T) {
	d := &decoder{}

	// A type Rein has no kind for becomes progress carrying the payload,
	// rather than a kind invented on the spot.
	evs, res, err := d.decode([]byte(`{"type":"something_new","session_id":"s1"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res != nil {
		t.Error("an unknown line must not be terminal")
	}
	if len(evs) != 1 || evs[0].Kind != adapter.EventProgress {
		t.Fatalf("events = %v, want one progress event", kinds(evs))
	}
	if len(evs[0].Raw) == 0 {
		t.Error("the vendor payload was dropped from Raw")
	}

	// Malformed JSON is surfaced, never fatal: it is the only evidence the
	// format moved.
	evs, _, err = d.decode([]byte(`{"type":"assistant"`))
	if err != nil {
		t.Fatalf("a malformed line must not error the session: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != adapter.EventProgress {
		t.Fatalf("events = %v, want one progress event", kinds(evs))
	}

	// Blank lines are nothing at all.
	evs, _, _ = d.decode([]byte("   "))
	if len(evs) != 0 {
		t.Errorf("a blank line produced %d events", len(evs))
	}
}

// TestDecodeMessageFieldPolymorphism guards the footgun that would break a
// naive struct: `message` is an object on assistant lines and a plain string
// on system/permission_denied.
func TestDecodeMessageFieldPolymorphism(t *testing.T) {
	d := &decoder{}

	evs, _, err := d.decode([]byte(`{"type":"system","subtype":"permission_denied",` +
		`"tool_name":"Bash","tool_use_id":"toolu_1","message":"Claude requested permissions"}`))
	if err != nil {
		t.Fatalf("string-valued message: %v", err)
	}
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "Claude requested permissions") {
		t.Errorf("string message not carried through: %+v", evs)
	}

	evs, _, err = d.decode([]byte(`{"type":"assistant","message":{"role":"assistant",` +
		`"model":"claude-fable-5","content":[{"type":"text","text":"hello"}]}}`))
	if err != nil {
		t.Fatalf("object-valued message: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != adapter.EventText || evs[0].Text != "hello" {
		t.Errorf("object message not decoded: %+v", evs)
	}
}

func TestDecodeDropsThinkingBlocks(t *testing.T) {
	d := &decoder{}
	evs, _, err := d.decode([]byte(`{"type":"assistant","message":{"role":"assistant",` +
		`"content":[{"type":"thinking","text":"private reasoning"},{"type":"text","text":"said"}]}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1 — thinking must not be relayed", len(evs))
	}
	if evs[0].Text != "said" {
		t.Errorf("text = %q, want the spoken text", evs[0].Text)
	}
}

func TestDecodeFailedResultIsFailed(t *testing.T) {
	d := &decoder{}
	_, res, err := d.decode([]byte(`{"type":"result","subtype":"error_during_execution",` +
		`"is_error":true,"session_id":"s9","result":"it broke","duration_ms":1200}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res == nil {
		t.Fatal("no result decoded")
	}
	if res.Status != adapter.StatusFailed {
		t.Errorf("status = %q, want failed", res.Status)
	}
	if res.SessionID != "s9" {
		t.Errorf("session id = %q", res.SessionID)
	}
}

// The session-state half: facts that were already on the wire and were being
// thrown away. Every assertion below is against the recorded fixtures, so a
// vendor change that moves one of these fails here rather than in a fleet page
// that quietly renders nothing.

func TestDecodeSessionTelemetryFromARealSession(t *testing.T) {
	got := replay(t, "hello.jsonl")
	tel := got.dec.telemetry()

	if tel.Model == "" {
		t.Error("the telemetry names no model")
	}

	// contextWindow rides modelUsage on the result line and was simply not in
	// the struct. It is the denominator that makes context fill renderable at
	// all: without it a used-token count is a number with nothing to compare
	// it to.
	if tel.ContextWindowTokens == nil {
		t.Fatal("no context window — the fixture carries contextWindow: 1000000")
	}
	if *tel.ContextWindowTokens != 1000000 {
		t.Errorf("context window = %d, want 1000000", *tel.ContextWindowTokens)
	}

	// Context used is the whole prompt the last request carried: the three
	// input counts are disjoint, so their sum is its real size. The fixture's
	// assistant message is 2 + 10022 + 17244.
	if tel.ContextUsedTokens == nil {
		t.Fatal("no context used")
	}
	if want := int64(2 + 10022 + 17244); *tel.ContextUsedTokens != want {
		t.Errorf("context used = %d, want %d — input + cache read + cache creation",
			*tel.ContextUsedTokens, want)
	}
	if *tel.ContextUsedTokens > *tel.ContextWindowTokens {
		t.Error("context used is over the window, which means one of them is being summed wrongly")
	}

	// The MCP inventory. Rein starts every run with --strict-mcp-config and an
	// explicit empty server set (ark:rein#21), so the honest answer here is the
	// EMPTY LIST — reported as such, and distinct from never having asked.
	if tel.MCPServers == nil {
		t.Fatal("mcp_servers was not decoded; the init line carries []")
	}
	if len(*tel.MCPServers) != 0 {
		t.Errorf("mcp_servers = %v, want the empty list a strict-mcp-config run really has", *tel.MCPServers)
	}
}

// TestDecodeRateLimitIsStructuredNotOnlyProse. The prose line stays — it is
// what a person reading `rein tail` sees — but a percentage cannot answer "and
// when does that stop mattering", which is the question somebody deciding
// whether to dispatch more work is actually asking.
func TestDecodeRateLimitIsStructuredNotOnlyProse(t *testing.T) {
	got := replay(t, "hello.jsonl")
	rl := got.dec.telemetry().RateLimit
	if rl == nil {
		t.Fatal("the rate limit event produced no structured reading")
	}
	if rl.Status != "allowed" || rl.Type != "five_hour" {
		t.Errorf("status/type = %q/%q", rl.Status, rl.Type)
	}
	if rl.ResetsAt.IsZero() {
		t.Error("resetsAt was decoded and dropped — it always was")
	}
	if want := time.Unix(1788057600, 0).UTC(); !rl.ResetsAt.Equal(want) {
		t.Errorf("resetsAt = %s, want %s (unix SECONDS, not milliseconds)", rl.ResetsAt, want)
	}
	if rl.OverageStatus != "allowed" {
		t.Errorf("overageStatus = %q", rl.OverageStatus)
	}
	if rl.UsingOverage == nil {
		t.Fatal("isUsingOverage was not decoded; an explicit false is a different claim from silence")
	}
	if *rl.UsingOverage {
		t.Error("isUsingOverage = true; the fixture says false")
	}
	if rl.OverageResetsAt.IsZero() {
		t.Error("overageResetsAt was dropped")
	}

	// Every window the vendor reported, keyed by its own name — including the
	// third one, which nothing renders today and which a curated struct would
	// have silently dropped.
	for name, want := range map[string]float64{
		"five_hour":                  0.43,
		"seven_day":                  0.67,
		"seven_day_overage_included": 0.18,
	} {
		w, ok := rl.Windows[name]
		if !ok {
			t.Errorf("window %q is missing", name)
			continue
		}
		if w.Utilization != want {
			t.Errorf("window %q utilization = %v, want %v", name, w.Utilization, want)
		}
		if w.ResetsAt.IsZero() {
			t.Errorf("window %q carries no reset time", name)
		}
	}

	// And the prose is still there for the run log.
	var prose string
	for _, e := range got.events {
		if strings.HasPrefix(e.Text, "rate limit ") {
			prose = e.Text
		}
	}
	if !strings.Contains(prose, "five_hour 43%") || !strings.Contains(prose, "seven_day 67%") {
		t.Errorf("the progress line lost its percentages: %q", prose)
	}
}

// TestTelemetryBeforeAnythingArrivesReportsNothing. Absent and zero are
// different claims, and a session that has not started must not render as a
// session at 0% context on a rate limit of nothing.
func TestTelemetryBeforeAnythingArrivesReportsNothing(t *testing.T) {
	tel := (&decoder{}).telemetry()
	if tel.ContextUsedTokens != nil || tel.ContextWindowTokens != nil {
		t.Errorf("context reported before any message: %+v", tel)
	}
	if tel.RateLimit != nil || tel.MCPServers != nil {
		t.Errorf("rate limit or MCP inventory reported before init: %+v", tel)
	}
}

// TestDecodeMCPServersWhenThereAreSome: the empty list is what a Rein run
// produces, but the parser must still read a real inventory — a `rein attach`
// user or a future non-strict mode would have one.
func TestDecodeMCPServersWhenThereAreSome(t *testing.T) {
	d := &decoder{}
	_, _, err := d.decode([]byte(`{"type":"system","subtype":"init","session_id":"s1",` +
		`"model":"claude-opus-5","mcp_servers":[{"name":"elk","status":"connected"},` +
		`{"name":"chrome","status":"failed"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	servers := d.telemetry().MCPServers
	if servers == nil || len(*servers) != 2 {
		t.Fatalf("mcp_servers = %v, want two entries", servers)
	}
	if (*servers)[0].Name != "elk" || (*servers)[0].Status != "connected" {
		t.Errorf("first server = %+v", (*servers)[0])
	}
	if (*servers)[1].Status != "failed" {
		t.Errorf("a failed server's status was dropped: %+v", (*servers)[1])
	}
}

// TestContextUsedFollowsTheLatestMessage: it is a per-request number and not a
// running total. Summing it across turns would count the same cached prefix
// once per turn and report a session many times over its own window.
func TestContextUsedFollowsTheLatestMessage(t *testing.T) {
	d := &decoder{}
	msg := func(in, cacheRead int64) []byte {
		return []byte(`{"type":"assistant","message":{"role":"assistant","model":"m",` +
			`"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":` +
			strconv.FormatInt(in, 10) + `,"cache_read_input_tokens":` +
			strconv.FormatInt(cacheRead, 10) + `,"output_tokens":5}}}`)
	}
	if _, _, err := d.decode(msg(10, 1000)); err != nil {
		t.Fatal(err)
	}
	if got := *d.telemetry().ContextUsedTokens; got != 1010 {
		t.Fatalf("context used = %d, want 1010", got)
	}
	if _, _, err := d.decode(msg(20, 3000)); err != nil {
		t.Fatal(err)
	}
	if got := *d.telemetry().ContextUsedTokens; got != 3020 {
		t.Errorf("context used = %d, want 3020 — the latest request, not the sum", got)
	}
}

// TestSessionExposesTelemetry checks the optional interface is actually
// satisfied, which is what the run loop type-asserts for.
func TestSessionExposesTelemetry(t *testing.T) {
	var s any = &Session{}
	if _, ok := s.(adapter.Telemeter); !ok {
		t.Fatal("claude's Session does not implement adapter.Telemeter")
	}
}

// kindsDiff renders a readable mismatch between two event-kind sequences.
func kindsDiff(got, want []adapter.EventKind) string {
	if len(got) == len(want) {
		same := true
		for i := range got {
			if got[i] != want[i] {
				same = false
				break
			}
		}
		if same {
			return ""
		}
	}
	return "got " + join(got) + ", want " + join(want)
}

func join(ks []adapter.EventKind) string {
	parts := make([]string, len(ks))
	for i, k := range ks {
		parts[i] = string(k)
	}
	return "[" + strings.Join(parts, " ") + "]"
}
