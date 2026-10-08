// Package runlog is the per-run event log Rein writes while it drives an agent,
// and the reader `rein tail` watches it through.
//
// Rein's whole design is that it watches structured events rather than
// scrollback — but until this package existed those events went down the
// adapter's channel, were sampled for `report_progress`, and were then
// dropped. The daemon's own log said "claimed run X" and "submitted ready"
// with forty minutes of silence in between, so there was no way to see what an
// agent was actually doing except to wait for the deliverable.
//
// The log is two files per run, under the Rein home's `log/runs`:
//
//	<run id>.jsonl        every event, one JSON object per line
//	<run id>.meta.json    the small facts: queue, direction, status, worktree
//
// Per-run meta rather than one shared index.json on purpose. Several queue
// goroutines write these at once, and more than one `rein run` may share a
// Rein home; a single index would need a lock and a rewrite per update, while
// a file per run needs neither and is repaired by deleting it.
//
// The log outlives the worktree deliberately. Under the default the worktree
// is removed the moment the deliverable saves, and from then on the log is the
// only account of what happened inside it.
//
// Nothing here may fail a run. A [Writer] that cannot write records the first
// error, keeps quiet afterwards and goes on returning successfully, so the
// call sites in the run loop are unconditional and a full disk costs a log
// rather than a session. A nil *Writer is a working no-op for the same reason:
// a runner with logging switched off calls exactly the same code.
package runlog

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// Source says which half of the system produced a record: the agent's own
// event stream, or Rein's loop around it. A reader needs the distinction
// because they are read differently — the agent's text is the content, and the
// runner's records are the frame around it.
type Source string

const (
	// SourceAgent — a record made from an [adapter.Event].
	SourceAgent Source = "agent"

	// SourceRunner — a record Rein's own loop wrote about what it was doing.
	SourceRunner Source = "runner"
)

// Kind is a [SourceRunner] record's kind. Agent records carry the
// [adapter.EventKind] verbatim instead, so a reader that knows the adapter
// contract already knows most of this vocabulary.
type Kind string

const (
	// KindClaimed — the run was claimed from its Elk queue. Always first.
	KindClaimed Kind = "claimed"

	// KindWorktree — the isolated git worktree was created.
	KindWorktree Kind = "worktree"

	// KindSession — an agent session was started, or restarted after a stall.
	KindSession Kind = "session"

	// KindReport — a `report_progress` was sent to Elk.
	KindReport Kind = "report"

	// KindReview — Rein is waiting on Elk's auto-review pass, or has heard
	// back from it.
	KindReview Kind = "review"

	// KindSubmit — a deliverable was submitted.
	KindSubmit Kind = "submit"

	// KindReaped — the worktree was removed, or deliberately kept.
	KindReaped Kind = "reaped"

	// KindAttach — a person attached to the live session, sent it something,
	// or detached. See `rein attach`.
	KindAttach Kind = "attach"

	// KindNote — anything else worth recording that has no kind of its own.
	KindNote Kind = "note"
)

// Status is where a run got to. It lives in the [Meta] so a reader can tell an
// active run from a finished one without parsing the log.
type Status string

const (
	// StatusRunning — Rein is driving it now. The only status a tail's
	// live view shows by default.
	StatusRunning Status = "running"

	// StatusSubmitted — a deliverable was submitted `ready`.
	StatusSubmitted Status = "submitted"

	// StatusStuck — submitted `stuck`: Rein could not finish it.
	StatusStuck Status = "stuck"

	// StatusCancelled — a person cancelled it while it ran; nothing submitted.
	StatusCancelled Status = "cancelled"

	// StatusInterrupted — the daemon stopped while it was in flight, so the
	// claim was left to lapse.
	StatusInterrupted Status = "interrupted"
)

// Active reports whether a run in this status is still being driven.
func (s Status) Active() bool { return s == StatusRunning || s == "" }

// Record is one line of the log: one thing that happened, in the order it
// happened. It is a superset of [adapter.Event] — the same tagged-union shape,
// plus the fields a runner record needs — because a reader switching on Kind
// should not have to care which half of the system spoke.
type Record struct {
	// RunID is Elk's action_run id, repeated on every line so a concatenated
	// or interleaved log still parses.
	RunID string `json:"run_id"`

	// Seq counts records within this log from 1. Gaps mean lost records, not
	// a quiet agent — [adapter.Event.Seq] counts a session and a run may hold
	// more than one.
	Seq uint64 `json:"seq"`

	// At is when Rein observed it.
	At time.Time `json:"at"`

	// Source says who spoke.
	Source Source `json:"source"`

	// Kind is an [adapter.EventKind] for [SourceAgent] and a [Kind] for
	// [SourceRunner].
	Kind string `json:"kind"`

	// Text is the body: assistant text, a question, a runner's own sentence.
	Text string `json:"text,omitempty"`

	// Tool is set on an agent `tool_use` record.
	Tool *adapter.ToolUse `json:"tool,omitempty"`

	// Permission is set on an agent `permission_request` record.
	Permission *adapter.PermissionRequest `json:"permission,omitempty"`

	// Usage is an increment, never a running total — the same contract
	// [adapter.Event.Usage] has.
	Usage *adapter.Usage `json:"usage,omitempty"`

	// Result is set on an agent `done` record.
	Result *adapter.Result `json:"result,omitempty"`

	// Error carries an agent `error` record's error as text, because an error
	// value does not survive JSON.
	Error string `json:"error,omitempty"`

	// Raw is the vendor's own event, verbatim, exactly as the adapter
	// contract passes it through. It is what `rein tail --raw` is for.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// AgentRecord converts an [adapter.Event] into a record. Seq and At are left
// for the writer to stamp: the event's own Seq counts its session, and a run
// may hold two of those after a stall restart.
func AgentRecord(ev adapter.Event) Record {
	r := Record{
		Source:     SourceAgent,
		Kind:       string(ev.Kind),
		Text:       ev.Text,
		Tool:       ev.Tool,
		Permission: ev.Permission,
		Usage:      ev.Usage,
		Result:     ev.Result,
		Raw:        ev.Raw,
		At:         ev.At,
	}
	if ev.Err != nil {
		r.Error = ev.Err.Error()
	}
	return r
}

// RunnerRecord builds a [SourceRunner] record.
func RunnerRecord(kind Kind, format string, args ...any) Record {
	text := format
	if len(args) > 0 {
		text = fmt.Sprintf(format, args...)
	}
	return Record{Source: SourceRunner, Kind: string(kind), Text: text}
}

// Terminal reports whether this record ends the agent's session.
func (r Record) Terminal() bool {
	return r.Source == SourceAgent && adapter.EventKind(r.Kind).Terminal()
}

// Meta is the small, mutable set of facts about a run that a reader needs
// before it opens the log at all: which queue, what was asked for, whether it
// is still going, and where the work is happening.
type Meta struct {
	RunID     string `json:"run_id"`
	Queue     string `json:"queue,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	AgentKind string `json:"agent_kind,omitempty"`

	// Direction is the first line of the Elk item's direction — what a person
	// asked for, in their words. The whole packet is not here; it can be
	// thousands of words and this file is read on every refresh.
	Direction string `json:"direction,omitempty"`

	// Status is where the run got to. See [Status].
	Status Status `json:"status"`

	// PID is the `rein run` process driving it, so a reader can tell a live
	// run from one whose runner died without updating this file.
	PID int `json:"pid,omitempty"`

	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`

	// Repo, Worktree and Branch are filled in once the worktree exists. The
	// branch is the durable one: the directory is removed at the end of the
	// run and the branch is not.
	Repo     string `json:"repo,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`

	// SessionID is the vendor's own session id, which is what `rein attach`
	// and a `--resume` both need.
	SessionID string `json:"session_id,omitempty"`

	// PermissionMode is the mode the session was started in.
	PermissionMode string `json:"permission_mode,omitempty"`

	// Attached is true while a person is driving the session through
	// `rein attach`, so the live view can say the daemon has stepped back.
	Attached bool `json:"attached,omitempty"`

	// Version is the Rein that wrote the log.
	Version string `json:"version,omitempty"`
}

// Elapsed is how long the run has been going, or how long it took.
func (m Meta) Elapsed(now time.Time) time.Duration {
	if m.StartedAt.IsZero() {
		return 0
	}
	end := m.EndedAt
	if end.IsZero() {
		end = now
	}
	if end.Before(m.StartedAt) {
		return 0
	}
	return end.Sub(m.StartedAt)
}

// Short abbreviates a run id for a column. Elk's run ids are UUIDs, so the
// first eight characters are plenty to tell two apart on one machine.
func Short(runID string) string {
	if len(runID) > 8 {
		return runID[:8]
	}
	return runID
}
