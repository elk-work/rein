package adapter

import (
	"encoding/json"
	"fmt"
	"time"
)

// EventKind tags an [Event]. The set is closed; an adapter that sees something
// its vendor emits and Rein has no kind for reports it as [EventProgress] with
// the vendor payload in [Event.Raw] rather than inventing a kind.
type EventKind string

const (
	// EventText — assistant text. What the agent said.
	EventText EventKind = "text"

	// EventToolUse — the agent called a tool. [Event.Tool] is set.
	EventToolUse EventKind = "tool_use"

	// EventPermissionRequest — the agent wants authorisation for something.
	// [Event.Permission] is set and the run loop must answer with
	// [Session.Respond], or the session waits. Only ever emitted by an
	// adapter declaring [CapApprovals].
	EventPermissionRequest EventKind = "permission_request"

	// EventQuestion — the agent needs a human. Maps to Elk's `ask_elk`;
	// answer with [Session.Send]. Distinct from a permission request: this is
	// a question about the work, not about authority.
	EventQuestion EventKind = "question"

	// EventIdle — the agent has stopped doing anything and is waiting. The
	// watchdog's input. An adapter derives it from a real signal — Claude
	// Code's `idle_prompt` / `agent_needs_input` notification hooks, an ACP
	// update — never from scraping a spinner out of scrollback.
	EventIdle EventKind = "idle"

	// EventProgress — anything else worth relaying: a step, a plan, a status
	// line, a vendor event with no Rein kind. Maps to Elk's
	// `report_progress`.
	EventProgress EventKind = "progress"

	// EventUsage — tokens spent. [Event.Usage] is set. Elk prices this
	// server-side from model-rates.json, so an adapter reports counts and
	// never money.
	EventUsage EventKind = "usage"

	// EventDone — the session finished. [Event.Result] is set. The last event
	// before the channel closes.
	EventDone EventKind = "done"

	// EventError — the session failed. [Event.Err] is set. Also a last event.
	EventError EventKind = "error"
)

// Terminal reports whether an event kind ends a session.
func (k EventKind) Terminal() bool { return k == EventDone || k == EventError }

// Event is one thing that happened in a session. It is a tagged union: read
// [Event.Kind] first, then the field that kind populates.
//
// One flat struct rather than an interface because these travel down a channel
// and every consumer switches on the kind anyway; an interface would buy
// nothing but type assertions.
type Event struct {
	// Kind says which of the fields below is meaningful.
	Kind EventKind `json:"kind"`

	// At is when the adapter observed it, not when the vendor stamped it.
	At time.Time `json:"at"`

	// Seq counts events within a session from 1, so a consumer can tell a
	// dropped event from a quiet agent.
	Seq uint64 `json:"seq"`

	// Text carries [EventText], [EventQuestion] and [EventProgress] bodies,
	// and a human-readable summary for the others.
	Text string `json:"text,omitempty"`

	// Tool is set on [EventToolUse].
	Tool *ToolUse `json:"tool,omitempty"`

	// Permission is set on [EventPermissionRequest].
	Permission *PermissionRequest `json:"permission,omitempty"`

	// Usage is set on [EventUsage] — an increment, not a running total.
	Usage *Usage `json:"usage,omitempty"`

	// Result is set on [EventDone].
	Result *Result `json:"result,omitempty"`

	// Err is set on [EventError].
	Err error `json:"-"`

	// Raw is the vendor's own event, verbatim, for debugging and for the
	// transcript. Never interpreted by the run loop.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// String renders an event for a log line.
func (e Event) String() string {
	switch e.Kind {
	case EventToolUse:
		if e.Tool != nil {
			return fmt.Sprintf("#%d tool_use %s", e.Seq, e.Tool.Name)
		}
	case EventUsage:
		if e.Usage != nil {
			return fmt.Sprintf("#%d usage in=%d out=%d %s",
				e.Seq, e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.Model)
		}
	case EventError:
		return fmt.Sprintf("#%d error %v", e.Seq, e.Err)
	case EventDone:
		if e.Result != nil {
			return fmt.Sprintf("#%d done %s", e.Seq, e.Result.Status)
		}
	}
	if e.Text != "" {
		return fmt.Sprintf("#%d %s %s", e.Seq, e.Kind, truncate(e.Text, 120))
	}
	return fmt.Sprintf("#%d %s", e.Seq, e.Kind)
}

// ToolUse is one tool call.
type ToolUse struct {
	ID    string          `json:"id"`              // the vendor's call id, for correlating a result
	Name  string          `json:"name"`            // the vendor's tool name
	Input json.RawMessage `json:"input,omitempty"` // arguments, verbatim
}

// PermissionRequest is the agent asking for authority.
type PermissionRequest struct {
	// ID is what [PermissionResponse.ID] must echo back.
	ID string `json:"id"`

	// Tool is the vendor tool name being authorised.
	Tool string `json:"tool"`

	// Summary is a one-line description for a human — the command, the path.
	Summary string `json:"summary,omitempty"`

	// Input is the call being authorised, verbatim.
	Input json.RawMessage `json:"input,omitempty"`
}

// PermissionResponse answers a [PermissionRequest] through [Session.Respond].
type PermissionResponse struct {
	// ID echoes [PermissionRequest.ID].
	ID string

	// Allow authorises the call. False denies it.
	Allow bool

	// Reason is shown to the agent, and matters most on a denial: it is the
	// only way the agent learns why, and an agent that does not know why
	// retries the same thing.
	Reason string
}

// Usage is a token increment.
type Usage struct {
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitempty"`
	Model            string `json:"model,omitempty"`
}

// Add accumulates u2 into u.
func (u *Usage) Add(u2 Usage) {
	u.InputTokens += u2.InputTokens
	u.OutputTokens += u2.OutputTokens
	u.CacheReadTokens += u2.CacheReadTokens
	u.CacheWriteTokens += u2.CacheWriteTokens
	if u2.Model != "" {
		u.Model = u2.Model
	}
}

// ResultStatus is how a session ended.
type ResultStatus string

const (
	// StatusSucceeded — the agent finished the work it was given.
	StatusSucceeded ResultStatus = "succeeded"

	// StatusFailed — the agent ran and did not succeed.
	StatusFailed ResultStatus = "failed"

	// StatusInterrupted — stopped by [Session.Interrupt], or by Elk
	// cancelling the run.
	StatusInterrupted ResultStatus = "interrupted"

	// StatusTimedOut — a [Timeouts] bound was hit.
	StatusTimedOut ResultStatus = "timed_out"
)

// Result is how a session ended, and what it produced.
type Result struct {
	// Status is the outcome.
	Status ResultStatus `json:"status"`

	// Summary is the agent's own account of what it did — the text that
	// becomes Elk's deliverable.
	Summary string `json:"summary,omitempty"`

	// SessionID is the vendor session id, for resume and for a `links[]`
	// entry pointing at the transcript.
	SessionID string `json:"session_id,omitempty"`

	// Usage is the session total.
	Usage Usage `json:"usage"`

	// ExitCode is the process exit code where there was a process.
	ExitCode int `json:"exit_code"`

	// Duration is wall-clock time from Start to the terminal event.
	Duration time.Duration `json:"duration"`

	// TranscriptPath is a local file holding the session's raw events, where
	// the adapter kept one. The run loop decides whether to upload it.
	TranscriptPath string `json:"transcript_path,omitempty"`
}

// OK reports whether the session succeeded.
func (r Result) OK() bool { return r.Status == StatusSucceeded }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
