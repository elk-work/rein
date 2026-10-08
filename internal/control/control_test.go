package control_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/control"
)

// tempHome is a short temp directory. t.TempDir() embeds the test's name, and
// on macOS the result plus `/run/control.sock` can pass the ~104-byte limit a
// unix socket path has — which fails as "invalid argument" and looks like
// anything but a path length.
func tempHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// handler is a scriptable [control.Handler].
type handler struct {
	mu        sync.Mutex
	runs      []control.RunInfo
	attachErr error
	inputErr  error
	attached  []string
	released  int
	detached  int
	inputs    []string
	answers   []string
}

func (h *handler) List(context.Context) ([]control.RunInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]control.RunInfo(nil), h.runs...), nil
}

func (h *handler) Attach(_ context.Context, ref string) (control.Attached, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.attachErr != nil {
		return control.Attached{}, nil, h.attachErr
	}
	h.attached = append(h.attached, ref)
	run := control.RunInfo{RunID: "run-1", Queue: "mac-claude", AgentKind: "fake", Interactive: true, Approvals: true}
	if len(h.runs) > 0 {
		run = h.runs[0]
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			h.mu.Lock()
			h.released++
			h.mu.Unlock()
		})
	}
	return control.Attached{Run: run, Note: "the daemon has stepped back"}, release, nil
}

func (h *handler) Input(_ context.Context, runID, text string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inputErr != nil {
		return h.inputErr
	}
	h.inputs = append(h.inputs, runID+": "+text)
	return nil
}

func (h *handler) Respond(_ context.Context, runID, id string, allow bool, reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	verdict := "deny"
	if allow {
		verdict = "allow"
	}
	h.answers = append(h.answers, runID+"/"+id+"="+verdict+":"+reason)
	return nil
}

func (h *handler) Detach(_ context.Context, runID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.detached++
	return nil
}

// state is a lock-free copy of what the handler has seen.
type state struct {
	attached []string
	released int
	detached int
	inputs   []string
	answers  []string
}

func (h *handler) snapshot() state {
	h.mu.Lock()
	defer h.mu.Unlock()
	return state{
		attached: append([]string(nil), h.attached...),
		released: h.released,
		detached: h.detached,
		inputs:   append([]string(nil), h.inputs...),
		answers:  append([]string(nil), h.answers...),
	}
}

// serve starts a control plane on a fresh home and returns it with a client.
func serve(t *testing.T, h *handler) (home string, ln net.Listener) {
	t.Helper()
	home = tempHome(t)
	ln, err := control.Listen(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = control.Serve(ctx, ln, h, nil)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return home, ln
}

func dial(t *testing.T, home string) *control.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := control.Dial(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestListAttachInputRespondDetach(t *testing.T) {
	h := &handler{runs: []control.RunInfo{{
		RunID: "run-1", Queue: "mac-claude", AgentKind: "fake",
		Direction: "Port the sheet", Interactive: true, Approvals: true,
	}}}
	home, _ := serve(t, h)
	c := dial(t, home)

	runs, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].RunID != "run-1" || !runs[0].Interactive {
		t.Fatalf("List = %+v", runs)
	}

	att, err := c.Attach("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if att.Run.RunID != "run-1" || att.Note == "" {
		t.Errorf("Attach = %+v", att)
	}

	if err := c.Input("try it with --verbose"); err != nil {
		t.Fatal(err)
	}
	if err := c.Respond("p1", true, "it is our own repo"); err != nil {
		t.Fatal(err)
	}

	got := h.snapshot()
	if len(got.inputs) != 1 || got.inputs[0] != "run-1: try it with --verbose" {
		t.Errorf("inputs = %v", got.inputs)
	}
	if len(got.answers) != 1 || got.answers[0] != "run-1/p1=allow:it is our own repo" {
		t.Errorf("answers = %v", got.answers)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.snapshot().released == 1 }, "the attachment to be released")
	if got := h.snapshot(); got.detached != 1 {
		t.Errorf("detached = %d, want 1", got.detached)
	}
}

// Errors are answered on the connection, never by dropping it: a client that
// asks for something silly must not be able to stop a daemon serving queues.
func TestRefusalsKeepTheConnection(t *testing.T) {
	h := &handler{attachErr: errors.New("no live run matches \"nope\"")}
	home, _ := serve(t, h)
	c := dial(t, home)

	if _, err := c.Attach("nope"); err == nil || !strings.Contains(err.Error(), "no live run matches") {
		t.Fatalf("Attach = %v", err)
	}
	// Still usable.
	if _, err := c.List(); err != nil {
		t.Fatalf("List after a refused attach: %v", err)
	}
	// And input before attaching is refused rather than silently dropped.
	if err := c.Input("hello"); err == nil || !strings.Contains(err.Error(), "attach first") {
		t.Fatalf("Input before attach = %v", err)
	}
}

func TestOneAttachmentPerConnection(t *testing.T) {
	h := &handler{}
	home, _ := serve(t, h)
	c := dial(t, home)

	if _, err := c.Attach("run-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Attach("run-2"); err == nil || !strings.Contains(err.Error(), "already attached") {
		t.Fatalf("second Attach = %v", err)
	}
}

// A socket file on disk proves nothing — a killed runner leaves one — so the
// test for "somebody is already serving this home" is a connection.
func TestListenRefusesASecondRunnerAndClearsDebris(t *testing.T) {
	h := &handler{}
	home, _ := serve(t, h)

	if _, err := control.Listen(home); !errors.Is(err, control.ErrInUse) {
		t.Fatalf("a second Listen = %v, want ErrInUse", err)
	}

	// Debris from a runner that was killed: the endpoint exists and nothing
	// answers. It must be replaced, not refused.
	debris := tempHome(t)
	if err := os.MkdirAll(filepath.Dir(control.Endpoint(debris)), 0o700); err == nil {
		_ = os.WriteFile(control.Endpoint(debris), nil, 0o600)
	}
	ln, err := control.Listen(debris)
	if err != nil {
		t.Fatalf("Listen over debris: %v", err)
	}
	ln.Close()
}

func TestDialWithNoRunner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := control.Dial(ctx, tempHome(t))
	if !errors.Is(err, control.ErrNoRunner) {
		t.Fatalf("Dial with nothing listening = %v, want ErrNoRunner", err)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
