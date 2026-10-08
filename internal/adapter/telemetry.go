package adapter

import (
	"context"
	"time"
)

// Session telemetry: what a running agent can say about ITSELF, as opposed to
// what it is doing.
//
// This is an OPTIONAL extension to the adapter contract, not part of
// [Session], and the split is deliberate on two counts.
//
// First, the event set in event.go is closed. A vendor fact with no Rein kind
// becomes [EventProgress] carrying the payload; it does not get a kind
// invented for it. Context fill and a subscription window are facts about the
// session's state rather than things that happened in it, so they are not
// events at all — an event stream would make a consumer replay the whole run
// to learn the current value of one number.
//
// Second, an interface every adapter must implement is an interface every
// adapter must implement *badly* where its vendor says nothing. Codex reports
// its subscription window and little else; Grok reports only a turn that
// failed on a rate limit. An optional interface lets an adapter answer only
// what its vendor actually tells it, and lets the run loop ask without caring
// which adapter it holds.
//
// Every field is a pointer or a zero-value-means-absent string for the same
// reason the wire format is: **the absence of a measurement and a measurement
// of zero are different claims.** A session at 0% context fill and a session
// whose adapter never learned the number must not render the same way.

// Telemeter is implemented by a [Session] that can describe its own state. The
// run loop type-asserts for it and asks nothing of a session without it.
//
// Telemetry may be called from any goroutine, at any time, including before
// the first event and after the session has ended. An implementation returns
// the best it knows and never blocks.
type Telemeter interface {
	Telemetry() SessionTelemetry
}

// SessionTelemetry is one instant's account of a session's own state.
type SessionTelemetry struct {
	// Model is the vendor model id actually in use, which is not always the
	// one that was asked for.
	Model string

	// ContextUsedTokens is how much of the context window the most recent
	// request carried; ContextWindowTokens is how much it could have. Nil
	// means the vendor has not said — never zero, which would render as an
	// empty context.
	ContextUsedTokens   *int64
	ContextWindowTokens *int64

	// RateLimit is the subscription state the vendor volunteers, if any. It
	// matters because a fleet that starves the developer at the keyboard is
	// the named risk in elk's docs/rein.md §3.
	RateLimit *RateLimit

	// MCPServers is the session's MCP inventory. A POINTER TO A SLICE, so that
	// an empty inventory and an unknown one stay different answers: nil is
	// "the vendor has not told us", and a pointer to an empty slice is "it
	// has, and there are none".
	//
	// The distinction is load-bearing here and not theoretical. Rein starts
	// every Claude run with --strict-mcp-config and an explicit empty server
	// set, so the true answer for a Rein-driven run IS the empty list — and
	// reporting that honestly is the point, because the alternative is what
	// ark:rein#21 fixed: a run inheriting the developer's own servers and
	// acting under their identity.
	MCPServers *[]MCPServer
}

// RateLimit is a vendor's account of the subscription behind a session.
//
// The first half is the vendor's own vocabulary, relayed. The second half —
// [RateLimit.Exhausted] and [RateLimit.ExhaustedUntil] — is the ADAPTER's
// reading of it, in one vocabulary for every vendor, because the run loop has
// to decide whether to claim the next run and must not learn that Claude says
// "rejected" where Codex says `rateLimitReachedType` and Grok says nothing at
// all until a turn has already failed (ark:rein#40).
type RateLimit struct {
	// Status is the vendor's own word — "allowed", "rejected".
	Status string

	// Type is which window is binding: "five_hour", "seven_day".
	Type string

	// ResetsAt is when the binding window resets. Zero means the vendor did
	// not say.
	ResetsAt time.Time

	// UsingOverage is nil when the vendor said nothing about overage, and a
	// pointer to false when it explicitly said no — which is a different and
	// more useful claim than silence.
	UsingOverage    *bool
	OverageStatus   string
	OverageResetsAt time.Time

	// Windows is every window the vendor reported, keyed by its own name. A
	// map rather than named fields so a window the vendor adds tomorrow
	// arrives rather than being silently dropped by a curated struct.
	Windows map[string]RateLimitWindow

	// Source names where this reading came from, so a reader can weigh it:
	// [SourceClaudeEvent], [SourceCodexRateLimits], [SourceGrokStopFailure].
	Source string

	// SampledAt is when the adapter observed it. A reading is carried forward
	// between runs, and one from four hours ago is a different claim from one
	// taken a minute ago; this is what lets a reader tell them apart.
	SampledAt time.Time

	// Exhausted is the adapter's verdict: the vendor has said this account
	// cannot serve another request right now, so a run started now would fail.
	// It is set only on the vendor's own word — never from a percentage the
	// adapter judged to be close enough.
	Exhausted bool

	// ExhaustedUntil is when the adapter expects that to end. Zero means the
	// vendor did not say, and the run loop then applies its own fallback
	// (a stated weekly reset, else 24 hours). Meaningless unless Exhausted.
	ExhaustedUntil time.Time
}

// Sources of a [RateLimit], in the vocabulary the fleet reading reports them.
const (
	// SourceClaudeEvent — Claude Code's `rate_limit_event`, which arrives on
	// the stream of a live run and only there.
	SourceClaudeEvent = "claude_rate_limit_event"

	// SourceCodexRateLimits — Codex's `account/rateLimits/read` and
	// `account/rateLimits/updated`, which can be read with no run at all.
	SourceCodexRateLimits = "codex_account_rate_limits"

	// SourceGrokStopFailure — a Grok turn that ended in the `StopFailure` hook
	// with `error: rate_limit`. Grok exposes no window, so this is the only
	// thing there is, and it arrives after a turn has already failed.
	SourceGrokStopFailure = "grok_stop_failure"
)

// RateLimitWindow is one subscription window's fill.
type RateLimitWindow struct {
	// Utilization is 0..1. Not a pointer: a window that appears in the map was
	// reported, and a reported 0 is a window that has just reset.
	Utilization float64

	// ResetsAt is zero when the vendor did not say.
	ResetsAt time.Time

	// Minutes is the window's length when the vendor states it — Codex does,
	// as `windowDurationMins`; Claude names its windows instead. Zero means not
	// stated.
	Minutes int64
}

// HeadroomReader is implemented by an [Adapter] that can read its
// subscription window WITHOUT spending a model turn. The run loop calls it
// while a queue is idle, so that the headroom it reports is a reading and not
// a memory of the last run.
//
// It is optional, and only one adapter has it. Codex answers
// `account/rateLimits/read` over its app server for the price of a process
// start. Claude Code publishes its window only on the stream of a request it
// is already making — `/usage` is a separate endpoint that is itself
// rate-limited, and polling it is exactly what the design rules out — so the
// Claude adapter carries forward what its last run saw and does not pretend
// otherwise. Grok exposes no window at all.
//
// ReadHeadroom talks to the vendor binary, never to the vendor's API with a
// credential Rein holds: the same rule as [Adapter.Preflight].
type HeadroomReader interface {
	ReadHeadroom(ctx context.Context) (*RateLimit, error)
}

// MCPServer is one MCP server a session has, as the vendor named it.
type MCPServer struct {
	Name   string
	Status string
}
