// Package fake is a scriptable [adapter.Adapter] for tests and for the run
// loop lane, which needs something to drive before any real agent adapter
// exists.
//
// It spawns no process and shells out to nothing: a session replays a scripted
// list of events on its own goroutine and then reports the result it was
// given. What it does faithfully is the *contract* — the channel closes after
// exactly one terminal event, Wait is idempotent, Send and Respond fail when
// the manifest does not declare the capability behind them, and Start refuses
// a spec that CheckSupport refuses. A run loop that passes against this
// adapter is exercising the same rules a real one enforces.
//
// Importing it registers it under kind "fake". Nothing in the daemon imports
// it, so it is present only in test binaries and in whatever explicitly asks
// for it.
package fake

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// Kind is the agent kind this adapter registers under.
const Kind = "fake"

func init() { adapter.MustRegister(New()) }

// Adapter is a scriptable adapter. The zero value is not usable; call [New]
// and adjust the exported fields before [Adapter.Start].
//
// It is safe for concurrent use, but the scripting fields should be set before
// the first Start rather than raced against one.
type Adapter struct {
	// Kind overrides the registry key. Change it to register a second fake
	// alongside the default one.
	Kind string

	// Capabilities is the declared manifest. Trim it to test that a run loop
	// actually fails closed — a fake with CapApprovals set to
	// [adapter.SupportPartial] must make [adapter.PermissionAsk] unusable.
	Capabilities adapter.Manifest

	// PreflightErr, when set, is what [Adapter.Preflight] returns.
	PreflightErr error

	// StartErr, when set, is what [Adapter.Start] returns — after the spec
	// checks, so a test can distinguish a refusal from a failure.
	StartErr error

	// Script is the event sequence a session replays. Seq and At are stamped
	// by the session; anything else is used verbatim. If it contains no
	// terminal event, one is appended from [Adapter.Result].
	Script []adapter.Event

	// Result is what [Session.Wait] reports when the script does not carry a
	// terminal event of its own.
	Result adapter.Result

	// WaitErr, when set, is the error [Session.Wait] returns alongside Result.
	WaitErr error

	// Delay is slept between scripted events. Zero is the useful default:
	// tests should not pay for realism they are not asserting on.
	Delay time.Duration

	// RateLimit is what every session reports as its subscription reading
	// through [adapter.Telemeter]. Nil reports none, which is what a session
	// whose vendor said nothing looks like. Each session gets its own copy,
	// with a zero SampledAt stamped as the session's start — so two sessions
	// report two readings, the way two real runs do.
	RateLimit *adapter.RateLimit

	// RefuseMCP names MCP servers [Adapter.MCPServerConfig] refuses as
	// unsupported.
	RefuseMCP []string

	mu       sync.Mutex
	specs    []adapter.RunSpec
	sessions []*Session
}

// MCPServerConfig implements [adapter.MCPConfigurer]: a server comes back as
// a map of its own neutral fields, so a test can see exactly what reached the
// RunSpec. A spec named in [Adapter.RefuseMCP] is refused the way Grok refuses
// a stdio server.
func (a *Adapter) MCPServerConfig(s adapter.MCPServerSpec) (any, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	for _, name := range a.RefuseMCP {
		if name == s.Name {
			return nil, fmt.Errorf("%w: fake refuses MCP server %q", adapter.ErrNotSupported, s.Name)
		}
	}
	m := map[string]any{"fake": true}
	if s.URL != "" {
		m["url"] = s.URL
	}
	if s.Command != "" {
		m["command"] = s.Command
	}
	return m, nil
}

// Reader is a fake that can also read its subscription window without a
// turn — the [adapter.HeadroomReader] half, which only some adapters have.
// Register it under its own kind; the plain [Adapter] deliberately does not
// implement the interface, so a test opts in to it.
type Reader struct {
	*Adapter

	// Read answers [Reader.ReadHeadroom].
	Read func(ctx context.Context) (*adapter.RateLimit, error)

	mu    sync.Mutex
	reads int
}

// NewReader returns a Reader over a fresh fake.
func NewReader(read func(ctx context.Context) (*adapter.RateLimit, error)) *Reader {
	return &Reader{Adapter: New(), Read: read}
}

// ReadHeadroom implements [adapter.HeadroomReader].
func (r *Reader) ReadHeadroom(ctx context.Context) (*adapter.RateLimit, error) {
	r.mu.Lock()
	r.reads++
	r.mu.Unlock()
	if r.Read == nil {
		return nil, errors.New("fake: no Read scripted")
	}
	return r.Read(ctx)
}

// Reads counts the ReadHeadroom calls.
func (r *Reader) Reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// New returns a fake adapter that declares every capability, runs on every
// platform Rein targets, and replays a short successful session.
func New() *Adapter {
	return &Adapter{
		Kind: Kind,
		Capabilities: adapter.Manifest{
			Kind:   Kind,
			Binary: "(none — this adapter spawns no process)",
			Capabilities: map[adapter.Capability]adapter.Support{
				adapter.CapGit:              adapter.SupportYes,
				adapter.CapWorktree:         adapter.SupportYes,
				adapter.CapFileEdit:         adapter.SupportYes,
				adapter.CapShell:            adapter.SupportYes,
				adapter.CapMCP:              adapter.SupportYes,
				adapter.CapResume:           adapter.SupportYes,
				adapter.CapStructuredEvents: adapter.SupportYes,
				adapter.CapApprovals:        adapter.SupportYes,
				adapter.CapSteer:            adapter.SupportYes,
			},
			Platforms: []adapter.Platform{
				adapter.AnyArch("darwin"), adapter.AnyArch("linux"), adapter.AnyArch("windows"),
			},
			Notes: "Test double. Declares everything so a test opts out of a capability " +
				"deliberately rather than by omission.",
		},
		Script: []adapter.Event{
			{Kind: adapter.EventProgress, Text: "starting"},
			{Kind: adapter.EventText, Text: "working on it"},
			{Kind: adapter.EventUsage, Usage: &adapter.Usage{
				InputTokens: 1000, OutputTokens: 250, Model: "fake-1"}},
		},
		Result: adapter.Result{
			Status:    adapter.StatusSucceeded,
			Summary:   "fake session completed",
			SessionID: "fake-session",
			Usage:     adapter.Usage{InputTokens: 1000, OutputTokens: 250, Model: "fake-1"},
		},
	}
}

// Name implements [adapter.Adapter].
func (a *Adapter) Name() string { return a.Kind }

// Manifest implements [adapter.Adapter]. The declared kind always matches
// [Adapter.Kind], so changing one field is enough to register a second fake.
func (a *Adapter) Manifest() adapter.Manifest {
	m := a.Capabilities
	m.Kind = a.Kind
	return m
}

// Preflight implements [adapter.Adapter].
func (a *Adapter) Preflight(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.PreflightErr
}

// Start implements [adapter.Adapter]. It validates the spec and runs the same
// fail-closed check a real adapter must, so a test that skips the run loop
// still cannot start a session the manifest forbids.
func (a *Adapter) Start(ctx context.Context, spec adapter.RunSpec) (adapter.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := adapter.CheckSupport(a.Manifest(), spec); err != nil {
		return nil, err
	}
	if a.StartErr != nil {
		return nil, a.StartErr
	}

	s := &Session{
		spec:      spec,
		adapter:   a,
		sessionID: a.Result.SessionID,
		events:    make(chan adapter.Event),
		done:      make(chan struct{}),
		started:   time.Now(),
	}
	if s.sessionID == "" {
		s.sessionID = "fake-" + spec.RunID
	}
	if a.RateLimit != nil {
		rl := *a.RateLimit
		if rl.SampledAt.IsZero() {
			rl.SampledAt = s.started
		}
		s.rateLimit = &rl
	}
	s.ctx, s.cancel = context.WithCancel(context.WithoutCancel(ctx))

	a.mu.Lock()
	a.specs = append(a.specs, spec)
	a.sessions = append(a.sessions, s)
	a.mu.Unlock()

	go s.replay(a.Script, a.Result, a.WaitErr, a.Delay)
	return s, nil
}

// Specs returns every [adapter.RunSpec] this adapter was started with, in
// order — what a test asserts the run loop actually asked for.
func (a *Adapter) Specs() []adapter.RunSpec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]adapter.RunSpec(nil), a.specs...)
}

// Sessions returns every session this adapter started, in order.
func (a *Adapter) Sessions() []*Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*Session(nil), a.sessions...)
}

// Session is one fake session.
type Session struct {
	spec      adapter.RunSpec
	adapter   *Adapter
	sessionID string
	events    chan adapter.Event
	done      chan struct{}
	started   time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	rateLimit *adapter.RateLimit

	mu          sync.Mutex
	sent        []string
	responses   []adapter.PermissionResponse
	interrupts  int
	result      adapter.Result
	err         error
	interrupted bool
}

// Spec returns the spec this session was started with.
func (s *Session) Spec() adapter.RunSpec { return s.spec }

// ID implements [adapter.Session].
func (s *Session) ID() string { return s.sessionID }

// Events implements [adapter.Session].
func (s *Session) Events() <-chan adapter.Event { return s.events }

// Send implements [adapter.Session]. It records the input, and refuses once
// the session has ended — a run loop that answers a question after the agent
// exited has a bug the fake should surface, not absorb.
func (s *Session) Send(ctx context.Context, input string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.done:
		return adapter.ErrSessionClosed
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, input)
	return nil
}

// Respond implements [adapter.Session]. It returns [adapter.ErrNotSupported]
// when the manifest does not declare [adapter.CapApprovals] as
// [adapter.SupportYes] — the same refusal a real adapter owes.
func (s *Session) Respond(ctx context.Context, resp adapter.PermissionResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.adapter.Manifest().Has(adapter.CapApprovals) {
		return fmt.Errorf("%w: %s does not declare approvals", adapter.ErrNotSupported, s.adapter.Kind)
	}
	select {
	case <-s.done:
		return adapter.ErrSessionClosed
	default:
	}
	if resp.ID == "" {
		return errors.New("fake: PermissionResponse has no ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, resp)
	return nil
}

// Interrupt implements [adapter.Session]. The session stops replaying and
// finishes as [adapter.StatusInterrupted].
func (s *Session) Interrupt(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.interrupts++
	s.interrupted = true
	s.mu.Unlock()
	s.cancel()
	return nil
}

// Wait implements [adapter.Session]. It is idempotent.
func (s *Session) Wait() (adapter.Result, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.err
}

// Telemetry implements [adapter.Telemeter]. It reports this session's copy of
// [Adapter.RateLimit] and nothing else.
func (s *Session) Telemetry() adapter.SessionTelemetry {
	return adapter.SessionTelemetry{RateLimit: s.rateLimit}
}

// Sent returns the follow-up inputs delivered through [Session.Send].
func (s *Session) Sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// Responses returns the permission answers delivered through
// [Session.Respond].
func (s *Session) Responses() []adapter.PermissionResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]adapter.PermissionResponse(nil), s.responses...)
}

// Interrupts counts the [Session.Interrupt] calls.
func (s *Session) Interrupts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interrupts
}

// replay emits the script and then exactly one terminal event, closes the
// channel, and unblocks Wait. It is the contract the run loop is written
// against, so it is worth stating plainly: one terminal event, then close,
// always — including when the session is interrupted or the consumer stops
// reading.
func (s *Session) replay(script []adapter.Event, result adapter.Result, waitErr error, delay time.Duration) {
	defer close(s.done)
	defer close(s.events)

	var (
		seq   uint64
		usage adapter.Usage
	)
	emit := func(ev adapter.Event) bool {
		seq++
		ev.Seq = seq
		if ev.At.IsZero() {
			ev.At = time.Now()
		}
		if ev.Usage != nil {
			usage.Add(*ev.Usage)
		}
		select {
		case s.events <- ev:
			return true
		case <-s.ctx.Done():
			return false
		}
	}

	delivered := true
	for _, ev := range script {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-s.ctx.Done():
				delivered = false
			}
		}
		if !delivered {
			break
		}
		if ev.Kind.Terminal() {
			// A scripted terminal event ends the session on its own terms —
			// but it is emitted carrying the result Wait will report, so a
			// consumer reading the event and a consumer calling Wait never
			// disagree.
			final, err := s.finish(ev, result, waitErr, usage)
			ev.Result = &final
			if ev.Kind == adapter.EventError && ev.Err == nil {
				ev.Err = err
			}
			emit(ev)
			return
		}
		if !emit(ev) {
			delivered = false
			break
		}
	}

	s.mu.Lock()
	interrupted := s.interrupted
	s.mu.Unlock()

	final := result
	final.Usage = usage
	final.SessionID = s.sessionID
	final.Duration = time.Since(s.started)
	if interrupted {
		final.Status = adapter.StatusInterrupted
		waitErr = nil
	}

	s.mu.Lock()
	s.result = final
	s.err = waitErr
	s.mu.Unlock()

	kind := adapter.EventDone
	ev := adapter.Event{Kind: kind, Result: &final, Text: final.Summary}
	if waitErr != nil {
		ev = adapter.Event{Kind: adapter.EventError, Err: waitErr, Text: waitErr.Error()}
	}
	// Best effort: an interrupted consumer that has stopped reading must not
	// wedge the session, and Wait is already satisfied by the state above.
	emit(ev)
}

// finish records the result carried by a scripted terminal event and returns
// what [Session.Wait] will report.
func (s *Session) finish(ev adapter.Event, fallback adapter.Result, waitErr error, usage adapter.Usage) (adapter.Result, error) {
	final := fallback
	if ev.Result != nil {
		final = *ev.Result
	}
	if final.Usage == (adapter.Usage{}) {
		final.Usage = usage
	}
	if final.SessionID == "" {
		final.SessionID = s.sessionID
	}
	final.Duration = time.Since(s.started)
	err := waitErr
	if ev.Kind == adapter.EventError && ev.Err != nil {
		err = ev.Err
		if final.Status == adapter.StatusSucceeded {
			final.Status = adapter.StatusFailed
		}
	}
	s.mu.Lock()
	s.result = final
	s.err = err
	s.mu.Unlock()
	return final, err
}
