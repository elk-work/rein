package gemini

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/secretenv"
)

// session is one `agy --print` process running one turn: the prompt is
// written to stdin as one stream-json message and stdin is closed, which is
// how agy is told that turn is the last (it exits once it has answered).
type session struct {
	spec    adapter.RunSpec
	cmd     *exec.Cmd
	home    *home
	started time.Time
	grace   time.Duration
	red     *secretenv.Redactor
	stderr  *boundedBuffer

	events chan adapter.Event
	done   chan struct{}

	mu          sync.Mutex
	id          string
	seq         uint64
	interrupted bool
	timedOut    bool
	result      adapter.Result
	err         error
}

// startProcess launches agy. It is separate from Start so the parts a unit
// test can check — argv, environment, the stdin message — are built in
// functions that need no process.
func (a *Adapter) startProcess(spec adapter.RunSpec, bin string, args []string, h *home, key string) (adapter.Session, error) {
	msg, err := stdinMessage(promptText(spec))
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = spec.WorktreeDir
	cmd.Env = h.childEnv(spec.InheritedEnv(), spec.Env, key)
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
		home:    h,
		started: time.Now(),
		grace:   a.InterruptGrace,
		red:     secretenv.NewRedactor(map[string]string{KeyName: key}),
		stderr:  &boundedBuffer{limit: 64 << 10},
		events:  make(chan adapter.Event),
		done:    make(chan struct{}),
		id:      spec.ResumeID,
	}
	if s.grace <= 0 {
		s.grace = defaultInterruptGrace
	}
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("gemini: starting %s: %w", bin, err)
	}
	// A write that fails means the process is already gone; its stderr, read
	// when the stream ends, says why, so the error is not returned here.
	go func() {
		_, _ = stdin.Write(msg)
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

// stdinMessage is the one stream-json user message agy reads: `"event"`, not
// Claude Code's `"type"` — agy refuses the latter with `stream input message is
// missing the "event" field`.
func stdinMessage(text string) ([]byte, error) {
	blob, err := json.Marshal(map[string]any{
		"event":   "user",
		"message": map[string]any{"role": "user", "content": text},
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: encoding the prompt: %w", err)
	}
	return append(blob, '\n'), nil
}

// ID implements [adapter.Session]: agy's conversation id, which is what
// `--conversation` resumes.
func (s *session) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// Events implements [adapter.Session].
func (s *session) Events() <-chan adapter.Event { return s.events }

// Send implements [adapter.Session]. One turn per process: stdin is closed
// once the prompt is written.
func (s *session) Send(context.Context, string) error { return adapter.ErrNotSupported }

// Respond implements [adapter.Session]. Headless agy never asks.
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
		return killProcess(s.cmd)
	}
	go func() {
		t := time.NewTimer(s.grace)
		defer t.Stop()
		select {
		case <-s.done:
		case <-t.C:
			_ = killProcess(s.cmd)
		}
	}()
	return nil
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

// run reads the stream to its end, waits for the process, and sends the one
// terminal event.
func (s *session) run(stdout io.Reader) {
	defer close(s.done)
	defer close(s.events)

	var res *resultInfo
	var usage adapter.Usage
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		d, err := decode(raw, s.spec.Model)
		if err != nil {
			s.emit(adapter.Event{Kind: adapter.EventProgress,
				Text: "agy printed a line Rein could not read: " + firstLine(s.red.String(string(raw)))})
			continue
		}
		if d.conversationID != "" {
			s.setID(d.conversationID)
		}
		for _, ev := range d.events {
			if ev.Kind == adapter.EventUsage && ev.Usage != nil {
				usage.Add(*ev.Usage)
			}
			s.emit(ev)
		}
		if d.result != nil {
			res = d.result
		}
	}
	scanErr := sc.Err()
	waitErr := s.cmd.Wait()

	s.mu.Lock()
	interrupted, timedOut, id := s.interrupted, s.timedOut, s.id
	s.mu.Unlock()

	r := adapter.Result{SessionID: id, Duration: time.Since(s.started), ExitCode: exitCode(s.cmd, waitErr)}
	if res != nil && res.Usage != nil {
		r.Usage = res.Usage.toAdapter(s.spec.Model)
	} else {
		r.Usage = usage
	}
	var err error
	switch {
	case timedOut:
		r.Status = adapter.StatusTimedOut
		err = fmt.Errorf("gemini: the session ran past its %s limit and was stopped", s.spec.Timeouts.Total)
	case interrupted:
		r.Status = adapter.StatusInterrupted
		err = errors.New("gemini: the session was interrupted")
	case res == nil:
		r.Status = adapter.StatusFailed
		err = s.noResult(scanErr, waitErr)
	case res.Status == "SUCCESS":
		r.Status = adapter.StatusSucceeded
		r.Summary = strings.TrimSpace(res.Response)
	default:
		r.Status = adapter.StatusFailed
		r.Summary = strings.TrimSpace(res.Response)
		why := res.Error
		if why == "" {
			why = "status " + res.Status
		}
		err = fmt.Errorf("gemini: Antigravity CLI ended the turn with an error: %s", s.red.String(why))
	}
	s.mu.Lock()
	s.result, s.err = r, err
	s.mu.Unlock()
	s.recordRun(id)

	if err != nil {
		s.emit(adapter.Event{Kind: adapter.EventError, Err: err, Text: err.Error()})
		return
	}
	rr := r
	s.emit(adapter.Event{Kind: adapter.EventDone, Result: &rr})
}

func (s *session) setID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = id
}

func (s *session) noResult(scanErr, waitErr error) error {
	tail := strings.TrimSpace(s.red.String(s.stderr.String()))
	why := "agy exited without a result"
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

// recordRun notes which conversation a run used, for ReapRun.
func (s *session) recordRun(conversationID string) {
	if s.home == nil || s.spec.RunID == "" || !validID(conversationID) {
		return
	}
	dir := filepath.Join(s.home.root, "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, safeName(s.spec.RunID)), []byte(conversationID+"\n"), 0o600)
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

// boundedBuffer keeps the last limit bytes written to it.
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.limit; over > 0 {
		b.buf = b.buf[over:]
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
