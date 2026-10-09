package elk

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The executor tools Rein calls. Named constants because three of them are
// also probed by name through [Client.HasTool].
const (
	ToolConnectExecutor   = "connect_executor"
	ToolClaimRun          = "claim_run"
	ToolReportProgress    = "report_progress"
	ToolAskElk            = "ask_elk"
	ToolSubmitDeliverable = "submit_deliverable"
	ToolHeartbeatExecutor = "heartbeat_executor"
	ToolListExecutors     = "list_executors"
	ToolListWorkspaces    = "list_workspaces"
	ToolListActions       = "list_actions"
)

// QueueNameMaxRunes is Elk's cap on a queue name, counted in Unicode scalars
// (`queueNameLength`, _shared/connected_executors.ts). Ruling 3 in elk
// docs/rein.md raised it from 10 to 12 and made an over-long name a rejection
// rather than a truncation — because the name is the address
// `claim_run(queue: …)` resolves, and two names cut to the same prefix would
// be one queue draining two machines' work.
const QueueNameMaxRunes = 12

// AgentKinds is Elk's closed `agent_kind` enum. A value outside it is refused
// at `connect_executor`, and refusing locally first means a mistyped kind
// costs a sentence rather than a spent claim code.
var AgentKinds = []string{"claude", "codex", "grok", "other"}

// KnownAgentKind reports whether kind is one Elk will accept.
func KnownAgentKind(kind string) bool {
	for _, k := range AgentKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- errors

// ErrCancelled reports that a run Rein holds is no longer running — the user
// cancelled it, or it reached a terminal status some other way.
//
// It is the most important signal on the whole connection and the easiest to
// miss, because Elk does NOT send it as an error. `report_progress` and
// `submit_deliverable` against a run that is not running match zero rows and
// answer with plain text and no `isError` at all — deliberately, so a
// heartbeat "must always stay open" (elk-mcp/queue.ts:952). A client that only
// checks `isError` sees a success and keeps burning tokens on work a human
// dropped.
//
// Match it with [errors.Is]; the value is a [*NotRunningError] carrying the
// status Elk named.
var ErrCancelled = errors.New("elk: this run is no longer running")

// ErrNoWork reports an empty queue: a `claim_run` that found nothing to claim.
// It is the ordinary case in a polling loop, not a failure — but it is a
// sentinel rather than a nil result so that a caller cannot fall through into
// driving a run it never claimed.
var ErrNoWork = errors.New("elk: queue is clear")

// NotRunningError is the cancellation signal, with what Elk said about it.
type NotRunningError struct {
	// RunID is the run Rein was reporting on.
	RunID string
	// Status is the status Elk named — "cancelled", "done", "stuck", … — or
	// "" when the reply did not name one.
	Status string
	// Cancelled distinguishes a user cancelling mid-run from a run that had
	// already reached a terminal status. Both mean stop; only one means
	// somebody changed their mind.
	Cancelled bool
	// Text is Elk's own sentence, worth logging verbatim.
	Text string
}

func (e *NotRunningError) Error() string {
	switch {
	case e.Cancelled:
		return fmt.Sprintf("elk: run %s was cancelled by the user", e.RunID)
	case e.Status != "":
		return fmt.Sprintf("elk: run %s is %s, not running", e.RunID, e.Status)
	}
	return fmt.Sprintf("elk: run %s is not running: %s", e.RunID, e.Text)
}

// Is makes [errors.Is] match [ErrCancelled].
func (e *NotRunningError) Is(target error) bool { return target == ErrCancelled }

// AwaitingReview reports the one case where "not running" does NOT mean stop:
// a run that Rein has just submitted under Elk's auto-review sits at `ready`
// while the review pass thinks, and both `report_progress` and `ask_elk`
// answer it with the same "Not recorded:" sentence they use for a cancelled
// run (elk-mcp/queue.ts — both run the guarded touch first).
//
// The status in the sentence is the only thing telling them apart. A caller
// that is waiting for a review must check this before treating
// [ErrCancelled] as final, or it will discard finished work every time.
func (e *NotRunningError) AwaitingReview() bool {
	return !e.Cancelled && e.Status == "ready"
}

// TierRefusalError reports a token whose tier is too low for the tool it was
// used on. Rein meets exactly one of these in normal life: `connect_executor`
// needs the `interactive` tier, while an invited agent's durable token is
// `run-scoped` — so a re-enrolment with the stored token is refused, and the
// only `connect_executor` such a token may make is the one carrying a
// `claim_code` (elk-mcp/handler.ts:638-641).
type TierRefusalError struct {
	Tool     string
	Required string
	Held     string
	Text     string
}

func (e *TierRefusalError) Error() string {
	return fmt.Sprintf("elk: %s needs a %s token; this one is %s", e.Tool, e.Required, e.Held)
}

var tierRefusalRE = regexp.MustCompile(
	`^Refused ([a-z_]+): it requires the (\S+) connector-token tier, but this token holds (\S+)\.`)

// asTierRefusal returns a [*TierRefusalError] when the reply is one, else nil.
func asTierRefusal(res *ToolResult) *TierRefusalError {
	if res == nil || !res.IsError {
		return nil
	}
	m := tierRefusalRE.FindStringSubmatch(res.Text)
	if m == nil {
		return nil
	}
	return &TierRefusalError{Tool: m[1], Required: m[2], Held: m[3], Text: res.Text}
}

// call runs a tool and turns a tier refusal into a typed error, because that
// one refusal has a specific remedy and every caller wants to name it.
func (c *Client) call(ctx context.Context, name string, args map[string]any) (*ToolResult, error) {
	res, err := c.CallTool(ctx, name, args)
	if err != nil {
		return nil, err
	}
	if t := asTierRefusal(res); t != nil {
		return res, t
	}
	return res, nil
}

// ---------------------------------------------------------------- usage

// Usage is one spend delta, in Elk's `usage` shape. It is a DELTA since the
// last report on this run, never a running total: Elk sums.
//
// Elk prices it from its own rate card, so Rein reports counts and the real
// vendor model id and never money. A malformed usage never fails the call —
// Elk appends "Usage not recorded: <reason>." to an otherwise successful
// reply — which is exactly why [Usage.Valid] exists: a silently dropped
// number is worse than an omitted one.
type Usage struct {
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Model        string `json:"model"`
	Provider     string `json:"provider"`

	CostUSD          float64 `json:"cost_usd,omitempty"`
	CacheReadTokens  int64   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64   `json:"reasoning_tokens,omitempty"`

	// HumanPromptTokens and HumanPrompts are the human half of the same
	// ledger. Rein always sends them and always sends zero: nobody typed
	// anything at a queue-driven run, and Elk distinguishes a reported zero
	// from an omitted field. They carry no omitempty for that reason.
	HumanPromptTokens int64 `json:"human_prompt_tokens"`
	HumanPrompts      int64 `json:"human_prompts"`
}

// Valid reports whether Elk will accept this usage. The four fields Elk
// requires are the four checked here; a usage that fails is omitted from the
// call rather than sent and silently dropped.
func (u *Usage) Valid() bool {
	return u != nil && u.InputTokens >= 0 && u.OutputTokens >= 0 &&
		strings.TrimSpace(u.Model) != "" && strings.TrimSpace(u.Provider) != ""
}

// Add accumulates u2 into u, so a run loop can aggregate an adapter's usage
// events between heartbeats and report one delta.
func (u *Usage) Add(u2 Usage) {
	u.InputTokens += u2.InputTokens
	u.OutputTokens += u2.OutputTokens
	u.CacheReadTokens += u2.CacheReadTokens
	u.CacheWriteTokens += u2.CacheWriteTokens
	u.ReasoningTokens += u2.ReasoningTokens
	u.CostUSD += u2.CostUSD
	if u2.Model != "" {
		u.Model = u2.Model
	}
	if u2.Provider != "" {
		u.Provider = u2.Provider
	}
}

// Empty reports whether anything was actually spent.
func (u *Usage) Empty() bool {
	return u == nil || (u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 && u.ReasoningTokens == 0)
}

func (u *Usage) put(args map[string]any) {
	if u.Valid() {
		args["usage"] = u
	}
}

// ---------------------------------------------------------- connect_executor

// ConnectRequest binds this machine to an Elk workspace as one named queue.
type ConnectRequest struct {
	// Workspace names the Elk workspace. Ignored on the claim-code path — the
	// claim carries its own.
	Workspace string

	// DisplayName is what the queue is called on the Connected agents page,
	// and the queue address when Queue is empty. Capped like a queue name.
	DisplayName string

	// Queue is the queue address `claim_run(queue: …)` resolves. Ignored on
	// the claim-code path.
	Queue string

	// HostID is the durable half of this executor's identity, with AgentKind.
	// A value that survives a hostname change — not the hostname.
	HostID string

	// AgentKind is one of [AgentKinds].
	AgentKind string

	// DeclaredCapabilities are NAMES of tools and credentials this machine
	// holds. Never values: Elk is not a credential broker.
	DeclaredCapabilities []string

	// ClaimCode is the single-use invite code. On this path the same code is
	// also the bearer token — an invite is a `bootstrap`-tier connector token,
	// and `connect_executor` carrying a claim_code is the one call that tier
	// may make.
	ClaimCode string

	// SelfConfigure asks for the durable credential back in the reply instead
	// of leaving a human to reveal it in Settings. Rein asserts it because it
	// puts the token straight into the OS keychain and never prints, logs or
	// repeats it — which is the whole condition Elk attaches to the flag.
	SelfConfigure bool

	// Shared opens this queue to every member of the workspace instead of
	// keeping it private to the person whose credential connected it (Elk's
	// scout migration 0201). False is Elk's own default, and false sends no
	// `shared` key at all — an Elk that predates 0201 must not be handed an
	// argument it does not know.
	//
	// Ignored on the claim-code path, where the inviting person already chose
	// on the invite sheet; Elk says so in the reply rather than failing, the
	// same way it does for a Workspace or Queue sent alongside a claim. Rein
	// passes it anyway so that sentence reaches the person who typed
	// --shared, rather than the flag disappearing in silence.
	Shared bool
}

// ConnectResult is what an enrolment learned.
type ConnectResult struct {
	// Workspace is the workspace named by the claim exchange, when supplied.
	Workspace string
	// Queue is the queue name Elk actually bound — on the claim-code path
	// this comes from the claim, not from the request.
	Queue string

	// ExecutorID is the `exec-<uuid>` row id.
	ExecutorID string

	// Token is the durable run-scoped token, present only on a
	// self-configuring claim exchange. Treat it as a secret: it goes to the
	// keychain and nowhere else.
	Token string

	// Text is Elk's reply, with the token already removed if there was one —
	// safe to print.
	Text string
}

var (
	connectWorkspaceRE = regexp.MustCompile(`workspace "([^"]+)"`)
	connectQueueRE     = regexp.MustCompile(`queue "([^"]+)"`)
	connectExecutorRE  = regexp.MustCompile("executor `([^`]+)`")
	connectorURLRE     = regexp.MustCompile(`durable connector URL: (https://\S+)`)
)

// ConnectExecutor binds a queue, and on the claim-code path exchanges the
// invite for a durable run-scoped token.
//
// Validation happens here as well as on the server for one reason: the
// claim-code path CONSUMES the invite before it binds anything, so a queue
// name one byte too long or an agent kind spelled "Claude" would cost the
// person who sent the invite a fresh one. Elk checks identity before it
// consumes for the same reason; Rein checking first means the round trip never
// happens.
func (c *Client) ConnectExecutor(ctx context.Context, req ConnectRequest) (*ConnectResult, error) {
	if strings.TrimSpace(req.DisplayName) == "" {
		return nil, errors.New("elk: connect_executor needs a display_name")
	}
	for _, name := range []struct{ what, v string }{
		{"display_name", req.DisplayName}, {"queue", req.Queue},
	} {
		if n := utf8.RuneCountInString(name.v); n > QueueNameMaxRunes {
			return nil, fmt.Errorf("elk: %s %q is %d characters; Elk's cap is %d and an over-long name is refused, not shortened",
				name.what, name.v, n, QueueNameMaxRunes)
		}
	}
	if req.AgentKind != "" && !KnownAgentKind(req.AgentKind) {
		return nil, fmt.Errorf("elk: agent_kind %q is not one Elk knows; use one of %s",
			req.AgentKind, strings.Join(AgentKinds, ", "))
	}

	args := map[string]any{"display_name": req.DisplayName}
	putIf(args, "workspace", req.Workspace)
	putIf(args, "queue", req.Queue)
	putIf(args, "host_id", req.HostID)
	putIf(args, "agent_kind", req.AgentKind)
	if len(req.DeclaredCapabilities) > 0 {
		args["declared_capabilities"] = req.DeclaredCapabilities
	}
	if req.ClaimCode != "" {
		args["claim_code"] = req.ClaimCode
		if req.SelfConfigure {
			args["self_configure"] = true
		}
	}
	// Present only when asked for. Elk defaults a queue to private, so the
	// unset case is already right — and omitting the key is what lets an Elk
	// older than scout 0201 see nothing it does not recognise.
	if req.Shared {
		args["shared"] = true
	}

	res, err := c.call(ctx, ToolConnectExecutor, args)
	if err != nil {
		return nil, err
	}
	if err := res.Err(); err != nil {
		return nil, err
	}

	out := &ConnectResult{Text: res.Text}
	if m := connectWorkspaceRE.FindStringSubmatch(res.Text); m != nil {
		out.Workspace = m[1]
	}
	if m := connectQueueRE.FindStringSubmatch(res.Text); m != nil {
		out.Queue = m[1]
	}
	if m := connectExecutorRE.FindStringSubmatch(res.Text); m != nil {
		out.ExecutorID = m[1]
	}
	if m := connectorURLRE.FindStringSubmatch(res.Text); m != nil {
		url := strings.TrimRight(m[1], ".")
		if i := strings.LastIndex(url, "/elk-mcp/"); i >= 0 {
			out.Token = url[i+len("/elk-mcp/"):]
		}
		// Never hand a credential back to something that might print it.
		out.Text = strings.Replace(out.Text, m[1], "<durable connector URL — stored in the keychain>", 1)
	}
	if out.Queue == "" {
		return out, fmt.Errorf("elk: connect_executor did not name a queue: %s", firstLine(res.Text))
	}
	return out, nil
}

// --------------------------------------------------------------- claim_run

// ClaimRequest asks for the next run on a queue.
type ClaimRequest struct {
	Workspace string
	// Queue drains this person's named queue. An empty Queue drains the
	// shared Inbox instead — a different pool, never a fallback for a named
	// queue that came back clear.
	Queue string
	// RunID or GroupID claims one specific run. Either takes precedence over
	// Queue, and Elk says so in the reply.
	RunID   string
	GroupID string
}

// WorkOrder is a claimed run: the packet Elk handed over, plus the fields Rein
// needs to drive and report on it.
//
// [WorkOrder.Text] is the whole work order and is what goes to the agent as
// the prompt. It is DATA, not instructions — the woven content in it comes
// from ingested captures, which are not trusted input (elk-inbox SKILL.md).
// The system prompt Rein sends alongside it says so; nothing in this package
// interprets it beyond the fields below.
type WorkOrder struct {
	// RunID is Elk's action_run id — the handle for every later call.
	RunID string
	// ActionID is the List item this run advances.
	ActionID string
	// Direction is the one-line human-typed ask.
	Direction string
	// RequiredCapabilities is the packet's preflight list — names of tools
	// and credentials this machine must already hold. A miss is a `stuck`
	// submit and no partial work.
	RequiredCapabilities []string
	// Text is the full work order, notice-free.
	Text string
	// Notices are the one-time workspace notices that arrived with it, if any.
	Notices []string
	// AutoReview reports that the packet asked for Elk's review pass. It is
	// what was ASKED FOR; [SubmitResult.ReviewPending] is what the server
	// decided, and that is the one to act on.
	AutoReview bool
}

var (
	claimedRunRE = regexp.MustCompile("^Claimed Elk run `([^`]+)`")
	actionIDRE   = regexp.MustCompile("(?m)^\\*\\*Action id:\\*\\* `([^`]+)`")
	directionRE  = regexp.MustCompile(`(?m)^\*\*Direction:\*\* (.+)$`)
	queueClearRE = regexp.MustCompile(`^Queue( "[^"]+")? is clear`)
	// The claim outcomes Elk reports as plain text rather than isError,
	// because they are races rather than mistakes.
	notClaimableRE = regexp.MustCompile("^(Run `[^`]+` is not claimable|Nothing left to claim in group)")
)

// ClaimRun claims the next run on a queue.
//
// It returns [ErrNoWork] when the queue is clear, and when a race left nothing
// to claim — both are Elk answering in plain text with no `isError`, and both
// mean the same thing to a poll loop: nothing to do, come back later.
func (c *Client) ClaimRun(ctx context.Context, req ClaimRequest) (*WorkOrder, error) {
	args := map[string]any{}
	putIf(args, "workspace", req.Workspace)
	putIf(args, "queue", req.Queue)
	putIf(args, "run_id", req.RunID)
	putIf(args, "group_id", req.GroupID)

	res, err := c.call(ctx, ToolClaimRun, args)
	if err != nil {
		return nil, err
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	if queueClearRE.MatchString(res.Text) || notClaimableRE.MatchString(res.Text) {
		return nil, fmt.Errorf("%w: %s", ErrNoWork, firstLine(res.Text))
	}

	m := claimedRunRE.FindStringSubmatch(res.Text)
	if m == nil {
		return nil, fmt.Errorf("elk: claim_run answered something Rein does not understand: %s", firstLine(res.Text))
	}
	wo := &WorkOrder{RunID: m[1], Text: res.Text, Notices: res.Notices}
	if a := actionIDRE.FindStringSubmatch(res.Text); a != nil {
		wo.ActionID = a[1]
	}
	if d := directionRE.FindStringSubmatch(res.Text); d != nil {
		wo.Direction = strings.TrimSpace(d[1])
	}
	wo.RequiredCapabilities = parseRequiredCapabilities(res.Text)
	wo.AutoReview = strings.Contains(res.Text, AutoReviewLine)
	return wo, nil
}

// requiredCapsHeading is the section Elk renders the packet's
// `required_capabilities` under (elk-mcp/queue.ts:740-748). The em dash is
// U+2014 and is part of the literal.
const requiredCapsHeading = "### Required capabilities — preflight BEFORE starting"

// parseRequiredCapabilities pulls the preflight list out of a work order.
//
// It is a text section because that is all Elk sends: the work order is one
// markdown string and `claim_run` has no structured content. Rein re-checks
// the names it finds against the adapter manifest before creating a worktree,
// so a heading Elk renames would show up as an empty list — which fails OPEN.
// That is the argument for pinning the heading here as a constant and testing
// it: the parse is the only place the fail-closed check gets its input.
func parseRequiredCapabilities(text string) []string {
	_, after, found := strings.Cut(text, requiredCapsHeading)
	if !found {
		return nil
	}
	var out []string
	for _, line := range strings.Split(after, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#"):
			return out // the next section
		case strings.HasPrefix(line, "- "):
			if v := strings.TrimSpace(line[2:]); v != "" {
				out = append(out, v)
			}
		case strings.HasPrefix(line, "Fail fast:"):
			return out
		}
	}
	return out
}

// --------------------------------------------------------- report_progress

// ProgressKind is `report_progress`'s `kind`.
type ProgressKind string

const (
	// ProgressStep is narration — the default.
	ProgressStep ProgressKind = "step"
	// ProgressTool is a tool or command the agent ran.
	ProgressTool ProgressKind = "tool"
	// ProgressQuestion is something the agent needs from the user.
	ProgressQuestion ProgressKind = "question"
	// ProgressGate needs human approval before continuing — an explicit
	// human-in-the-loop stop, not a blocker.
	ProgressGate ProgressKind = "gate"
)

// Valid reports whether k is one of Elk's four kinds.
func (k ProgressKind) Valid() bool {
	switch k {
	case ProgressStep, ProgressTool, ProgressQuestion, ProgressGate:
		return true
	}
	return false
}

// Progress is one heartbeat.
type Progress struct {
	RunID string
	Body  string
	Kind  ProgressKind
	Usage *Usage
}

// ReportProgress heartbeats a claimed run and renews its 15-minute lease.
//
// It returns an error wrapping [ErrCancelled] when Elk answers "Not recorded:"
// — the run was cancelled or has otherwise stopped running. That answer is a
// plain-text success as far as MCP is concerned, which is the trap this
// wrapper exists to close.
func (c *Client) ReportProgress(ctx context.Context, p Progress) (*ToolResult, error) {
	if p.RunID == "" {
		return nil, errors.New("elk: report_progress needs a run id")
	}
	if strings.TrimSpace(p.Body) == "" {
		return nil, errors.New("elk: report_progress needs a body")
	}
	if p.Kind != "" && !p.Kind.Valid() {
		return nil, fmt.Errorf("elk: progress kind %q: want step, tool, question or gate", p.Kind)
	}
	args := map[string]any{"run_id": p.RunID, "body": p.Body}
	if p.Kind != "" {
		args["kind"] = string(p.Kind)
	}
	p.Usage.put(args)

	res, err := c.call(ctx, ToolReportProgress, args)
	if err != nil {
		return nil, err
	}
	if nr := asNotRunning(p.RunID, res, "Not recorded: "); nr != nil {
		return res, nr
	}
	return res, res.Err()
}

// notRunningStatusRE pulls the status out of "This run is `done`, not
// running." and "it is `done`, not running.".
var notRunningStatusRE = regexp.MustCompile("is `([a-z_]+)`, not running")

// asNotRunning recognises Elk's zero-row answer.
//
// The two shapes differ by more than their prefix — `report_progress` says
// "Not recorded: The user cancelled this run — STOP work on it now." and
// `submit_deliverable` says "Not saved: the user cancelled it mid-run.
// Nothing was persisted." — so the prefix is passed in and the rest is matched
// case-insensitively on the one phrase both share.
func asNotRunning(runID string, res *ToolResult, prefix string) *NotRunningError {
	if res == nil || res.IsError || !strings.HasPrefix(res.Text, prefix) {
		return nil
	}
	e := &NotRunningError{RunID: runID, Text: res.Text}
	if strings.Contains(strings.ToLower(res.Text), "cancelled") {
		e.Cancelled = true
		e.Status = "cancelled"
	}
	if m := notRunningStatusRE.FindStringSubmatch(res.Text); m != nil {
		e.Status = m[1]
	}
	return e
}

// ------------------------------------------------------------ auto-review

// AutoReviewLine is the marching-orders line Elk renders into a work order
// when the run is flagged for its own review pass. Byte-identical to
// AUTO_REVIEW_LINE in elk-mcp/queue.ts:137 and harness/packet.mjs; a run
// carrying it will not reach the user straight from the agent.
const AutoReviewLine = "Auto-review by Elk is ON for this run: report to Elk with report_progress at " +
	"every critical review point — after planning, before anything irreversible, and before submitting — " +
	"and expect Elk to review the deliverable before the user sees it."

// reviewPendingMarker appears in `submit_deliverable`'s reply when Elk has
// parked the run for its review pass (elk-mcp/queue.ts:2075).
//
// This, not the work order, is what Rein acts on. The packet's `auto_review`
// is what was asked for; the submit reply is what the SERVER decided, after
// its own coercion of `done` and `ready` — and only the server knows.
const reviewPendingMarker = "Auto-review by Elk is set on this run"

// The frame Elk wraps a revision request in when `ask_elk` is polled with no
// question (elk-mcp/queue.ts:1855-1861). The verdict is between them.
const (
	reviewRevisionsIntro = "needs revisions before the user sees it:"
	reviewRevisionsOutro = "The run is `running` again under your claim"
)

// AskResult is what a poll or a question came back with.
type AskResult struct {
	// Text is Elk's whole reply.
	Text string

	// Revisions is Elk's review verdict when the reply is a revision request,
	// else "". A non-empty value means the run has been reopened to `running`
	// under the same claim and is waiting for a corrected deliverable on the
	// same run id.
	Revisions string
}

// AskElk asks Elk a question on a run, or — with an empty question — polls for
// anything unread on it.
//
// The poll is how Rein learns that Elk's review pass wants changes. It is also
// a heartbeat: `ask_elk` runs the same guarded touch `report_progress` does, so
// polling a reopened run keeps its lease alive at the same time.
//
// A run that is not running comes back as a [*NotRunningError] matching
// [ErrCancelled] — but see [NotRunningError.AwaitingReview], because a run
// waiting for its review is `ready` and answers with the same sentence.
func (c *Client) AskElk(ctx context.Context, runID, question string) (*AskResult, error) {
	if runID == "" {
		return nil, errors.New("elk: ask_elk needs a run id")
	}
	args := map[string]any{"run_id": runID}
	putIf(args, "question", question)

	res, err := c.call(ctx, ToolAskElk, args)
	if err != nil {
		return nil, err
	}
	if nr := asNotRunning(runID, res, "Not recorded: "); nr != nil {
		return nil, nr
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return &AskResult{Text: res.Text, Revisions: parseRevisions(res.Text)}, nil
}

// parseRevisions pulls Elk's verdict out of a revision request, or returns ""
// when the reply is anything else.
func parseRevisions(text string) string {
	_, after, found := strings.Cut(text, reviewRevisionsIntro)
	if !found {
		return ""
	}
	if before, _, ok := strings.Cut(after, reviewRevisionsOutro); ok {
		after = before
	}
	return strings.TrimSpace(after)
}

// ------------------------------------------------------- submit_deliverable

// SubmitStatus is `submit_deliverable`'s `status`.
type SubmitStatus string

const (
	// StatusReady — a human should review this. The runner's normal terminal
	// state: Rein does not own the List item, so it is not Rein's to close.
	StatusReady SubmitStatus = "ready"
	// StatusDone — no review needed. Elk coerces it to ready unless the
	// caller owns the action, so a runner asking for it will usually get
	// ready back anyway; ask for ready and mean it.
	StatusDone SubmitStatus = "done"
	// StatusStuck — blocked, with the blocker explained in the deliverable.
	// A missing required capability is this, with no partial work.
	StatusStuck SubmitStatus = "stuck"
)

// Valid reports whether s is one of Elk's three statuses. `cancelled` and
// `failed` are deliberately absent: they are not an agent's to set.
func (s SubmitStatus) Valid() bool {
	switch s {
	case StatusReady, StatusDone, StatusStuck:
		return true
	}
	return false
}

// Link is a `links[]` entry on a deliverable. Elk accepts https only.
type Link struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Submission finishes a run.
type Submission struct {
	RunID       string
	Deliverable string
	Status      SubmitStatus
	Title       string
	Summary     string
	// CompletedActions are OTHER List items this run finished. Elk caps it at
	// 20 silently.
	CompletedActions []string
	Links            []Link
	Usage            *Usage
}

// SubmitResult is what Elk recorded.
type SubmitResult struct {
	// Status is the status Elk actually saved, which is not always the one
	// asked for: `done` becomes `ready` when the caller does not own the List
	// item, when it has no owner, or when another run on it is still active.
	// Read it from here rather than assuming.
	Status SubmitStatus

	// ReviewPending reports that Elk has parked this run for its own review
	// pass. The run is `ready` now, but it is not finished: the pass may
	// reopen it to `running` with a revision request, and whoever submitted is
	// expected to still be there. A runner that exits here strands the run.
	ReviewPending bool

	Text string
}

var submittedRE = regexp.MustCompile("^Deliverable saved: run `[^`]+` is `([a-z]+)`")

// SubmitDeliverable finishes a run.
//
// Like [Client.ReportProgress] it returns an error wrapping [ErrCancelled]
// when Elk answers "Not saved:" — the user cancelled at the last moment and
// nothing persisted.
func (c *Client) SubmitDeliverable(ctx context.Context, s Submission) (*SubmitResult, error) {
	if s.RunID == "" {
		return nil, errors.New("elk: submit_deliverable needs a run id")
	}
	if strings.TrimSpace(s.Deliverable) == "" {
		return nil, errors.New("elk: submit_deliverable needs a deliverable")
	}
	if !s.Status.Valid() {
		return nil, fmt.Errorf("elk: submit status %q: want ready, done or stuck", s.Status)
	}
	for _, l := range s.Links {
		if strings.TrimSpace(l.Title) == "" || !strings.HasPrefix(l.URL, "https://") {
			return nil, fmt.Errorf("elk: link %q → %q: needs a title and an https url", l.Title, l.URL)
		}
	}
	args := map[string]any{
		"run_id":      s.RunID,
		"deliverable": s.Deliverable,
		"status":      string(s.Status),
	}
	putIf(args, "title", s.Title)
	putIf(args, "summary", s.Summary)
	if len(s.CompletedActions) > 0 {
		args["completed_actions"] = s.CompletedActions
	}
	if len(s.Links) > 0 {
		args["links"] = s.Links
	}
	s.Usage.put(args)

	res, err := c.call(ctx, ToolSubmitDeliverable, args)
	if err != nil {
		return nil, err
	}
	if nr := asNotRunning(s.RunID, res, "Not saved: "); nr != nil {
		return nil, nr
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	out := &SubmitResult{
		Status:        s.Status,
		Text:          res.Text,
		ReviewPending: strings.Contains(res.Text, reviewPendingMarker),
	}
	if m := submittedRE.FindStringSubmatch(res.Text); m != nil {
		out.Status = SubmitStatus(m[1])
	}
	return out, nil
}

// ----------------------------------------------------- heartbeat_executor

// HeartbeatRequest says this machine is switched on.
type HeartbeatRequest struct {
	Workspace string
	Queue     string
	// The identity fields are optional re-declarations, for when the
	// environment changes. DeclaredCapabilities REPLACES the stored list; it
	// is not merged.
	HostID               string
	AgentKind            string
	DeclaredCapabilities []string

	// Telemetry is the optional fleet reading: what the machine looks like
	// right now, and what the session on this queue is doing. Either half may
	// be nil and an empty one is not sent — see telemetry.go.
	//
	// Sending it never costs the beat. Elk stores what it can and counts the
	// machine alive either way, and a reading it could not store comes back as
	// a note in [Heartbeat.Text] rather than as an error.
	Telemetry Telemetry
}

// Heartbeat is what Elk answered.
type Heartbeat struct {
	// Queue is the queue Elk recorded the beat against.
	Queue string
	// Waiting is how many runs are queued for it. The heartbeat reply carries
	// the queue depth, which makes it a complete poll on its own.
	Waiting int
	Text    string
}

var (
	heartbeatQueueRE = regexp.MustCompile(`^Heartbeat recorded for queue "([^"]+)"`)
	heartbeatWaitRE  = regexp.MustCompile(`(\d+) runs? waiting`)
)

// HeartbeatExecutor tells Elk this machine is up, whether or not it holds a
// run. It is a different clock from [Client.ReportProgress]: that renews one
// RUN's 15-minute lease, this says the MACHINE is up, which an idle agent with
// an empty queue has no other way to say. Between beats Elk shows the queue as
// offline in `list_executors` — which is what people and the router dispatch
// from, though nothing on the claim path itself refuses an offline queue.
//
// It returns a [*UnknownToolError] against an Elk that has not shipped the
// tool. Callers must tolerate that and carry on: heartbeating is how Rein
// becomes visible, not how it works.
func (c *Client) HeartbeatExecutor(ctx context.Context, req HeartbeatRequest) (*Heartbeat, error) {
	args := map[string]any{}
	putIf(args, "workspace", req.Workspace)
	putIf(args, "queue", req.Queue)
	putIf(args, "host_id", req.HostID)
	putIf(args, "agent_kind", req.AgentKind)
	if len(req.DeclaredCapabilities) > 0 {
		args["declared_capabilities"] = req.DeclaredCapabilities
	}
	// Trim before sending rather than letting the server refuse an over-cap
	// reading. What was dropped, if anything, is not an error: the beat is the
	// point and the reading is the passenger.
	tel := req.Telemetry
	if _, err := tel.Fit(); err == nil {
		tel.put(args)
	}

	res, err := c.call(ctx, ToolHeartbeatExecutor, args)
	if err != nil {
		return nil, err
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	hb := &Heartbeat{Queue: req.Queue, Text: res.Text}
	if m := heartbeatQueueRE.FindStringSubmatch(res.Text); m != nil {
		hb.Queue = m[1]
	}
	if m := heartbeatWaitRE.FindStringSubmatch(res.Text); m != nil {
		hb.Waiting, _ = strconv.Atoi(m[1])
	}
	return hb, nil
}

// ListExecutors returns Elk's own view of the queues this token can see —
// kind, host, online, queue depth — as the text Elk formats it. `rein status`
// prints it verbatim rather than re-rendering it, so the machine's answer and
// Elk's answer cannot drift apart in the one place a human goes to compare
// them.
func (c *Client) ListExecutors(ctx context.Context, workspace string) (string, error) {
	args := map[string]any{}
	putIf(args, "workspace", workspace)
	res, err := c.call(ctx, ToolListExecutors, args)
	if err != nil {
		return "", err
	}
	return res.Text, res.Err()
}

// notLiveActionMarker is how `list_actions` reports an action that is no
// longer on the List — done, or archived (elk-mcp/mcp.ts:871).
const notLiveActionMarker = "No live List action with id"

// ActionState is what a lookup of one List action found.
type ActionState struct {
	// Live reports whether the action is still open on the List. An action
	// that has gone `done` or been archived is not.
	Live bool
	// Text is Elk's reply.
	Text string
}

// ActionDetail looks up one List action.
//
// Its use in Rein is narrow and worth stating, because the reply is ambiguous
// in general and unambiguous here: `list_actions` cannot tell "this action is
// resolved" from "this action never existed" — both answer "No live List
// action with id …". But Rein only asks about an action it CLAIMED A RUN ON,
// so the second possibility is already excluded. Not live therefore means
// resolved, and that is the difference between a user cancelling a run and a
// run's own merge closing the item out from under it.
func (c *Client) ActionDetail(ctx context.Context, workspace, actionID string) (*ActionState, error) {
	if actionID == "" {
		return nil, errors.New("elk: list_actions needs an action id")
	}
	args := map[string]any{"action_id": actionID}
	putIf(args, "workspace", workspace)

	res, err := c.call(ctx, ToolListActions, args)
	if err != nil {
		return nil, err
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return &ActionState{
		Live: !strings.Contains(res.Text, notLiveActionMarker),
		Text: res.Text,
	}, nil
}

// ListWorkspaces is the cheapest read that proves a token still works. `rein
// status` and the tail of `rein enrol` use it as a liveness probe, because
// every alternative either changes something or claims something.
func (c *Client) ListWorkspaces(ctx context.Context) (string, error) {
	res, err := c.call(ctx, ToolListWorkspaces, nil)
	if err != nil {
		return "", err
	}
	return res.Text, res.Err()
}

func putIf(args map[string]any, key, value string) {
	if v := strings.TrimSpace(value); v != "" {
		args[key] = v
	}
}
