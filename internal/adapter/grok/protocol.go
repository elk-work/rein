package grok

import "encoding/json"

// The subset of the Agent Client Protocol this adapter speaks, plus the x.ai
// extensions Grok Build adds on top of it.
//
// Hand-written rather than generated: Grok ships no schema generator the way
// Codex does (`codex app-server generate-json-schema`). What these types are
// derived from is the ACP specification (agentclientprotocol.com), the vendor
// documentation that ships inside the binary's home
// (`~/.grok/docs/user-guide/15-agent-mode.md`), and — for anything either of
// those left ambiguous — frames recorded from the running agent. The
// recordings are in testdata/ and the fields that came from them are marked.
//
// **Protocol version 1**, negotiated and confirmed by `grok 1.0.13` on
// 2026-08-29. `initialize` returned `protocolVersion: 1`.

const (
	// protocolVersion is what this client asks for and what Grok agreed to.
	protocolVersion = 1

	// Client → agent.
	methodInitialize    = "initialize"
	methodSessionNew    = "session/new"
	methodSessionLoad   = "session/load"
	methodSessionPrompt = "session/prompt"
	methodSessionCancel = "session/cancel"

	// Agent → client notification.
	notifySessionUpdate = "session/update"

	// Agent → client request.
	reqRequestPermission = "session/request_permission"

	// x.ai extensions. Everything under this prefix is Grok's own and is not
	// part of ACP; the ones named here are the ones this adapter reads.
	notifyXSessionNotification = "_x.ai/session_notification"
	notifyXSessionsChanged     = "_x.ai/sessions/changed"
	notifyXPromptComplete      = "_x.ai/session/prompt_complete"
	notifyXMCPServerStatus     = "_x.ai/mcp/server_status"
)

// sessionUpdate discriminator values — the `sessionUpdate` member of a
// `session/update` notification. The first five are ACP's, documented in
// `15-agent-mode.md`; the rest arrive on `_x.ai/session_notification` and were
// read off the wire.
const (
	updAgentMessageChunk = "agent_message_chunk"
	updAgentThoughtChunk = "agent_thought_chunk"
	updUserMessageChunk  = "user_message_chunk"
	updToolCall          = "tool_call"
	updToolCallUpdate    = "tool_call_update"
	updPlan              = "plan"
	updAvailableCommands = "available_commands_update"
	updCurrentMode       = "current_mode_update"
	updSessionInfo       = "session_info_update"

	// x.ai, observed.
	updResponseCompleted = "response_completed"
	updTurnCompleted     = "turn_completed"
	updPendingInteract   = "pending_interaction"
	updInteractResolved  = "interaction_resolved"
	updLastTurnSummary   = "last_turn_summary"
	updHookExecution     = "hook_execution"
	updModelChanged      = "model_changed"
	updSessionSummary    = "session_summary_generated"
)

// ACP stop reasons, returned by `session/prompt`.
const (
	stopEndTurn         = "end_turn"
	stopCancelled       = "cancelled"
	stopMaxTokens       = "max_tokens"
	stopMaxTurnRequests = "max_turn_requests"
	stopRefusal         = "refusal"
)

type clientCapabilities struct {
	FS       fsCapabilities `json:"fs"`
	Terminal bool           `json:"terminal"`
}

// fsCapabilities is what the client offers to do on the agent's behalf. Rein
// declares **neither**: an adapter that offers to read and write files for the
// agent has to police those paths itself, and the whole point of the sandbox
// mapping below is that the OS polices them instead. Grok falls back to its own
// tools, which the sandbox covers.
type fsCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
}

type initializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	Meta              initializeMeta    `json:"_meta"`
}

type agentCapabilities struct {
	LoadSession bool `json:"loadSession"`
}

type initializeMeta struct {
	AgentVersion string `json:"agentVersion"`
	// DefaultAuthMethodID is `cached_token` when Grok has a login cached. It
	// is a hint, not the preflight — the preflight is `grok models`, which
	// says so in words.
	DefaultAuthMethodID string `json:"defaultAuthMethodId"`
}

// sessionNewParams starts a session. `mcpServers` is ACP's; `_meta` is Grok's,
// documented in `~/.grok/docs/user-guide/15-agent-mode.md` under "Session
// `_meta` options".
type sessionNewParams struct {
	Cwd        string          `json:"cwd"`
	MCPServers []any           `json:"mcpServers"`
	Meta       *sessionNewMeta `json:"_meta,omitempty"`
}

// sessionNewMeta carries the two knobs Rein uses.
//
//	yoloMode — always-approve for this session
//	rules    — extra rules appended to the system prompt
//
// `rules` is what carries [adapter.RunSpec.SystemPrompt]: it is *additive*,
// where the sibling `systemPromptOverride` would replace Grok's own
// instructions wholesale. Verified against the running agent — a rule saying
// "begin every reply with ZZQQ" produced replies beginning with ZZQQ.
type sessionNewMeta struct {
	YoloMode bool   `json:"yoloMode,omitempty"`
	AutoMode bool   `json:"autoMode,omitempty"`
	Rules    string `json:"rules,omitempty"`
}

type sessionNewResponse struct {
	SessionID string     `json:"sessionId"`
	Models    modelState `json:"models"`
}

type modelState struct {
	CurrentModelID string `json:"currentModelId"`
}

// sessionLoadParams resumes a session by id — ACP's `session/load`, available
// because `initialize` reported `loadSession: true`.
//
// **It replays the whole prior conversation as `session/update` notifications
// before it returns.** That is the protocol working as designed and it is a
// trap for a translator: replayed history is not news, and emitting it as
// fresh text would double-report every message the run loop already saw.
// See [session.loadSession].
type sessionLoadParams struct {
	SessionID  string `json:"sessionId"`
	Cwd        string `json:"cwd"`
	MCPServers []any  `json:"mcpServers"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func textPrompt(s string) []contentBlock { return []contentBlock{{Type: "text", Text: s}} }

type sessionPromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

// sessionPromptResponse ends the turn. `stopReason` is ACP's; `_meta` is
// Grok's, and is where the turn's token accounting lives — ACP itself has no
// usage channel, so an adapter that reported nothing here would be reporting
// nothing at all.
type sessionPromptResponse struct {
	StopReason string            `json:"stopReason"`
	Meta       sessionPromptMeta `json:"_meta"`
}

type sessionPromptMeta struct {
	ModelID string     `json:"modelId"`
	Usage   *turnUsage `json:"usage"`
}

// turnUsage is the whole-turn total. Note the casing: this one is camelCase
// while the per-response usage below is snake_case. They are different
// payloads from different code paths and both are real.
type turnUsage struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	TotalTokens       int64 `json:"totalTokens"`
	CachedReadTokens  int64 `json:"cachedReadTokens"`
	CacheCreationToks int64 `json:"cacheCreationTokens"`
	ReasoningTokens   int64 `json:"reasoningTokens"`
	ModelCalls        int64 `json:"modelCalls"`
}

type sessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

// sessionUpdateNotification is the stream. `update` is a union discriminated
// by `sessionUpdate`; like the Codex adapter's item type this is one permissive
// superset struct, because the variants share a discriminator and Rein reads a
// handful of fields from any of them.
type sessionUpdateNotification struct {
	SessionID string        `json:"sessionId"`
	Update    updatePayload `json:"update"`
}

type updatePayload struct {
	SessionUpdate string `json:"sessionUpdate"`

	// agent_message_chunk, agent_thought_chunk, user_message_chunk
	Content *contentBlock `json:"content"`

	// tool_call, tool_call_update — and session_info_update, which reuses
	// `title` for the session's generated name.
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`

	// plan
	Entries json.RawMessage `json:"entries"`

	// x.ai: response_completed
	Usage *responseUsage `json:"usage"`

	// x.ai: turn_completed
	StopReason string `json:"stop_reason"`

	// x.ai: last_turn_summary
	Summary string `json:"summary"`

	// x.ai: session_summary_generated
	SessionSummary string `json:"session_summary"`

	// x.ai: pending_interaction / interaction_resolved
	ToolCallIDSnake string `json:"tool_call_id"`

	// x.ai: model_changed
	ModelID string `json:"model_id"`
}

// responseUsage is one model call's accounting, from the x.ai
// `response_completed` notification. Snake_case, and — the part that matters —
// `input_tokens` **excludes** the cached read, where the turn total above
// includes it. Summing `input_tokens + cache_read_input_tokens` across the
// responses of a turn reproduces the turn's `inputTokens` exactly; summing
// `input_tokens` alone does not, and would under-report a cached turn by an
// order of magnitude. Verified against a recorded two-call turn: 4822+11776
// and 307+16512 sum to the turn's 33417.
type responseUsage struct {
	InputTokens             int64 `json:"input_tokens"`
	OutputTokens            int64 `json:"output_tokens"`
	CacheReadInputTokens    int64 `json:"cache_read_input_tokens"`
	CacheCreationInputToken int64 `json:"cache_creation_input_tokens"`
	ReasoningTokens         int64 `json:"reasoning_tokens"`
}

// sessionsChangedNotification is x.ai's session index. The `activity` field is
// the honest idle signal Rein's watchdog needs — "working" or "idle", reported
// by the agent rather than scraped from a spinner (docs/adapters.md).
type sessionsChangedNotification struct {
	Upserted []struct {
		SessionID string `json:"sessionId"`
		Activity  string `json:"activity"`
		Title     string `json:"title"`
	} `json:"upserted"`
}

// requestPermissionParams is ACP's approval request.
//
// **Unverified against the running agent.** In four probes on 2026-08-29 Grok
// never sent one: every permission gate resolved inside the agent, announced
// as a `pending_interaction` notification and closed by an
// `interaction_resolved` a few milliseconds later with no client round trip.
// The handler below is written to the ACP specification and the manifest
// declares `approvals: partial` in consequence. See docs/adapter-grok.md.
type requestPermissionParams struct {
	SessionID string              `json:"sessionId"`
	ToolCall  *permissionToolCall `json:"toolCall"`
	Options   []permissionOption  `json:"options"`
}

type permissionToolCall struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Kind       string          `json:"kind"`
	RawInput   json.RawMessage `json:"rawInput"`
}

// permissionOption is one button the agent is offering. `kind` is ACP's
// vocabulary: allow_once, allow_always, reject_once, reject_always.
type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

const (
	optAllowOnce    = "allow_once"
	optAllowAlways  = "allow_always"
	optRejectOnce   = "reject_once"
	optRejectAlways = "reject_always"
)

type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

type requestPermissionResponse struct {
	Outcome permissionOutcome `json:"outcome"`
}

const (
	outcomeSelected  = "selected"
	outcomeCancelled = "cancelled"
)

// pick chooses the option matching one of kinds, in the order given.
func pickOption(options []permissionOption, kinds ...string) (permissionOption, bool) {
	for _, want := range kinds {
		for _, o := range options {
			if o.Kind == want {
				return o, true
			}
		}
	}
	return permissionOption{}, false
}
