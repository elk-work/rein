package gemini

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/secretenv"
)

// session is one process of either Google program running one turn: the
// prompt is written to stdin and stdin closed, and the program exits once it
// has answered.
type session struct {
	spec    adapter.RunSpec
	cmd     *exec.Cmd
	program string                        // "Gemini CLI" or "Antigravity", for what a person reads
	decode  func([]byte) (decoded, error) // the program's stream-json
	cleanup func()                        // removes what the session left on disk; may be nil
	notes   []string                      // progress lines emitted after the first
	started time.Time
	grace   time.Duration
	red     *secretenv.Redactor
	stderr  *stderrWatch

	events chan adapter.Event
	done   chan struct{}

	mu          sync.Mutex
	id          string
	model       string
	seq         uint64
	interrupted bool
	timedOut    bool
	signIn      bool
	result      adapter.Result
	err         error
}

// signInMarkers are Gemini CLI's words when it is about to start a Google
// sign-in. Rein's settings make that unreachable; seeing one anyway stops the
// session at once rather than let a sign-in proceed.
var signInMarkers = []string{"Opening authentication page", "Waiting for auth", "Code Assist login required",
	"Please visit the following URL to authorize"}

// launch is what differs between the two programs.
type launch struct {
	program string
	bin     string
	args    []string
	env     []string
	stdin   []byte
	key     string
	decode  func([]byte) (decoded, error)
	signIn  []string
	fold    bool     // match signIn without regard to case (agy)
	notes   []string // progress lines a person reads before the program's own
	cleanup func()
}

// startProcess launches Gemini CLI.
func (a *Adapter) startProcess(spec adapter.RunSpec, bin string, args []string, h *home, key string) (adapter.Session, error) {
	return a.launch(spec, launch{
		program: "Gemini CLI", bin: bin, args: args, env: h.childEnv(spec.InheritedEnv(), spec.Env, key),
		stdin: []byte(promptText(spec)), key: key, decode: decode, signIn: signInMarkers, cleanup: h.remove,
	})
}

// launch starts one session.
func (a *Adapter) launch(spec adapter.RunSpec, l launch) (adapter.Session, error) {
	bin, args := l.bin, l.args
	cmd := exec.Command(bin, args...)
	cmd.Dir = spec.WorktreeDir
	cmd.Env = l.env
	setProcAttr(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("gemini: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("gemini: stdout pipe: %w", err)
	}
	s := &session{
		spec:    spec,
		cmd:     cmd,
		program: l.program,
		decode:  l.decode,
		cleanup: l.cleanup,
		started: time.Now(),
		grace:   a.InterruptGrace,
		red:     secretenv.NewRedactor(map[string]string{KeyName: l.key}),
		events:  make(chan adapter.Event),
		done:    make(chan struct{}),
		model:   spec.Model,
		notes:   l.notes,
	}
	if s.grace <= 0 {
		s.grace = defaultInterruptGrace
	}
	s.stderr = &stderrWatch{limit: 64 << 10, markers: l.signIn, fold: l.fold, onSignIn: s.stopSignIn}
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("gemini: starting %s: %w", bin, err)
	}
	// A failed write means the process is already gone; its stderr, read
	// when the stream ends, says why.
	go func() {
		_, _ = stdin.Write(l.stdin)
		_ = stdin.Close()
	}()
	go s.run(stdout)
	if t := spec.Timeouts.Total; t > 0 {
		go func() {
			timer := time.NewTimer(t)
			defer timer.Stop()
			select {
			case <-s.done:
			case <-timer.C:
				s.mu.Lock()
				s.timedOut = true
				s.mu.Unlock()
				_ = s.interrupt()
			}
		}()
	}
	return s, nil
}

// ID implements [adapter.Session]: Gemini CLI's session id.
func (s *session) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// Telemetry implements [adapter.Telemeter]: the model Gemini CLI reports.
func (s *session) Telemetry() adapter.SessionTelemetry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return adapter.SessionTelemetry{Model: s.model}
}

// Events implements [adapter.Session].
func (s *session) Events() <-chan adapter.Event { return s.events }

// Send implements [adapter.Session]. One turn per process.
func (s *session) Send(context.Context, string) error { return adapter.ErrNotSupported }

// Respond implements [adapter.Session]. Headless Gemini CLI never asks.
func (s *session) Respond(context.Context, adapter.PermissionResponse) error {
	return adapter.ErrNotSupported
}

// Interrupt implements [adapter.Session]: SIGINT to the process group, then a
// kill after the grace period.
func (s *session) Interrupt(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.done:
		return adapter.ErrSessionClosed
	default:
	}
	return s.interrupt()
}

func (s *session) interrupt() error {
	s.mu.Lock()
	s.interrupted = true
	s.mu.Unlock()
	if err := interruptProcess(s.cmd); err != nil {
		return s.kill()
	}
	go func() {
		t := time.NewTimer(s.grace)
		defer t.Stop()
		select {
		case <-s.done:
		case <-t.C:
			_ = s.kill()
		}
	}()
	return nil
}

// kill signals the process group twice: a child forked at the instant of the
// first SIGKILL can miss it and keep stdout open, which would leave the
// session waiting on it (the same race the Claude adapter's killNow handles).
func (s *session) kill() error {
	err := killProcess(s.cmd)
	go func() {
		t := time.NewTimer(killRepeat)
		defer t.Stop()
		select {
		case <-s.done:
		case <-t.C:
			_ = killProcess(s.cmd)
		}
	}()
	return err
}

const killRepeat = 250 * time.Millisecond

// stopSignIn kills the session the moment Gemini CLI starts a Google sign-in.
func (s *session) stopSignIn() {
	s.mu.Lock()
	already := s.signIn
	s.signIn = true
	s.mu.Unlock()
	if !already {
		_ = s.kill()
	}
}

// Wait implements [adapter.Session].
func (s *session) Wait() (adapter.Result, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.err
}

func (s *session) emit(ev adapter.Event) {
	s.mu.Lock()
	s.seq++
	ev.Seq = s.seq
	s.mu.Unlock()
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	s.events <- ev
}

// run reads the stream to its end, waits for the process, removes the private
// home and sends the one terminal event.
func (s *session) run(stdout io.Reader) {
	defer close(s.done)
	defer close(s.events)

	s.emit(adapter.Event{Kind: adapter.EventProgress, Text: "Running " + s.program + " on the person's own Google key"})
	for _, n := range s.notes {
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: n})
	}
	if s.spec.Effort != "" && s.program == "Gemini CLI" {
		s.emit(adapter.Event{Kind: adapter.EventProgress,
			Text: "Gemini CLI has no reasoning-effort setting, so the queue's effort " + s.spec.Effort + " was not applied"})
	}
	var res *turnResult
	var text strings.Builder
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		d, err := s.decode(raw)
		if err != nil {
			s.emit(adapter.Event{Kind: adapter.EventProgress,
				Text: s.program + " printed a line Rein could not read: " + firstLine(s.red.String(string(raw)))})
			continue
		}
		if d.sessionID != "" || d.model != "" {
			s.mu.Lock()
			if d.sessionID != "" {
				s.id = d.sessionID
			}
			if d.model != "" && s.model == "" {
				s.model = d.model
			}
			s.mu.Unlock()
		}
		for _, ev := range d.events {
			if ev.Kind == adapter.EventText {
				text.WriteString(ev.Text)
			}
			s.emit(ev)
		}
		if d.result != nil {
			res = d.result
		}
	}
	scanErr := sc.Err()
	waitErr := s.cmd.Wait()
	if s.cleanup != nil {
		s.cleanup()
	}

	s.mu.Lock()
	interrupted, timedOut, signIn, id := s.interrupted, s.timedOut, s.signIn, s.id
	s.mu.Unlock()

	r := adapter.Result{SessionID: id, Duration: time.Since(s.started), ExitCode: exitCode(s.cmd, waitErr),
		Summary: strings.TrimSpace(text.String())}
	if res != nil && r.Summary == "" && res.response != "" {
		r.Summary = strings.TrimSpace(res.response)
	}
	if res != nil {
		for _, u := range res.usage {
			r.Usage.Add(u)
		}
	}
	var err error
	switch {
	case signIn:
		r.Status = adapter.StatusFailed
		err = &signInError{why: s.program + " began a Google sign-in, so Rein stopped it"}
	case timedOut:
		r.Status = adapter.StatusTimedOut
		err = fmt.Errorf("gemini: the session ran past its %s limit and was stopped", s.spec.Timeouts.Total)
	case interrupted:
		r.Status = adapter.StatusInterrupted
		err = errors.New("gemini: the session was interrupted")
	case res == nil:
		r.Status = adapter.StatusFailed
		err = s.noResult(scanErr, waitErr)
	case res.ok:
		r.Status = adapter.StatusSucceeded
	default:
		r.Status = adapter.StatusFailed
		err = fmt.Errorf("gemini: %s ended the turn with an error: %s", s.program, s.red.String(res.error))
	}
	s.mu.Lock()
	s.result, s.err = r, err
	s.mu.Unlock()

	if err != nil {
		s.emit(adapter.Event{Kind: adapter.EventError, Err: err, Text: err.Error()})
		return
	}
	rr := r
	s.emit(adapter.Event{Kind: adapter.EventDone, Result: &rr})
}

func (s *session) noResult(scanErr, waitErr error) error {
	tail := strings.TrimSpace(s.red.String(s.stderr.String()))
	why := s.program + " exited without a result"
	if waitErr != nil {
		why += " (" + waitErr.Error() + ")"
	}
	if scanErr != nil {
		why += "; reading its output failed: " + scanErr.Error()
	}
	if tail != "" {
		return fmt.Errorf("gemini: %s: %s", why, lastLines(tail, 5))
	}
	return errors.New("gemini: " + why)
}

func exitCode(cmd *exec.Cmd, waitErr error) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// stderrWatch keeps the last limit bytes of stderr and calls onSignIn once if
// the program says it is starting a Google sign-in.
type stderrWatch struct {
	mu       sync.Mutex
	limit    int
	markers  []string
	fold     bool
	buf      []byte
	fired    bool
	onSignIn func()
}

func (w *stderrWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	if over := len(w.buf) - w.limit; over > 0 {
		w.buf = w.buf[over:]
	}
	fire := false
	if !w.fired {
		text := string(w.buf)
		if w.fold {
			text = strings.ToLower(text)
		}
		for _, m := range w.markers {
			if w.fold {
				m = strings.ToLower(m)
			}
			if strings.Contains(text, m) {
				w.fired, fire = true, true
				break
			}
		}
	}
	w.mu.Unlock()
	if fire && w.onSignIn != nil {
		w.onSignIn()
	}
	return len(p), nil
}

func (w *stderrWatch) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
