package codex

import "encoding/json"

// The subset of the Codex app-server protocol this adapter speaks.
//
// Hand-written from the JSON Schema the binary generates
// (`codex app-server generate-json-schema --out`, see schema/README.md), not
// from OpenAI's prose docs, which are stale. Every type here has a schema file
// checked in beside it; the schema is the reference, these structs are the
// subset Rein reads.
//
// Two rules govern the shapes:
//
//   - **Decode permissively.** Every notification struct is a superset view:
//     unknown fields are dropped, absent ones stay zero. Codex adds fields
//     between releases and an adapter that fails to decode a new one is an
//     adapter that stops working on the next `codex` upgrade.
//   - **Encode minimally.** Every optional request field is a pointer or has
//     `omitempty`, so a zero value means "leave the vendor's default alone"
//     rather than "set it to zero" — which is the difference between
//     inheriting the developer's model and silently forcing one.

// Method names. Requests we send, notifications we receive, and the requests
// the server sends us.
const (
	// Client → server requests.
	methodInitialize    = "initialize"
	methodThreadStart   = "thread/start"
	methodThreadResume  = "thread/resume"
	methodTurnStart     = "turn/start"
	methodTurnSteer     = "turn/steer"
	methodTurnInterrupt = "turn/interrupt"

	// methodRateLimitsRead reads the account's subscription windows. It costs
	// no model turn — a process start and one backend read — which is what
	// makes Codex the one adapter that can report headroom while idle
	// (ark:rein#40). Schema `GetAccountRateLimitsResponse`.
	methodRateLimitsRead = "account/rateLimits/read"

	// methodAccountRead says which login the app server is on — schema
	// `GetAccountResponse`. Read once per session, before the thread starts,
	// as the plan-login check (planauth.go).
	methodAccountRead = "account/read"

	// Client → server notification, sent once after initialize.
	methodInitialized = "initialized"

	// Server → client notifications the adapter interprets.
	notifyThreadStarted    = "thread/started"
	notifyThreadStatus     = "thread/status/changed"
	notifyThreadTokenUsage = "thread/tokenUsage/updated"
	notifyTurnStarted      = "turn/started"
	notifyTurnCompleted    = "turn/completed"
	notifyItemStarted      = "item/started"
	notifyItemCompleted    = "item/completed"
	notifyAgentMsgDelta    = "item/agentMessage/delta"
	notifyError            = "error"
	notifyRateLimits       = "account/rateLimits/updated"

	// Server → client requests. The first three are approvals the run loop
	// can answer; the rest this adapter declines, for the reasons in
	// docs/adapter-codex.md.
	reqCommandApproval     = "item/commandExecution/requestApproval"
	reqFileChangeApproval  = "item/fileChange/requestApproval"
	reqPermissionsApproval = "item/permissions/requestApproval"
	reqToolUserInput       = "item/tool/requestUserInput"
	reqMCPElicitation      = "mcpServer/elicitation/request"
	reqAuthTokensRefresh   = "account/chatgptAuthTokens/refresh"
	reqAttestation         = "attestation/generate"
	reqDynamicToolCall     = "item/tool/call"
	reqExecCommandApproval = "execCommandApproval" // legacy v1 spelling
	reqApplyPatchApproval  = "applyPatchApproval"  // legacy v1 spelling
)

// sandboxMode is Codex's filesystem policy — schema `SandboxMode`. The three
// values are the ones `codex exec -s` takes, and the reason
// elk `docs/rein.md` §3 calls `--full-auto` a footgun: this is the knob, and
// `--full-auto` is not a spelling of it.
type sandboxMode string

const (
	sandboxReadOnly       sandboxMode = "read-only"
	sandboxWorkspaceWrite sandboxMode = "workspace-write"
	sandboxFullAccess     sandboxMode = "danger-full-access"
)

// askForApproval is Codex's approval policy — schema `AskForApproval`, in its
// string form. (The schema also allows a `{granular: …}` object; Rein does not
// use it, because Rein's four permission modes do not have a granular tier and
// inventing one here would be vendor policy leaking into the contract.)
type askForApproval string

const (
	approvalUntrusted askForApproval = "untrusted"
	approvalOnRequest askForApproval = "on-request"
	approvalNever     askForApproval = "never"
)

// clientInfo identifies Rein to the app server. It ends up in the user agent
// Codex reports back, which is how a session in `~/.codex/sessions` can be
// told apart from one a human started.
type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

type initializeParams struct {
	ClientInfo clientInfo `json:"clientInfo"`
}

type initializeResponse struct {
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
	UserAgent      string `json:"userAgent"`
}

// threadStartParams starts a new thread. Everything but the cwd is optional,
// and an omitted field means Codex's own default.
type threadStartParams struct {
	Cwd                   string          `json:"cwd,omitempty"`
	Sandbox               sandboxMode     `json:"sandbox,omitempty"`
	ApprovalPolicy        askForApproval  `json:"approvalPolicy,omitempty"`
	Model                 string          `json:"model,omitempty"`
	DeveloperInstructions string          `json:"developerInstructions,omitempty"`
	Config                map[string]any  `json:"config,omitempty"`
	Ephemeral             *bool           `json:"ephemeral,omitempty"`
	Personality           json.RawMessage `json:"personality,omitempty"`
}

// threadResumeParams continues a thread by id. Same knobs as a start, plus the
// id; Codex re-reads the thread's history and applies these to the new turns.
type threadResumeParams struct {
	ThreadID              string         `json:"threadId"`
	Cwd                   string         `json:"cwd,omitempty"`
	Sandbox               sandboxMode    `json:"sandbox,omitempty"`
	ApprovalPolicy        askForApproval `json:"approvalPolicy,omitempty"`
	Model                 string         `json:"model,omitempty"`
	DeveloperInstructions string         `json:"developerInstructions,omitempty"`
	Config                map[string]any `json:"config,omitempty"`
}

// threadResponse is what both thread/start and thread/resume return. Only the
// fields Rein uses are named; `model` is here because the token-usage
// notifications do not carry one and Elk prices usage by model.
type threadResponse struct {
	Thread         thread `json:"thread"`
	Model          string `json:"model"`
	ModelProvider  string `json:"modelProvider"`
	Cwd            string `json:"cwd"`
	ApprovalPolicy string `json:"approvalPolicy"`
}

// thread is the vendor session. `ID` is what [adapter.RunSpec.ResumeID] takes
// and what Result.SessionID reports; `Path` is the rollout JSONL Codex writes
// under $CODEX_HOME, which becomes Result.TranscriptPath.
type thread struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionId"`
	Path       string `json:"path"`
	Cwd        string `json:"cwd"`
	CliVersion string `json:"cliVersion"`
	Turns      []turn `json:"turns"`
}

type threadStartedNotification struct {
	Thread thread `json:"thread"`
}

// threadStatus — schema `ThreadStatus`. `Type` is one of notLoaded, idle,
// systemError, active; `ActiveFlags` carries things like "waitingOnApproval".
// The idle status is the real signal behind [adapter.EventIdle]: Rein does not
// scrape a spinner (docs/adapters.md).
type threadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags"`
}

type threadStatusChangedNotification struct {
	ThreadID string       `json:"threadId"`
	Status   threadStatus `json:"status"`
}

type tokenUsageBreakdown struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

// threadTokenUsage carries a running `Total` for the thread and the `Last`
// model call. Rein's Usage events are increments, so the adapter differences
// `Total` against what it has already reported rather than forwarding `Last` —
// `Last` is per model call and a turn makes several.
type threadTokenUsage struct {
	Last               tokenUsageBreakdown `json:"last"`
	Total              tokenUsageBreakdown `json:"total"`
	ModelContextWindow int64               `json:"modelContextWindow"`
}

type threadTokenUsageNotification struct {
	ThreadID   string           `json:"threadId"`
	TurnID     string           `json:"turnId"`
	TokenUsage threadTokenUsage `json:"tokenUsage"`
}

// turnStatus — schema `TurnStatus`.
const (
	turnCompleted  = "completed"
	turnInterrupt  = "interrupted"
	turnFailed     = "failed"
	turnInProgress = "inProgress"
)

type turnError struct {
	Message           string `json:"message"`
	AdditionalDetails string `json:"additionalDetails"`
}

type turn struct {
	ID          string       `json:"id"`
	Status      string       `json:"status"`
	Items       []threadItem `json:"items"`
	Error       *turnError   `json:"error"`
	StartedAt   int64        `json:"startedAt"`
	CompletedAt int64        `json:"completedAt"`
	DurationMs  int64        `json:"durationMs"`
}

type turnStartParams struct {
	ThreadID string      `json:"threadId"`
	Input    []userInput `json:"input"`
	Model    string      `json:"model,omitempty"`
	// Effort is schema `ReasoningEffort`: "override the reasoning effort for
	// this turn and subsequent turns". Empty leaves the model's default.
	Effort string `json:"effort,omitempty"`
	Cwd    string `json:"cwd,omitempty"`
}

// accountReadParams — schema `GetAccountParams`. No refresh: the check reads
// what the app server already holds.
type accountReadParams struct{}

// accountReadResponse — schema `GetAccountResponse`, the subset the check
// reads. `account.type` is "chatgpt" on the plan login, "apiKey" on an API
// key, "amazonBedrock" on Bedrock; null when there is no login at all.
type accountReadResponse struct {
	Account *struct {
		Type     string `json:"type"`
		PlanType string `json:"planType"`
	} `json:"account"`
}

type turnStartResponse struct {
	Turn turn `json:"turn"`
}

type turnNotification struct {
	ThreadID string `json:"threadId"`
	Turn     turn   `json:"turn"`
}

type turnSteerParams struct {
	ThreadID       string      `json:"threadId"`
	ExpectedTurnID string      `json:"expectedTurnId"`
	Input          []userInput `json:"input"`
}

type turnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// userInput is one prompt element — schema `UserInput`. Rein only ever sends
// text; images, audio, skills and mentions exist in the protocol and have no
// Rein equivalent.
type userInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func textInput(s string) []userInput { return []userInput{{Type: "text", Text: s}} }

// Item types — the `type` discriminator on schema `ThreadItem`.
const (
	itemUserMessage      = "userMessage"
	itemAgentMessage     = "agentMessage"
	itemReasoning        = "reasoning"
	itemPlan             = "plan"
	itemCommandExecution = "commandExecution"
	itemFileChange       = "fileChange"
	itemMCPToolCall      = "mcpToolCall"
	itemDynamicToolCall  = "dynamicToolCall"
	itemWebSearch        = "webSearch"
)

// threadItem is the flattened union of schema `ThreadItem`. One struct rather
// than eighteen because the variants share `id` and `type` and Rein reads at
// most three fields from any of them; a field that belongs to another variant
// simply stays zero.
type threadItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`

	// agentMessage, plan
	Text  string `json:"text"`
	Phase string `json:"phase"` // "final_answer" | "commentary" | …

	// commandExecution
	Command    string `json:"command"`
	Cwd        string `json:"cwd"`
	ExitCode   *int   `json:"exitCode"`
	DurationMs *int64 `json:"durationMs"`

	// commandExecution, fileChange, mcpToolCall, dynamicToolCall
	Status string `json:"status"`

	// mcpToolCall, dynamicToolCall
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`

	// fileChange
	Changes json.RawMessage `json:"changes"`

	// webSearch
	Query string `json:"query"`
}

type itemNotification struct {
	ThreadID      string     `json:"threadId"`
	TurnID        string     `json:"turnId"`
	Item          threadItem `json:"item"`
	StartedAtMs   int64      `json:"startedAtMs"`
	CompletedAtMs int64      `json:"completedAtMs"`
}

type agentMessageDeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

// errorNotification is a turn-level error. `WillRetry` matters: Codex reports
// a retriable failure and then retries it, so an adapter that treats every one
// of these as terminal ends sessions that were about to succeed.
type errorNotification struct {
	ThreadID  string    `json:"threadId"`
	TurnID    string    `json:"turnId"`
	Error     turnError `json:"error"`
	WillRetry bool      `json:"willRetry"`
}

// ------------------------------------------------------------ rate limits ---

// rateLimitsReadParams — schema `GetAccountRateLimitsParams`. A background
// poll skips the reset-credit detail lookup, which the schema says is exactly
// what the flag is for.
type rateLimitsReadParams struct {
	ExcludeResetCreditDetails bool `json:"excludeResetCreditDetails,omitempty"`
}

// rateLimitsReadResponse — schema `GetAccountRateLimitsResponse`, the subset
// Rein reads. Verified against codex-cli 0.159.0 on a ChatGPT Pro login on
// 2026-09-29: one `primary` window of 10080 minutes (weekly), no `secondary`,
// `ordinaryUsageAllowed: true`, answered 0.4 s after initialize.
type rateLimitsReadResponse struct {
	// OrdinaryUsageAllowed is the backend's own yes or no. Null means
	// unavailable, and the schema is explicit that a client "must not infer
	// recovery from percentages or reset times" — so a false here holds a
	// queue until a later read says true, whatever the clock says.
	OrdinaryUsageAllowed *bool             `json:"ordinaryUsageAllowed"`
	RateLimits           rateLimitSnapshot `json:"rateLimits"`
}

// rateLimitsUpdatedNotification — schema
// `AccountRateLimitsUpdatedNotification`, which Codex sends during a turn as
// the backend's headers move.
type rateLimitsUpdatedNotification struct {
	RateLimits rateLimitSnapshot `json:"rateLimits"`
}

// rateLimitSnapshot — schema `RateLimitSnapshot`.
type rateLimitSnapshot struct {
	LimitID   string           `json:"limitId"`
	PlanType  string           `json:"planType"`
	Primary   *rateLimitWindow `json:"primary"`
	Secondary *rateLimitWindow `json:"secondary"`
	Credits   *creditsSnapshot `json:"credits"`

	// RateLimitReachedType is set when a limit has been hit —
	// "rate_limit_reached", or one of the workspace credit/usage variants.
	// Any value means ordinary usage is stopped.
	RateLimitReachedType string `json:"rateLimitReachedType"`
}

// rateLimitWindow — schema `RateLimitWindow`. usedPercent is an integer
// percentage; resetsAt is unix seconds.
type rateLimitWindow struct {
	UsedPercent        int64  `json:"usedPercent"`
	WindowDurationMins *int64 `json:"windowDurationMins"`
	ResetsAt           *int64 `json:"resetsAt"`
}

// creditsSnapshot — schema `CreditsSnapshot`. Credits are what keeps a Codex
// account working past a full window, the way extra usage does for Claude.
type creditsSnapshot struct {
	HasCredits bool `json:"hasCredits"`
	Unlimited  bool `json:"unlimited"`
}

// Approval requests. The three shapes differ but Rein's PermissionRequest is
// one shape, so approvalRequest is the common view the translator builds.
type commandApprovalParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Command  string `json:"command"`
	Cwd      string `json:"cwd"`
	Reason   string `json:"reason"`
}

type fileChangeApprovalParams struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	ItemID    string `json:"itemId"`
	Reason    string `json:"reason"`
	GrantRoot string `json:"grantRoot"`
}

type permissionsApprovalParams struct {
	ThreadID    string          `json:"threadId"`
	TurnID      string          `json:"turnId"`
	ItemID      string          `json:"itemId"`
	Cwd         string          `json:"cwd"`
	Reason      string          `json:"reason"`
	Permissions json.RawMessage `json:"permissions"`
}

// Decision values for the two decision-shaped approval responses — schema
// `CommandExecutionApprovalDecision` and `FileChangeApprovalDecision`.
const (
	decisionAccept  = "accept"
	decisionDecline = "decline"
)

type decisionResponse struct {
	Decision string `json:"decision"`
}

// permissionsApprovalResponse answers `item/permissions/requestApproval`.
// There is no "decline" in this shape — the response grants a profile — so a
// denial is an *empty* grant, which is what "you may do nothing extra" means
// here. See docs/adapter-codex.md.
type permissionsApprovalResponse struct {
	Permissions map[string]any `json:"permissions"`
}
