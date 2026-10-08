package runner_test

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/control"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
)

// listen opens a control endpoint on a home of the test's own.
//
// os.MkdirTemp rather than t.TempDir: the latter embeds the test's name, and on
// macOS that plus `/run/control.sock` can pass the ~104-byte limit a unix
// socket path has — which fails as "invalid argument" and looks like anything
// but a path length.
func listen(t *testing.T) (home string, ln net.Listener) {
	t.Helper()
	home, err := os.MkdirTemp("", "rn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	ln, err = control.Listen(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return home, ln
}

// slowScript is a session long enough for a person to reach: n events, one per
// [fake.Adapter.Delay]. Without it the fake finishes before a test can dial.
func slowScript(n int) []adapter.Event {
	out := make([]adapter.Event, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, adapter.Event{Kind: adapter.EventText, Text: "step"})
	}
	return out
}

// attachToLive waits for the runner to register a live session and attaches to
// it, the way `rein attach` with no argument does.
func attachToLive(t *testing.T, home string) (*control.Client, control.Attached) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		c, err := control.Dial(ctx, home)
		cancel()
		if err == nil {
			att, aErr := c.Attach("")
			if aErr == nil {
				return c, att
			}
			c.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("no live run appeared on the control socket")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForRun(t *testing.T, done <-chan error, log any) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%v\nlog:\n%v", err, log)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the run never finished\nlog:\n%v", log)
	}
}

// A person attaches to a live run over the control socket, sends it a line and
// detaches. The run finishes normally, the agent received what was typed, and
// the deliverable says a person took part — in one line, without the
// transcript.
func TestAttachOverTheControlSocket(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	logs := openRunLogs(t)
	home, ln := listen(t)

	h.agent.Delay = 20 * time.Millisecond
	h.agent.Script = slowScript(60)

	done := make(chan error, 1)
	go func() { done <- h.run(runner.Options{Logs: logs, ControlListener: ln}) }()

	c, att := attachToLive(t, home)
	if att.Run.Queue != queue || !att.Run.Interactive {
		t.Fatalf("attached to %+v", att.Run)
	}
	if !strings.Contains(att.Note, "stepped back") {
		t.Errorf("the attach note does not say the daemon stepped back: %q", att.Note)
	}
	if err := c.Input("stop after the current file"); err != nil {
		t.Fatalf("Input: %v", err)
	}
	// While attached, the run's meta says so — which is what `rein tail` puts
	// in the LAST column.
	waitUntil(t, func() bool {
		m, err := logs.ReadMeta("run-1")
		return err == nil && m.Attached
	}, "the run to read as attached")

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	waitForRun(t, done, h.log)

	sessions := h.agent.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d", len(sessions))
	}
	if sent := sessions[0].Sent(); len(sent) != 1 || sent[0] != "stop after the current file" {
		t.Errorf("the agent received %v", sent)
	}

	// One line in the deliverable, and never what they typed: a deliverable is
	// read by whoever asked for the work, and a takeover transcript is not
	// theirs.
	deliverable := h.submitted().Arg("deliverable")
	if !strings.Contains(deliverable, "A person attached to this session") ||
		!strings.Contains(deliverable, "sent it one message") {
		t.Errorf("the deliverable does not record the intervention:\n%s", deliverable)
	}
	if strings.Contains(deliverable, "stop after the current file") {
		t.Errorf("the deliverable carries the person's own words:\n%s", deliverable)
	}

	// The local run log does carry them, because that is the account of the
	// run and it never leaves the machine.
	recs, err := logs.Records("run-1")
	if err != nil {
		t.Fatal(err)
	}
	var attachText []string
	for _, r := range recs {
		if r.Kind == string(runlog.KindAttach) {
			attachText = append(attachText, r.Text)
		}
	}
	joined := strings.Join(attachText, "\n")
	for _, want := range []string{"a person attached", "a person sent: stop after the current file", "detached after"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the run log is missing %q:\n%s", want, joined)
		}
	}
	if m, err := logs.ReadMeta("run-1"); err != nil || m.Attached {
		t.Errorf("the run still reads as attached after the detach: %+v %v", m, err)
	}
}

// While a person is attached the watchdog is held: silence from the agent is
// them thinking, not the agent dying, and killing a session somebody is in the
// middle of driving is the worst possible reading of the same signal.
func TestAttachPausesTheWatchdog(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	logs := openRunLogs(t)
	home, ln := listen(t)

	// The agent says nothing until well past the stall threshold.
	h.agent.Delay = 1500 * time.Millisecond
	h.agent.Script = []adapter.Event{{Kind: adapter.EventText, Text: "finally"}}

	done := make(chan error, 1)
	go func() {
		done <- h.run(runner.Options{
			Logs: logs, ControlListener: ln, StallAfter: 400 * time.Millisecond,
		})
	}()

	c, _ := attachToLive(t, home)
	waitForRun(t, done, h.log)
	c.Close()

	if !strings.Contains(h.log.String(), "a person is attached — not stalling") {
		t.Errorf("the watchdog was not held for the attached person:\n%s", h.log)
	}
	if sub := h.submitted(); sub.Arg("status") != "ready" {
		t.Errorf("status = %q, want ready — the run stalled with somebody driving it\n%s",
			sub.Arg("status"), h.log)
	}
}

// Denying is what a run with nobody at the keyboard owes a permission request.
// While somebody IS at the keyboard that premise is gone, so the request goes
// to them — and the mode is not weakened, because a human answering is not the
// same as auto-approval.
func TestAttachedPersonAnswersAPermissionRequest(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	logs := openRunLogs(t)
	home, ln := listen(t)

	h.agent.Delay = 30 * time.Millisecond
	script := slowScript(8)
	script = append(script, adapter.Event{Kind: adapter.EventPermissionRequest,
		Permission: &adapter.PermissionRequest{ID: "p1", Tool: "Bash", Summary: "git push"}})
	script = append(script, slowScript(4)...)
	h.agent.Script = script

	done := make(chan error, 1)
	go func() { done <- h.run(runner.Options{Logs: logs, ControlListener: ln}) }()

	c, _ := attachToLive(t, home)

	// Wait for the agent to actually ask, then answer as a person would.
	waitUntil(t, func() bool {
		recs, err := logs.Records("run-1")
		if err != nil {
			return false
		}
		for _, r := range recs {
			if r.Kind == string(adapter.EventPermissionRequest) {
				return true
			}
		}
		return false
	}, "the agent to ask for permission")
	if err := c.Respond("p1", true, "it is our own repository"); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	waitForRun(t, done, h.log)
	c.Close()

	resps := h.agent.Sessions()[0].Responses()
	if len(resps) != 1 || !resps[0].Allow || resps[0].ID != "p1" {
		t.Fatalf("the agent got %+v, want one allow for p1\nlog:\n%s", resps, h.log)
	}
	// Elk still hears about it: a decision made outside the loop would be one
	// nobody could see afterwards.
	var gate string
	for _, call := range h.elk.CallsTo("report_progress") {
		if call.Arg("kind") == "gate" {
			gate = call.Arg("body")
		}
	}
	if !strings.Contains(gate, "A person was attached") || !strings.Contains(gate, "allowed it") {
		t.Errorf("the gate event does not record who decided: %q", gate)
	}
}

// An adapter with no `steer` capability can be watched and not driven, and
// attach says so before the person types rather than after.
func TestAttachToAnAdapterThatTakesNoInput(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	logs := openRunLogs(t)
	home, ln := listen(t)

	h.agent.Capabilities.Capabilities[adapter.CapSteer] = adapter.SupportNo
	h.agent.Delay = 20 * time.Millisecond
	h.agent.Script = slowScript(40)

	done := make(chan error, 1)
	go func() { done <- h.run(runner.Options{Logs: logs, ControlListener: ln}) }()

	c, att := attachToLive(t, home)
	if att.Run.Interactive {
		t.Errorf("an adapter declaring steer:no reported itself interactive")
	}
	if !strings.Contains(att.Note, "Watch only") {
		t.Errorf("the note does not warn that nothing can be sent: %q", att.Note)
	}
	if err := c.Input("please stop"); err == nil || !strings.Contains(err.Error(), "steer") {
		t.Errorf("Input on a non-steerable session = %v", err)
	}
	c.Close()
	waitForRun(t, done, h.log)

	if sent := h.agent.Sessions()[0].Sent(); len(sent) != 0 {
		t.Errorf("something reached a session that takes no input: %v", sent)
	}
}

// Attaching to a machine that is driving nothing is an error that says so, and
// points at the command that does answer the question.
//
// It serves [runner.Runner.Control] directly rather than running the loop: the
// control plane's lifetime is the loop's, and a loop with nothing to claim
// returns at once — so a test that started one would be racing the shutdown it
// causes for the connection it needs. That race is exactly what failed on the
// Windows leg, where a named pipe stops accepting the instant its listener
// closes while a unix socket leaves a file behind that a dial can still reach.
func TestAttachWithNothingLive(t *testing.T) {
	h := newHarness(t)
	home, ln := listen(t)

	r, err := runner.New(h.prepare(runner.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = control.Serve(ctx, ln, r.Control(), nil)
	}()
	defer func() {
		cancel()
		<-served
	}()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()
	c, err := control.Dial(dialCtx, home)
	if err != nil {
		t.Fatalf("never reached the control plane: %v", err)
	}
	defer c.Close()

	if _, err := c.Attach(""); err == nil || !strings.Contains(err.Error(), "no run is being driven") {
		t.Fatalf("Attach with nothing live = %v", err)
	}
	if runs, err := c.List(); err != nil || len(runs) != 0 {
		t.Errorf("List = %v, %v", runs, err)
	}
}
