package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// maxStreamLine bounds one NDJSON line. The `system/init` line alone runs to
// tens of kilobytes on a machine with plugins and MCP servers, and a tool
// result can be far larger, so the scanner needs a much bigger ceiling than
// bufio's 64 KiB default. Hitting this is a stream error, not a silent skip.
const maxStreamLine = 16 << 20

// Session is one Claude conversation, with at most one idle-recovery child.
type Session struct {
	spec         adapter.RunSpec
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stderr       *boundedBuffer
	argv         []string
	tmpDir       string
	connectorURL string
	startCtx     context.Context
	grace        time.Duration
	dec          *decoder

	// The background-work wait (ark:rein#80). bgWait, bgSettle and bgBeat
	// are set in Start and only read afterwards.
	bgWait   time.Duration
	bgSettle time.Duration
	bgBeat   time.Duration

	// promptErr is a failure to write the opening prompt, kept so the terminal
	// error can name it when nothing better explains the session. It is written
	// in Start before any goroutine runs and only read afterwards.
	promptErr error

	events chan adapter.Event
	done   chan struct{}

	// hookEvents carries watchdog payloads from the local listener.
	hookEvents  chan hookPayload
	watchdogSrv *watchdog

	started time.Time

	// Activity clocks, read by the watch goroutine.
	lastActivity atomic.Int64 // unix nanos
	initSeen     atomic.Bool
	resultAt     atomic.Int64 // unix nanos, 0 until the terminal result line
	killed       atomic.Bool

	// The background-work clocks, written by the stream goroutine and read
	// by the watch goroutine. bgSince is the unix nanos at which a turn ended
	// with background work still running, 0 when the session is not waiting
	// on any; bgLive is how many tasks are running now; bgSummary names them.
	// bgGivenUp is set by watch once the wait has run out and the agent has
	// been told to finish in the foreground.
	bgSince   atomic.Int64
	bgLive    atomic.Int32
	bgSummary atomic.Value // string
	bgGivenUp atomic.Bool

	emitMu     sync.Mutex
	seq        uint64
	emitClosed bool

	stdinMu     sync.Mutex
	stdinClosed bool

	cleanupOnce sync.Once

	mu             sync.Mutex
	sessionID      string
	telemetry      adapter.SessionTelemetry
	transcriptPath string
	interrupted    bool
	timedOut       bool
	timeoutReason  string
	resumePending  bool
	resumed        bool
	modelActive    bool
	unanswered     map[string]string
	// authRefused is set when the start-up line shows anything but the plan
	// login; the process is killed and the session fails with it.
	authRefused error
	// abandoned is set when the session ended with the agent's background
	// work unfinished — the run failed, whatever its last result said.
	abandoned string
	result    adapter.Result
	err       error
}

// ID implements [adapter.Session]. It is empty until the vendor's
// `system/init` line arrives, which is normally within a second of Start.
func (s *Session) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Telemetry implements [adapter.Telemeter]: what this session can say about
// its own state right now — the model, how full its context is, the
// subscription windows, the MCP inventory.
//
// It reads a snapshot the stream goroutine publishes under the same lock that
// carries the session id, rather than reaching into the decoder. The decoder
// belongs to one goroutine and this is called from another — the run loop's
// telemetry tick — so the copy is the whole point.
//
// It answers before the first event (a zero value, which reports nothing
// rather than reporting zeroes) and after the session has ended (the last
// thing it knew), and never blocks on the agent.
func (s *Session) Telemetry() adapter.SessionTelemetry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.telemetry
}

// Events implements [adapter.Session]. It must be drained: this adapter blocks
// rather than dropping, so a consumer that stops reading stalls the agent.
func (s *Session) Events() <-chan adapter.Event { return s.events }

// Wait implements [adapter.Session]. It is idempotent.
func (s *Session) Wait() (adapter.Result, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.err
}

// Send implements [adapter.Session]: a mid-run steer, delivered as another
// stream-json user message on stdin.
//
// It works only while the turn is in flight. Once the terminal event has been
// emitted, stdin is closed and this returns [adapter.ErrSessionClosed] — a
// follow-up after that is a new session started with
// [adapter.RunSpec.ResumeID], which is what CapResume is for.
func (s *Session) Send(ctx context.Context, input string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.done:
		return adapter.ErrSessionClosed
	default:
	}
	return s.writeUserMessage(input)
}

// Respond implements [adapter.Session].
//
// Always [adapter.ErrNotSupported]: the manifest declares approvals partial,
// and the contract says an adapter that does not declare it yes must refuse
// here. Claude Code 2.1.251 gives a headless caller no way to answer a
// permission prompt — see the manifest note.
func (s *Session) Respond(ctx context.Context, resp adapter.PermissionResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%w: claude declares approvals=partial; `claude -p` auto-denies "+
		"a permission request and offers no channel to answer one", adapter.ErrNotSupported)
}

// Interrupt implements [adapter.Session]: SIGINT, then SIGKILL after the grace
// period. On Windows there is no SIGINT to send, so it kills.
//
// It returns as soon as the request is delivered; the outcome still arrives
// through [Session.Wait], with StatusInterrupted.
func (s *Session) Interrupt(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.done:
		return adapter.ErrSessionClosed
	default:
	}

	s.mu.Lock()
	s.interrupted = true
	cmd := s.cmd
	s.mu.Unlock()
	if err := interruptProcess(cmd); err != nil {
		// Interrupting failed, so escalate immediately rather than waiting out
		// a grace period that protects nothing.
		return s.killNow()
	}

	go func() {
		t := time.NewTimer(s.grace)
		defer t.Stop()
		select {
		case <-s.done:
		case <-t.C:
			_ = s.killNow()
		}
	}()
	return nil
}

// killNow terminates the process and everything it spawned.
//
// The group is signalled twice. A child forked at the instant of the first
// SIGKILL can miss it — measured on macOS when a session was killed the moment
// it printed its init line, with its shell mid-fork — and that child keeps the
// stderr pipe open, so Wait blocks until it exits on its own. The second
// signal, once the first has had time to land, takes it.
func (s *Session) killNow() error {
	s.killed.Store(true)
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	err := killProcess(cmd)
	go func() {
		t := time.NewTimer(killRepeat)
		defer t.Stop()
		select {
		case <-s.done:
		case <-t.C:
			_ = killProcess(cmd)
		}
	}()
	return err
}

// killRepeat is how long killNow waits before signalling the group again.
const killRepeat = 250 * time.Millisecond

// writeUserMessage puts one stream-json user message on stdin.
func (s *Session) writeUserMessage(text string) error {
	blob, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	})
	if err != nil {
		return fmt.Errorf("claude: encoding the user message: %w", err)
	}

	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	if s.stdinClosed {
		return adapter.ErrSessionClosed
	}
	if _, err := s.stdin.Write(append(blob, '\n')); err != nil {
		return fmt.Errorf("claude: writing to the session's stdin: %w", err)
	}
	return nil
}

// closeStdin ends the vendor session. Under --input-format stream-json the
// process waits for more input after every turn, so this is what makes it exit.
func (s *Session) closeStdin() {
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	if s.stdinClosed {
		return
	}
	s.stdinClosed = true
	_ = s.stdin.Close()
}

// emit stamps and delivers one event. Seq counts from 1 without gaps so a
// consumer can tell a dropped event from a quiet agent.
func (s *Session) emit(ev adapter.Event) {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.emitClosed {
		return
	}
	s.seq++
	ev.Seq = s.seq
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	s.events <- s.redactEvent(ev)
}

// finish emits the one terminal event and closes the channel. Everything after
// it is dropped, which is what makes "exactly one terminal event" true even
// when the watchdog is still delivering.
func (s *Session) finish(ev adapter.Event) {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.emitClosed {
		return
	}
	s.seq++
	ev.Seq = s.seq
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	s.events <- s.redactEvent(ev)
	s.emitClosed = true
	close(s.events)
}

func (s *Session) markActivity() { s.lastActivity.Store(time.Now().UnixNano()) }

// run reads the NDJSON stream, translates it, and produces the terminal event.
func (s *Session) run(stdout io.ReadCloser) {
	defer close(s.done)
	defer s.cleanup()

	go s.consumeHooks()
	for {
		s.markActivity()
		stopWatch := make(chan struct{})
		watchDone := make(chan struct{})
		go func() { defer close(watchDone); s.watch(stopWatch) }()
		ev := s.runAttempt(stdout)
		close(stopWatch)
		<-watchDone
		s.mu.Lock()
		retry := s.resumePending && !s.interrupted && !s.timedOut && s.startCtx.Err() == nil
		s.resumePending = false
		s.mu.Unlock()
		if !retry {
			s.finish(ev)
			return
		}
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "claude model request went silent; resuming the same session (attempt 1 of 1)"})
		var err error
		stdout, err = s.resumeChild()
		if err != nil {
			s.finish(s.terminalEvent(nil, err, nil))
			return
		}
	}
}

func (s *Session) runAttempt(stdout io.ReadCloser) adapter.Event {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), maxStreamLine)

	var result *adapter.Result
	// pending is the latest result of a turn that ended while background work
	// was still running. It is not terminal: the session stays open, wakes
	// itself when the work finishes, and its next result supersedes this one.
	var pending *adapter.Result
	// empty is a result line for a turn with no work in it (decoder.emptyTurn).
	// It ends nothing; it stands only if the stream ends with no other result.
	var empty *adapter.Result
	authChecked := false
	for scanner.Scan() {
		s.markActivity()
		evs, res, _ := s.dec.decode([]byte(s.redact(scanner.Text())))
		s.trackRequest(scanner.Bytes())

		// Publish what the decoder now knows, under the lock that guards
		// everything another goroutine may read while this one is running.
		s.mu.Lock()
		if id := s.dec.sessionID; id != "" {
			s.sessionID = id
		}
		s.telemetry = s.dec.telemetry()
		s.mu.Unlock()
		if s.dec.sawInit {
			s.initSeen.Store(true)
		}
		s.bgLive.Store(int32(len(s.dec.background)))
		s.bgSummary.Store(s.dec.backgroundSummary())

		for _, ev := range evs {
			s.emit(ev)
		}

		// The plan-login check, on the first init line: before the agent has
		// made a tool call, so a refused session has done no work. Killed,
		// not interrupted — a graceful stop would let the turn carry on
		// spending on the account it should never have used (planauth.go).
		if s.dec.sawInit && !authChecked {
			authChecked = true
			if err := checkPlanInit(s.dec.apiKeySource, s.spec.Hosted); err != nil {
				s.mu.Lock()
				s.authRefused = err
				s.mu.Unlock()
				_ = s.killNow()
				break
			}
		}

		if res == nil || result != nil {
			continue
		}
		if s.dec.emptyTurn {
			// Not the agent's turn — Claude Code's own bookkeeping, written
			// before the prompt is read. Closing stdin here ends the session
			// thirty seconds into the real turn, with nothing to submit.
			if empty == nil {
				empty = res
			}
			s.emit(adapter.Event{
				Kind: adapter.EventProgress,
				Text: "claude reported a turn with no work in it; waiting for the agent's own turn",
			})
			continue
		}
		if live := len(s.dec.background); live > 0 && !s.bgGivenUp.Load() {
			// The turn ended with background work still running — helper
			// agents, background shells, a monitor. That is not the end of
			// the run (ark:rein#80): closing stdin here would end the session
			// and kill that work, and the run would submit whatever the agent
			// said while it was waiting. Keep stdin open; Claude Code starts
			// a new turn by itself when the work reports back.
			pending = res
			if s.bgSince.Load() == 0 {
				s.bgSince.Store(time.Now().UnixNano())
			}
			s.emit(adapter.Event{
				Kind: adapter.EventProgress,
				Text: "the agent's turn ended with background work still running: " +
					s.dec.backgroundSummary() + ". Keeping the session open until it finishes.",
			})
			continue
		}
		// The terminal result. Under --input-format stream-json the process
		// would otherwise sit waiting for another turn, so closing stdin here
		// is what ends the session — and what keeps one Rein run equal to one
		// Claude Code session.
		if live := len(s.dec.background); live > 0 {
			// The wait ran out and the agent still left work running.
			s.abandon("the agent's background work was still running when Rein stopped waiting for it: " +
				s.dec.backgroundSummary())
		}
		result = res
		s.bgSince.Store(0)
		s.resultAt.Store(time.Now().UnixNano())
		s.closeStdin()
	}
	scanErr := scanner.Err()

	if result == nil && pending != nil {
		// The stream ended while the session was waiting on background work:
		// the process died, was killed, or Rein closed stdin after the work
		// had all finished and the agent did not pick it up. Only the last
		// case leaves nothing lost.
		result = pending
		if len(s.dec.background) > 0 {
			s.abandon("the session ended while the agent's background work was still running: " +
				s.dec.backgroundSummary())
		}
	}

	if result == nil && pending == nil {
		result = empty
	}

	// The stream is finished; make sure the process is too, then reap it.
	s.closeStdin()
	waitErr := s.cmd.Wait()

	return s.terminalEvent(result, scanErr, waitErr)
}

// abandon records that the session ended with the agent's background work
// unfinished. The first reason stands.
func (s *Session) abandon(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.abandoned == "" {
		s.abandoned = reason
	}
}

// terminalEvent settles the outcome and builds the single closing event. The
// Result it carries is the same one Wait returns, so a consumer reading events
// and a consumer calling Wait never disagree.
func (s *Session) terminalEvent(result *adapter.Result, scanErr, waitErr error) adapter.Event {
	s.mu.Lock()
	interrupted := s.interrupted
	timedOut := s.timedOut
	reason := s.timeoutReason
	authRefused := s.authRefused
	abandoned := s.abandoned
	transcript := s.transcriptPath
	sessionID := s.sessionID
	s.mu.Unlock()

	final := adapter.Result{
		Status:         adapter.StatusFailed,
		SessionID:      sessionID,
		TranscriptPath: transcript,
	}
	if result != nil {
		final = *result
		final.TranscriptPath = transcript
		if final.SessionID == "" {
			final.SessionID = sessionID
		}
	}
	final.Duration = time.Since(s.started)
	if s.cmd.ProcessState != nil {
		final.ExitCode = s.cmd.ProcessState.ExitCode()
	}

	var err error
	switch {
	case authRefused != nil:
		// Outranks everything, a timeout included: we killed the process
		// because of what it said about its login, and that is the story.
		final.Status = adapter.StatusFailed
		final.Summary = authRefused.Error()
		err = authRefused

	case timedOut:
		// A timeout outranks everything: the process was killed by us, so its
		// exit status says nothing about the work.
		final.Status = adapter.StatusTimedOut
		err = fmt.Errorf("claude: session timed out: %s", reason)

	case interrupted:
		final.Status = adapter.StatusInterrupted
		err = nil

	case abandoned != "":
		// Whatever the agent's last turn said, the work it started in the
		// background did not finish, so the run did not either. Failing it
		// is what makes the run loop submit stuck rather than ready.
		final.Status = adapter.StatusFailed
		err = fmt.Errorf("claude: %s", abandoned)

	case result == nil:
		// No result line ever arrived. The process died, or the stream broke.
		final.Status = adapter.StatusFailed
		err = s.noResultError(scanErr, waitErr)

	case final.Status != adapter.StatusSucceeded:
		err = fmt.Errorf("claude: session finished unsuccessfully: %s",
			firstNonEmpty(final.Summary, "no summary given"))

	default:
		err = nil
	}

	s.mu.Lock()
	s.result = final
	s.err = err
	s.mu.Unlock()

	if err != nil && final.Status != adapter.StatusInterrupted {
		return adapter.Event{
			Kind:   adapter.EventError,
			Err:    err,
			Text:   err.Error(),
			Result: &final,
		}
	}
	return adapter.Event{Kind: adapter.EventDone, Result: &final, Text: final.Summary}
}

// noResultError explains a session that produced no result line, using whatever
// the process left behind — an exit status, a scanner failure, stderr.
func (s *Session) noResultError(scanErr, waitErr error) error {
	tail := strings.TrimSpace(s.redact(s.stderr.String()))
	switch {
	case scanErr != nil && errors.Is(scanErr, bufio.ErrTooLong):
		return fmt.Errorf("claude: a stream line exceeded %d bytes; the session produced no result", maxStreamLine)
	case scanErr != nil:
		return fmt.Errorf("claude: reading the event stream: %w", scanErr)
	case waitErr != nil && tail != "":
		return fmt.Errorf("claude: exited without a result: %w: %s", waitErr, truncate(tail, 2000))
	case waitErr != nil:
		return fmt.Errorf("claude: exited without a result: %w", waitErr)
	case tail != "":
		return fmt.Errorf("claude: the stream ended with no result line: %s", truncate(tail, 2000))
	case s.promptErr != nil:
		// Last, not first: a process that rejected its own arguments explains
		// itself far better on stderr than the EPIPE we got writing to it.
		return fmt.Errorf("claude: the prompt could not be delivered: %w", s.promptErr)
	default:
		return errors.New("claude: the stream ended with no result line")
	}
}

// watch enforces the spec's timeouts. The run loop has its own watchdog ladder,
// but the contract says an adapter must not accept a timeout it will not
// enforce, so all three are enforced here. It also runs the background-work
// wait (ark:rein#80) — see [Session.watchBackground].
func (s *Session) watch(stop <-chan struct{}) {
	startup := s.spec.Timeouts.Startup
	if startup <= 0 {
		startup = defaultStartup
	}
	idle := s.spec.Timeouts.Idle
	total := s.spec.Timeouts.Total

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()

	var bg bgWaitState
	attemptStarted := time.Now()
	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			// Waiting on live background work is not idleness: a background
			// shell watching a CI run is silent for as long as the run takes.
			// The wait has its own bound instead.
			waitingOnWork := s.bgSince.Load() != 0 && s.bgLive.Load() > 0
			switch {
			case !s.initSeen.Load() && now.Sub(attemptStarted) > startup:
				s.timeout(fmt.Sprintf("no session start within %s", startup))
				return
			case total > 0 && now.Sub(s.started) > total:
				s.timeout(fmt.Sprintf("exceeded the total budget of %s", total))
				return
			case idle > 0 && s.resultAt.Load() == 0 && !waitingOnWork &&
				now.Sub(time.Unix(0, s.lastActivity.Load())) > idle:
				s.idleTimeout(idle)
				return
			case s.resultAt.Load() > 0 &&
				now.Sub(time.Unix(0, s.resultAt.Load())) > defaultExitGrace:
				// The turn is over and stdin is closed, but the process has
				// not exited. Reap it rather than leaking it.
				_ = s.killNow()
				return
			}
			if s.watchBackground(now, &bg) {
				return
			}
		}
	}
}

// bgWaitState is watch's own memory of one background-work wait.
type bgWaitState struct {
	since    int64     // the bgSince this state belongs to
	lastBeat time.Time // the last "still waiting" progress event
	drained  time.Time // when the live count was first seen at zero
	nudged   time.Time // when the agent was told its work had finished
	steered  time.Time // when the agent was told to finish in the foreground
}

// Nudges sent to the agent during a background-work wait. They are user
// messages on stdin, the same channel a steer uses.
const (
	bgNudge = "Rein: your background work has finished. Pick up its results " +
		"and finish the task; do not end your turn while anything is still running in the background."
	bgSteer = "Rein: this run cannot wait any longer for background work. Stop " +
		"anything still running in the background, finish what you can in the foreground, " +
		"and say plainly what is unfinished."
)

// watchBackground drives a session whose turn ended with background work
// still running. It reports true when it has ended the session.
//
//   - Every bgBeat it emits progress naming the work, so neither Elk's lease
//     nor the run loop's stall watchdog reads the wait as a dead agent.
//   - Once the work has all finished, Claude Code normally starts the next
//     turn by itself within seconds. If it has not done so after bgSettle, the
//     agent is nudged once; after bgSettle more, stdin is closed and the last
//     result stands — the work finished, so nothing was lost.
//   - After bgWait the agent is told to finish in the foreground, and the next
//     result is terminal whatever is still running. If none comes within
//     bgSettle the process is killed, and the run fails as abandoned.
func (s *Session) watchBackground(now time.Time, st *bgWaitState) bool {
	since := s.bgSince.Load()
	if since == 0 || s.resultAt.Load() != 0 {
		return false
	}
	if st.since != since {
		*st = bgWaitState{since: since, lastBeat: now}
	}
	live := s.bgLive.Load()
	quiet := now.Sub(time.Unix(0, s.lastActivity.Load()))

	if live > 0 {
		st.drained, st.nudged = time.Time{}, time.Time{}
		if now.Sub(st.lastBeat) >= s.bgBeat {
			st.lastBeat = now
			summary, _ := s.bgSummary.Load().(string)
			s.emit(adapter.Event{
				Kind: adapter.EventProgress,
				Text: "waiting for the agent's background work: " + summary,
			})
		}
	} else if st.drained.IsZero() {
		st.drained = now
	}

	switch {
	case !st.steered.IsZero():
		if now.Sub(st.steered) > s.bgSettle && quiet > s.bgSettle {
			s.abandon("the agent did not finish in the foreground after Rein stopped waiting for its background work")
			_ = s.killNow()
			return true
		}

	case now.Sub(time.Unix(0, since)) > s.bgWait:
		s.bgGivenUp.Store(true)
		st.steered = now
		if err := s.writeUserMessage(bgSteer); err != nil {
			s.abandon("Rein stopped waiting for the agent's background work and could not tell it so: " + err.Error())
			_ = s.killNow()
			return true
		}
		s.emit(adapter.Event{Kind: adapter.EventProgress,
			Text: fmt.Sprintf("background work still running after %s; told the agent to finish in the foreground", s.bgWait)})

	case live == 0 && st.nudged.IsZero() &&
		now.Sub(st.drained) > s.bgSettle && quiet > s.bgSettle:
		st.nudged = now
		if err := s.writeUserMessage(bgNudge); err == nil {
			s.emit(adapter.Event{Kind: adapter.EventProgress,
				Text: "the agent's background work has finished; nudged it to pick up the results"})
		}

	case live == 0 && !st.nudged.IsZero() &&
		now.Sub(st.nudged) > s.bgSettle && quiet > s.bgSettle:
		// Its work is done and it will not take another turn. The last
		// result stands; closing stdin ends the session.
		// watch keeps running: the exit grace now applies.
		s.resultAt.Store(now.UnixNano())
		s.closeStdin()
	}
	return false
}

// timeout marks the session timed out and kills the process, which ends the
// stream and lets run produce the terminal event.
func (s *Session) timeout(reason string) {
	s.mu.Lock()
	if s.timedOut {
		s.mu.Unlock()
		return
	}
	s.timedOut = true
	s.timeoutReason = reason
	s.mu.Unlock()
	_ = s.killNow()
}

// consumeHooks turns watchdog payloads into events. It exits when the session
// does; anything it emits after the terminal event is dropped by emit.
func (s *Session) consumeHooks() {
	for {
		select {
		case <-s.done:
			return
		case p, ok := <-s.hookEvents:
			if !ok {
				return
			}
			s.markActivity()
			if ev, emit := s.hookEvent(p); emit {
				s.emit(ev)
			}
		}
	}
}

// hookEvent maps one hook payload onto Rein's vocabulary.
//
// The Stop hook is recorded rather than reported: it fires immediately before
// the `result` line, so emitting an idle event for it would tell the watchdog a
// finishing session had stalled. What it is genuinely good for is the two
// fields the stream never carries — the transcript path and the last assistant
// message.
func (s *Session) hookEvent(p hookPayload) (adapter.Event, bool) {
	switch p.HookEventName {
	case "Stop", "SubagentStop":
		s.mu.Lock()
		if p.TranscriptPath != "" {
			s.transcriptPath = p.TranscriptPath
		}
		s.mu.Unlock()
		return adapter.Event{}, false

	case "Notification":
		switch p.NotificationType {
		case "idle_prompt", "agent_needs_input":
			// The contract names exactly these as the source of EventIdle.
			return adapter.Event{
				Kind: adapter.EventIdle,
				Text: firstNonEmpty(p.Message, "the agent is waiting for input"),
				Raw:  p.Raw,
			}, true
		case "permission_prompt":
			return adapter.Event{
				Kind: adapter.EventProgress,
				Text: "claude raised a permission prompt, which a headless run cannot answer: " +
					firstNonEmpty(p.Message, "(no message)"),
				Raw: p.Raw,
			}, true
		default:
			// No notification_type means no signal Rein can name. Progress,
			// never a guess at idleness from the message text.
			return adapter.Event{
				Kind: adapter.EventProgress,
				Text: "claude notification: " + firstNonEmpty(p.Message, p.NotificationType),
				Raw:  p.Raw,
			}, true
		}

	default:
		return adapter.Event{}, false
	}
}

// cleanup releases the watchdog listener and the temp directory holding the
// generated settings and MCP config.
func (s *Session) cleanup() {
	// Once, and without clearing the fields: they are written before the
	// session's goroutines start and read afterwards, so leaving them alone
	// keeps them race-free.
	s.cleanupOnce.Do(func() {
		if s.watchdogSrv != nil {
			s.watchdogSrv.close()
		}
		if s.tmpDir != "" {
			_ = os.RemoveAll(s.tmpDir)
		}
	})
}

// boundedBuffer keeps at most limit bytes of a process's stderr — enough to
// explain a failure, not enough for a runaway to exhaust the daemon's memory.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = b.buf[len(b.buf)-b.limit:]
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// trackRequest distinguishes model silence from tools whose results never arrived.
// Unknown housekeeping events do not clear outstanding calls.
func (s *Session) trackRequest(raw []byte) {
	var line streamLine
	if json.Unmarshal(raw, &line) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unanswered == nil {
		s.unanswered = make(map[string]string)
	}
	switch line.Type {
	case "api-request", "api_request", "assistant", "stream_event":
		s.modelActive = true
	}
	if line.Type != "assistant" && line.Type != "user" {
		return
	}
	var msg vendorMessage
	if json.Unmarshal(line.Message, &msg) != nil {
		return
	}
	for _, b := range msg.Content {
		switch b.Type {
		case "tool_use":
			var input struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(b.Input, &input)
			detail := b.Name
			if input.Command != "" {
				detail += ": " + input.Command
			}
			s.unanswered[b.ID] = detail
		case "tool_result":
			delete(s.unanswered, b.ToolUseID)
			s.modelActive = true
		}
	}
}

func (s *Session) idleTimeout(idle time.Duration) {
	s.mu.Lock()
	if s.modelActive && len(s.unanswered) == 0 && s.sessionID != "" && !s.resumed && !s.interrupted && !s.timedOut {
		s.resumed, s.resumePending = true, true
		s.mu.Unlock()
		_ = s.killNow()
		return
	}
	reason := fmt.Sprintf("no event for %s", idle)
	var tools []string
	for _, detail := range s.unanswered {
		tools = append(tools, detail)
	}
	sort.Strings(tools)
	if len(tools) > 0 {
		reason += "; unanswered tool calls: " + strings.Join(tools, "; ")
	}
	s.mu.Unlock()
	s.timeout(s.redact(reason))
}

// resumeChild preserves launch policy and generated configuration. The old
// child has been reaped and its watcher joined before this is called.
func (s *Session) resumeChild() (io.ReadCloser, error) {
	args := append([]string(nil), s.argv[1:]...)
	for i := 0; i < len(args); i++ {
		if args[i] == "--resume" {
			args = append(args[:i], args[i+2:]...)
			break
		}
	}
	args = append(args, "--resume", s.ID())
	cmd := exec.Command(s.argv[0], args...)
	cmd.Dir, cmd.Env = s.spec.WorktreeDir, s.cmd.Env
	setProcAttr(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr := &boundedBuffer{limit: 64 << 10}
	cmd.Stderr = stderr
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.interrupted || s.timedOut || s.startCtx.Err() != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, errors.New("claude: resume cancelled")
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("claude: resuming session: %w", err)
	}
	s.cmd, s.stderr = cmd, stderr
	s.stdinMu.Lock()
	s.stdin, s.stdinClosed = stdin, false
	s.stdinMu.Unlock()
	s.dec = &decoder{}
	s.modelActive = false
	s.killed.Store(false)
	s.initSeen.Store(false)
	s.resultAt.Store(0)
	s.markActivity()
	s.promptErr = s.writeUserMessage("Rein: continue where you left off and finish the task.")
	return stdout, nil
}
