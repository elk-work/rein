package claude

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// streamLine is one NDJSON line of `claude --output-format stream-json`.
//
// One struct covers every line type because the vendor reuses field names
// across them, and the reuse is the footgun: `message` is a STRING on
// system/permission_denied and an OBJECT on assistant and user lines. It is
// therefore [json.RawMessage] here and decoded per line type — a typed field
// would fail to unmarshal a whole class of lines.
type streamLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	UUID      string          `json:"uuid"`
	Message   json.RawMessage `json:"message"`

	// system/init
	Model             string `json:"model"`
	PermissionMode    string `json:"permissionMode"`
	CWD               string `json:"cwd"`
	ClaudeCodeVersion string `json:"claude_code_version"`
	APIKeySource      string `json:"apiKeySource"`

	// MCPServers is the session's MCP inventory, on system/init.
	//
	// A POINTER to a slice so the decoder can tell "the init line said `[]`"
	// from "no init line has arrived". For a Rein-driven run the honest answer
	// is the empty list — every session is started with --strict-mcp-config
	// and an explicit `{"mcpServers":{}}` (see claude.go), deliberately, after
	// ark:rein#21 — and reporting that empty list accurately is the whole
	// point. Synthesising an inventory from the developer's ~/.claude.json is
	// precisely the failure that flag closes.
	//
	// `tools`, `plugins` and `skills` ride the same line and are still not
	// parsed. They are an inventory of what the session COULD do rather than
	// of what it is doing, they run to tens of kilobytes on a developer's own
	// machine, and the reading they would go into is capped at 16 KiB. They
	// stay in Event.Raw, which is where the run log already keeps them.
	MCPServers *[]mcpServerLine `json:"mcp_servers"`

	// system/hook_started, system/hook_response
	HookName   string `json:"hook_name"`
	HookEvent  string `json:"hook_event"`
	Outcome    string `json:"outcome"`
	HookStderr string `json:"stderr"`

	// system/permission_denied
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`

	// rate_limit_event
	RateLimitInfo *rateLimitInfo `json:"rate_limit_info"`

	// result
	IsError           bool                  `json:"is_error"`
	Result            string                `json:"result"`
	NumTurns          int                   `json:"num_turns"`
	DurationMS        int64                 `json:"duration_ms"`
	TotalCostUSD      float64               `json:"total_cost_usd"`
	TerminalReason    string                `json:"terminal_reason"`
	StopReason        string                `json:"stop_reason"`
	Usage             *tokenUsage           `json:"usage"`
	ModelUsage        map[string]modelUsage `json:"modelUsage"`
	PermissionDenials []permissionDenial    `json:"permission_denials"`
}

// vendorMessage is the Anthropic message carried by assistant and user lines.
type vendorMessage struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
	Usage   *tokenUsage    `json:"usage"`
}

// contentBlock is one block of a message's content array.
type contentBlock struct {
	Type string `json:"type"`

	// text / thinking
	Text string `json:"text"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// tokenUsage is the vendor's token accounting.
type tokenUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// toAdapter converts vendor token counts to Rein's. Cache creation is a write,
// cache read is a read; Rein reports counts and never money, because Elk prices
// server-side from model-rates.json.
func (u tokenUsage) toAdapter(model string) adapter.Usage {
	return adapter.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
		Model:            model,
	}
}

// contextTokens is how full the context was for the request this usage
// describes: everything the model was sent, whether it was read from the cache
// or not.
//
// The three input counts are disjoint — the vendor splits one prompt into what
// was newly written to the cache, what was read back from it, and what was
// neither — so the prompt's real size is their sum. Output tokens are
// deliberately excluded: they are what came BACK, and they only become context
// on the next request.
//
// This is a per-request number and not a running total. Summing it across
// messages would count the same cached prefix once per turn and report a
// session as many times over its own window.
func (u tokenUsage) contextTokens() int64 {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// modelUsage is the vendor's per-model roll-up on a result line.
type modelUsage struct {
	InputTokens              int64  `json:"inputTokens"`
	OutputTokens             int64  `json:"outputTokens"`
	CacheReadInputTokens     int64  `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64  `json:"cacheCreationInputTokens"`
	CanonicalModel           string `json:"canonicalModel"`

	// ContextWindow is how many tokens this model can hold. It has always been
	// on the wire — the recorded fixtures carry `"contextWindow":1000000` —
	// and was simply not in this struct, so it was dropped on the floor. It is
	// the denominator that makes context fill renderable at all: without it a
	// used-token count is a number with nothing to compare it to.
	//
	// It arrives on the RESULT line, so it is known from the end of the first
	// turn onwards, not from the start of one. That is fine and the reading
	// reflects it: the key is omitted until the vendor has said, rather than
	// guessed at from the model id.
	ContextWindow int64 `json:"contextWindow"`

	// MaxOutputTokens rides the same object and is deliberately unread: it
	// bounds one reply, not the session's state, and nothing renders it.
}

// mcpServerLine is one entry of system/init's mcp_servers.
type mcpServerLine struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// permissionDenial is one entry of a result line's permission_denials.
type permissionDenial struct {
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// rateLimitInfo is the subscription window state the vendor volunteers. It is
// worth relaying: a fleet that starves the developer at the keyboard is the
// named risk in elk docs/rein.md §3.
//
// Until fleet telemetry this was decoded and then thrown away as a sentence —
// "rate limit allowed, five_hour 58%, seven_day 70%" — with `resetsAt` decoded
// and never read, and the overage fields not decoded at all. A percentage in a
// progress line answers "how full"; it cannot answer "and when does that stop
// mattering", which is the question a person deciding whether to dispatch more
// work is actually asking. So it is structured now, and the prose line stays
// as well, because the prose is what a person reading `rein tail` sees.
type rateLimitInfo struct {
	Status        string `json:"status"`
	RateLimitType string `json:"rateLimitType"`

	// The three timestamps are unix epoch SECONDS, not milliseconds: the
	// fixtures carry 1788057600, which is 2026, and would be 1970 read as
	// milliseconds.
	ResetsAt        int64 `json:"resetsAt"`
	OverageResetsAt int64 `json:"overageResetsAt"`

	OverageStatus  string `json:"overageStatus"`
	IsUsingOverage *bool  `json:"isUsingOverage"`

	// OverageInUse is the newer spelling of "requests are being served from
	// extra usage" that Claude Code 2.1 emits beside isUsingOverage. Either one
	// true means a rejected window is not stopping work.
	OverageInUse *bool `json:"overageInUse"`

	UnifiedWindows map[string]rateLimitWindowLine `json:"unifiedWindows"`
}

// rateLimitWindowLine is one entry of unifiedWindows. Each window carries its
// own reset time, which is not the same as the event's top-level one: the
// top-level `resetsAt` belongs to whichever window is currently binding.
type rateLimitWindowLine struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
}

// epoch turns one of the vendor's unix seconds into a time, treating 0 and
// anything negative as "not said" rather than as 1970.
func epoch(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// toAdapter turns a rate-limit event into the structured form, with a fresh
// map every time. Freshness matters: the decoder publishes this to another
// goroutine by replacing the whole value, and a map it kept mutating would be
// a data race rather than an update.
//
// It also gives the adapter's verdict on it — see [rateLimitInfo.exhausted].
func (r *rateLimitInfo) toAdapter(now time.Time) *adapter.RateLimit {
	if r == nil {
		return nil
	}
	out := &adapter.RateLimit{
		Status:          r.Status,
		Type:            r.RateLimitType,
		ResetsAt:        epoch(r.ResetsAt),
		UsingOverage:    r.IsUsingOverage,
		OverageStatus:   r.OverageStatus,
		OverageResetsAt: epoch(r.OverageResetsAt),
		Source:          adapter.SourceClaudeEvent,
		SampledAt:       now.UTC(),
	}
	if len(r.UnifiedWindows) > 0 {
		out.Windows = make(map[string]adapter.RateLimitWindow, len(r.UnifiedWindows))
		for name, w := range r.UnifiedWindows {
			out.Windows[name] = adapter.RateLimitWindow{
				Utilization: w.Utilization,
				ResetsAt:    epoch(w.ResetsAt),
			}
		}
	}
	if r.exhausted() {
		out.Exhausted = true
		out.ExhaustedUntil = out.ResetsAt
		if out.ExhaustedUntil.IsZero() {
			// The binding window's reset is the top-level one; without it, the
			// latest reset among the windows that are full is the soonest the
			// account can possibly be served again.
			for _, w := range out.Windows {
				if w.Utilization >= 1 && w.ResetsAt.After(out.ExhaustedUntil) {
					out.ExhaustedUntil = w.ResetsAt
				}
			}
		}
	}
	return out
}

// exhausted is the adapter's reading of the event: Claude has said no to this
// account, and nothing else is paying for the request.
//
// Only `rejected` counts. `allowed_warning` is Claude saying the account is
// close, which is exactly when a run should still be allowed to start — the
// router, not this adapter, is the place to prefer a queue with more room. And
// a rejected window is not a stopped account when extra usage is serving the
// request: Claude Code 2.1 says so with `isUsingOverage`/`overageInUse`, or
// with an overage status that still allows it. Treating that as exhausted
// would park a queue whose owner has chosen to pay to keep it going.
func (r *rateLimitInfo) exhausted() bool {
	if r == nil || r.Status != "rejected" {
		return false
	}
	if (r.IsUsingOverage != nil && *r.IsUsingOverage) || (r.OverageInUse != nil && *r.OverageInUse) {
		return false
	}
	switch r.OverageStatus {
	case "allowed", "allowed_warning":
		return false
	}
	return true
}

// usageLimitRE is the older way Claude Code said the same thing: a failed
// result whose text is "Claude AI usage limit reached|<unix seconds>". Newer
// builds send a `rejected` rate_limit_event first, which is the signal this
// adapter prefers; the text is the fallback for a build that does not.
var usageLimitRE = regexp.MustCompile(`(?i)usage limit reached\|(\d{9,})`)

// decoder turns the vendor's NDJSON into Rein events. It carries the small
// amount of state that spans lines: the session id and model learned from
// `system/init`, and the denials collected along the way.
//
// It is deliberately free of process and channel concerns so the translation
// can be tested against recorded fixtures with no subprocess in sight.
type decoder struct {
	sessionID string
	model     string
	version   string
	sawInit   bool
	// apiKeySource is the init line's own account of where its credential
	// came from — [planAPIKeySource] on the plan login (planauth.go).
	apiKeySource string
	denials      int

	// The session-state half, for [Session.Telemetry]. It is state that spans
	// lines in exactly the way the fields above are, and it is published to
	// another goroutine — so every value here is REPLACED wholesale rather
	// than mutated in place, and [decoder.telemetry] hands out a copy. A map
	// or slice that this decoder kept writing into after publishing it would
	// be a data race dressed up as an update.
	contextUsed   int64
	contextKnown  bool
	contextWindow int64
	windowKnown   bool
	rateLimit     *adapter.RateLimit
	mcpServers    *[]adapter.MCPServer

	// clock stamps a rate-limit reading's SampledAt. Nil is the wall clock;
	// tests set it so a fixture decodes to the same reading every time.
	clock func() time.Time
}

func (d *decoder) now() time.Time {
	if d.clock != nil {
		return d.clock()
	}
	return time.Now()
}

// telemetry is the decoder's account of the session's own state, safe to hand
// to another goroutine.
func (d *decoder) telemetry() adapter.SessionTelemetry {
	t := adapter.SessionTelemetry{
		Model:      d.model,
		RateLimit:  d.rateLimit,
		MCPServers: d.mcpServers,
	}
	if d.contextKnown {
		used := d.contextUsed
		t.ContextUsedTokens = &used
	}
	if d.windowKnown {
		window := d.contextWindow
		t.ContextWindowTokens = &window
	}
	return t
}

// decode turns one NDJSON line into zero or more events, plus a non-nil result
// when the line is terminal.
//
// Returning a slice rather than one event is not a convenience: a single
// assistant line routinely carries text AND a tool call AND a usage increment,
// and collapsing those would lose the tool call.
func (d *decoder) decode(raw []byte) (evs []adapter.Event, res *adapter.Result, err error) {
	line := strings.TrimSpace(string(raw))
	if line == "" {
		return nil, nil, nil
	}

	var sl streamLine
	if err := json.Unmarshal([]byte(line), &sl); err != nil {
		// A line Rein cannot parse is still worth surfacing — it is the only
		// evidence a format changed — but it must never kill the session.
		return []adapter.Event{{
			Kind: adapter.EventProgress,
			Text: "unparsable stream line: " + truncate(line, 200),
			Raw:  json.RawMessage(line),
		}}, nil, nil
	}
	if sl.SessionID != "" {
		d.sessionID = sl.SessionID
	}
	rawMsg := json.RawMessage(line)

	switch sl.Type {
	case "system":
		return d.decodeSystem(sl, rawMsg)
	case "assistant":
		return d.decodeAssistant(sl, rawMsg), nil, nil
	case "user":
		return d.decodeUser(sl, rawMsg), nil, nil
	case "rate_limit_event":
		return d.decodeRateLimit(sl, rawMsg), nil, nil
	case "result":
		ev, result := d.decodeResult(sl, rawMsg)
		return ev, result, nil
	default:
		// The event set is closed. Anything Rein has no kind for is progress
		// with the vendor payload attached, never a kind invented on the spot.
		return []adapter.Event{{
			Kind: adapter.EventProgress,
			Text: "claude event " + sl.Type,
			Raw:  rawMsg,
		}}, nil, nil
	}
}

func (d *decoder) decodeSystem(sl streamLine, raw json.RawMessage) ([]adapter.Event, *adapter.Result, error) {
	switch sl.Subtype {
	case "init":
		d.model = sl.Model
		d.version = sl.ClaudeCodeVersion
		d.apiKeySource = sl.APIKeySource
		d.sawInit = true
		if sl.MCPServers != nil {
			servers := make([]adapter.MCPServer, 0, len(*sl.MCPServers))
			for _, s := range *sl.MCPServers {
				servers = append(servers, adapter.MCPServer{Name: s.Name, Status: s.Status})
			}
			// A fresh slice each init, assigned wholesale: see the note on
			// [decoder]. An empty one is a real answer and is kept as such.
			d.mcpServers = &servers
		}
		return []adapter.Event{{
			Kind: adapter.EventProgress,
			Text: fmt.Sprintf("claude session %s started (model %s, permission mode %s, version %s)",
				sl.SessionID, sl.Model, sl.PermissionMode, sl.ClaudeCodeVersion),
			Raw: raw,
		}}, nil, nil

	case "hook_started":
		// Dropped. The developer's own hooks fire on every session and a
		// progress event each would drown the run's real story.
		return nil, nil, nil

	case "hook_response":
		if sl.Outcome == "success" {
			return nil, nil, nil
		}
		text := fmt.Sprintf("hook %s (%s) did not succeed: %s", sl.HookName, sl.HookEvent, sl.Outcome)
		if e := strings.TrimSpace(sl.HookStderr); e != "" {
			text += ": " + truncate(e, 300)
		}
		return []adapter.Event{{Kind: adapter.EventProgress, Text: text, Raw: raw}}, nil, nil

	case "permission_denied":
		d.denials++
		// NOT an EventPermissionRequest. That kind obliges the run loop to
		// answer, and this adapter declares approvals partial precisely because
		// nothing can. The denial has already happened; reporting it as a
		// request would invite a Respond call that returns ErrNotSupported.
		msg := decodeStringField(sl.Message)
		text := fmt.Sprintf("permission denied for %s (headless runs cannot approve)", sl.ToolName)
		if msg != "" {
			text = "permission denied: " + msg
		}
		return []adapter.Event{{Kind: adapter.EventProgress, Text: text, Raw: raw}}, nil, nil

	default:
		return []adapter.Event{{
			Kind: adapter.EventProgress,
			Text: "claude system/" + sl.Subtype,
			Raw:  raw,
		}}, nil, nil
	}
}

func (d *decoder) decodeAssistant(sl streamLine, raw json.RawMessage) []adapter.Event {
	var msg vendorMessage
	if err := json.Unmarshal(sl.Message, &msg); err != nil {
		return []adapter.Event{{Kind: adapter.EventProgress, Text: "assistant message", Raw: raw}}
	}
	if msg.Model != "" {
		d.model = msg.Model
	}

	var evs []adapter.Event
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			evs = append(evs, adapter.Event{Kind: adapter.EventText, Text: b.Text, Raw: raw})
		case "tool_use":
			evs = append(evs, adapter.Event{
				Kind: adapter.EventToolUse,
				Text: b.Name,
				Tool: &adapter.ToolUse{ID: b.ID, Name: b.Name, Input: b.Input},
				Raw:  raw,
			})
		case "thinking", "redacted_thinking":
			// Deliberately dropped. Reasoning is not a work record, and Elk
			// relays what an adapter emits.
		}
	}

	if msg.Usage != nil {
		u := msg.Usage.toAdapter(d.model)
		evs = append(evs, adapter.Event{
			Kind:  adapter.EventUsage,
			Usage: &u,
			Raw:   raw,
		})
		d.contextUsed, d.contextKnown = msg.Usage.contextTokens(), true
	}
	return evs
}

func (d *decoder) decodeUser(sl streamLine, raw json.RawMessage) []adapter.Event {
	var msg vendorMessage
	if err := json.Unmarshal(sl.Message, &msg); err != nil {
		return []adapter.Event{{Kind: adapter.EventProgress, Text: "user message", Raw: raw}}
	}

	var evs []adapter.Event
	for _, b := range msg.Content {
		if b.Type != "tool_result" {
			continue
		}
		// Rein's event vocabulary has tool_use but no tool_result, so this is
		// progress carrying the vendor payload — the correlation id is in Raw
		// and in the text, so a transcript can pair them up.
		status := "ok"
		if b.IsError {
			status = "error"
		}
		evs = append(evs, adapter.Event{
			Kind: adapter.EventProgress,
			Text: fmt.Sprintf("tool result %s (%s): %s", b.ToolUseID, status,
				truncate(decodeToolResultContent(b.Content), 400)),
			Raw: raw,
		})
	}
	return evs
}

func (d *decoder) decodeRateLimit(sl streamLine, raw json.RawMessage) []adapter.Event {
	text := "rate limit update"
	if sl.RateLimitInfo != nil {
		// Structured for the fleet reading, and still prose for the run log:
		// the two readers want different things and neither substitutes for
		// the other.
		d.rateLimit = sl.RateLimitInfo.toAdapter(d.now())

		parts := []string{"rate limit " + sl.RateLimitInfo.Status}
		for _, w := range []string{"five_hour", "seven_day"} {
			if v, ok := sl.RateLimitInfo.UnifiedWindows[w]; ok {
				parts = append(parts, fmt.Sprintf("%s %.0f%%", w, v.Utilization*100))
			}
		}
		if t := epoch(sl.RateLimitInfo.ResetsAt); !t.IsZero() {
			parts = append(parts, "resets "+t.Format(time.RFC3339))
		}
		if b := sl.RateLimitInfo.IsUsingOverage; b != nil && *b {
			parts = append(parts, "on overage")
		}
		text = strings.Join(parts, ", ")
	}
	return []adapter.Event{{Kind: adapter.EventProgress, Text: text, Raw: raw}}
}

// decodeResult builds the terminal result. The caller decides whether it
// becomes an EventDone or an EventError, because only the caller knows whether
// the session was also interrupted or timed out.
func (d *decoder) decodeResult(sl streamLine, raw json.RawMessage) ([]adapter.Event, *adapter.Result) {
	status := adapter.StatusSucceeded
	if sl.IsError || sl.Subtype != "success" {
		status = adapter.StatusFailed
	}

	model := d.model
	for _, mu := range sl.ModelUsage {
		if mu.CanonicalModel != "" {
			model = mu.CanonicalModel
			break
		}
	}
	// The largest window wins where a turn used more than one model — a
	// session with subagents reports several. It is the window the main
	// conversation is measured against, and the alternative is taking
	// whichever entry Go's map iteration happened to visit first.
	for _, mu := range sl.ModelUsage {
		if mu.ContextWindow > d.contextWindow {
			d.contextWindow, d.windowKnown = mu.ContextWindow, true
		}
	}

	// A failed result that says the usage limit was reached is an exhausted
	// account even when no `rejected` rate_limit_event came first — which is
	// how older Claude Code builds reported it. The event, when there was one,
	// already said as much and carries more, so it is left alone.
	if status == adapter.StatusFailed && (d.rateLimit == nil || !d.rateLimit.Exhausted) {
		if m := usageLimitRE.FindStringSubmatch(sl.Result); m != nil {
			sec, _ := strconv.ParseInt(m[1], 10, 64)
			rl := &adapter.RateLimit{
				Status:    "rejected",
				Source:    adapter.SourceClaudeEvent,
				SampledAt: d.now().UTC(),
				Exhausted: true,
				ResetsAt:  epoch(sec),
			}
			if prev := d.rateLimit; prev != nil {
				// Keep what the last event said about the windows. Replaced
				// wholesale, never mutated, for the reason on [decoder].
				rl.Windows, rl.Type, rl.UsingOverage = prev.Windows, prev.Type, prev.UsingOverage
			}
			rl.ExhaustedUntil = rl.ResetsAt
			d.rateLimit = rl
		}
	}

	res := &adapter.Result{
		Status:    status,
		Summary:   sl.Result,
		SessionID: sl.SessionID,
		Duration:  time.Duration(sl.DurationMS) * time.Millisecond,
	}
	if res.SessionID == "" {
		res.SessionID = d.sessionID
	}
	if sl.Usage != nil {
		// The session total, taken from the vendor rather than summed from the
		// EventUsage increments: the per-message numbers repeat cache reads
		// across iterations, so summing them would disagree with what the
		// vendor bills.
		res.Usage = sl.Usage.toAdapter(model)
	}
	return nil, res
}

// decodeStringField reads a JSON field that is sometimes a string. It returns
// "" for anything else rather than erroring — the caller always has a fallback.
func decodeStringField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// decodeToolResultContent renders a tool result body, which the vendor writes
// as either a bare string or an array of content blocks.
func decodeToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s := decodeStringField(raw); s != "" {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return string(raw)
}

// truncate shortens a string for a log line or an event summary.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
