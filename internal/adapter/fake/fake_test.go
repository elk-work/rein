package fake_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/fake"
)

func spec(t *testing.T) adapter.RunSpec {
	t.Helper()
	return adapter.RunSpec{
		RunID:          "run-1",
		WorktreeDir:    t.TempDir(),
		Prompt:         "do the thing",
		SystemPrompt:   "you are a runner",
		PermissionMode: adapter.PermissionFull,
	}
}

// drain reads a session to completion and returns the events in order.
func drain(t *testing.T, s adapter.Session) []adapter.Event {
	t.Helper()
	var evs []adapter.Event
	for ev := range s.Events() {
		evs = append(evs, ev)
	}
	return evs
}

func TestFakeIsRegistered(t *testing.T) {
	a, ok := adapter.Lookup(fake.Kind)
	if !ok {
		t.Fatalf("importing the package did not register %q", fake.Kind)
	}
	if a.Name() != fake.Kind {
		t.Fatalf("Name = %q", a.Name())
	}
	if err := a.Manifest().Validate(); err != nil {
		t.Fatalf("the registered manifest does not validate: %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	a := fake.New()
	sess, err := a.Start(context.Background(), spec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sess.ID() == "" {
		t.Error("ID is empty before the session ends; attach and resume both need it")
	}

	evs := drain(t, sess)
	if len(evs) < 2 {
		t.Fatalf("got %d events, want the script plus a terminal one", len(evs))
	}
	// Exactly one terminal event, and it is last.
	last := evs[len(evs)-1]
	if !last.Kind.Terminal() {
		t.Fatalf("the final event is %q, want a terminal one", last.Kind)
	}
	for _, ev := range evs[:len(evs)-1] {
		if ev.Kind.Terminal() {
			t.Fatalf("a terminal %q event arrived before the end", ev.Kind)
		}
	}
	// Seq counts from 1, without gaps.
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d has Seq %d", i, ev.Seq)
		}
		if ev.At.IsZero() {
			t.Fatalf("event %d has no timestamp", i)
		}
	}

	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !res.OK() {
		t.Fatalf("Status = %q, want succeeded", res.Status)
	}
	// The terminal event and Wait must agree.
	if last.Result == nil || last.Result.Status != res.Status {
		t.Errorf("the done event and Wait disagree: %+v vs %+v", last.Result, res)
	}
	// Usage accumulates from the events rather than being asserted twice.
	if res.Usage.InputTokens != 1000 || res.Usage.OutputTokens != 250 {
		t.Errorf("Usage = %+v, want the script's totals", res.Usage)
	}
	// Duration is asserted in its own test below, because asserting it here
	// would be asserting the clock rather than the adapter.

	// Wait is idempotent.
	res2, err2 := sess.Wait()
	if res2 != res || !errors.Is(err2, err) {
		t.Error("a second Wait returned something different")
	}
}

func TestDurationIsRecorded(t *testing.T) {
	// This used to be one line inside TestSessionLifecycle — `res.Duration <= 0`
	// on a session with no delay — and the windows-latest leg failed it on the
	// first run it ever had (ark:rein#11).
	//
	// The adapter was right and the assertion was wrong. Go's monotonic clock
	// on Windows advances in system timer ticks, 15.6 ms by default, so a fake
	// session that starts and finishes inside one tick measures **exactly
	// zero** — indistinguishable from a Duration nobody set. On macOS and
	// Linux the clock is fine-grained enough that the same code passes, which
	// is why it went unnoticed.
	//
	// A real session lasts minutes, so nothing was broken in production. The
	// fix is to give the fake something to measure rather than to weaken the
	// assertion: a delay comfortably over a Windows tick makes "was it
	// recorded" a question the clock can actually answer.
	const delay = 25 * time.Millisecond

	a := fake.New()
	a.Delay = delay
	sess, err := a.Start(context.Background(), spec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain(t, sess)
	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Duration < delay {
		t.Errorf("Duration = %s, want at least the %s the session was made to take", res.Duration, delay)
	}
}

func TestStartRefusesWhatTheManifestForbids(t *testing.T) {
	t.Run("missing required capability", func(t *testing.T) {
		a := fake.New()
		a.Capabilities.Capabilities[adapter.CapShell] = adapter.SupportPartial
		s := spec(t)
		s.RequiredCapabilities = []string{"shell"}
		_, err := a.Start(context.Background(), s)
		if !errors.Is(err, adapter.ErrCapability) {
			t.Fatalf("Start = %v, want ErrCapability", err)
		}
	})

	t.Run("ask mode without approvals", func(t *testing.T) {
		a := fake.New()
		a.Capabilities.Capabilities[adapter.CapApprovals] = adapter.SupportNo
		s := spec(t)
		s.PermissionMode = adapter.PermissionAsk
		if _, err := a.Start(context.Background(), s); !errors.Is(err, adapter.ErrCapability) {
			t.Fatalf("Start = %v, want ErrCapability", err)
		}
	})

	t.Run("invalid spec", func(t *testing.T) {
		a := fake.New()
		s := spec(t)
		s.PermissionMode = adapter.PermissionUnset
		if _, err := a.Start(context.Background(), s); err == nil {
			t.Fatal("Start accepted a spec with no permission mode")
		}
	})

	t.Run("a platform it does not run on", func(t *testing.T) {
		a := fake.New()
		a.Capabilities.Platforms = []adapter.Platform{{OS: "plan9"}}
		if _, err := a.Start(context.Background(), spec(t)); !errors.Is(err, adapter.ErrPlatform) {
			t.Fatalf("Start = %v, want ErrPlatform", err)
		}
	})
}

func TestPreflightAndStartErrors(t *testing.T) {
	sentinel := errors.New("claude: not on PATH")
	a := fake.New()
	a.PreflightErr = sentinel
	if err := a.Preflight(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Preflight = %v", err)
	}

	a2 := fake.New()
	a2.StartErr = sentinel
	if _, err := a2.Start(context.Background(), spec(t)); !errors.Is(err, sentinel) {
		t.Fatalf("Start = %v", err)
	}
}

func TestSendAndRespondAreRecorded(t *testing.T) {
	a := fake.New()
	a.Script = []adapter.Event{
		{Kind: adapter.EventQuestion, Text: "which branch?"},
		{Kind: adapter.EventPermissionRequest, Permission: &adapter.PermissionRequest{
			ID: "perm-1", Tool: "Bash", Summary: "rm -rf build"}},
	}
	sess, err := a.Start(context.Background(), spec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx := context.Background()
	for ev := range sess.Events() {
		switch ev.Kind {
		case adapter.EventQuestion:
			if err := sess.Send(ctx, "main"); err != nil {
				t.Errorf("Send: %v", err)
			}
		case adapter.EventPermissionRequest:
			resp := adapter.PermissionResponse{ID: ev.Permission.ID, Allow: false, Reason: "outside the worktree"}
			if err := sess.Respond(ctx, resp); err != nil {
				t.Errorf("Respond: %v", err)
			}
		}
	}
	if _, err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	fs := a.Sessions()[0]
	if got := fs.Sent(); len(got) != 1 || got[0] != "main" {
		t.Errorf("Sent = %v, want [main]", got)
	}
	resps := fs.Responses()
	if len(resps) != 1 || resps[0].ID != "perm-1" || resps[0].Allow {
		t.Errorf("Responses = %+v", resps)
	}
	if resps[0].Reason == "" {
		t.Error("the denial reason was dropped; an agent that does not know why retries")
	}
}

// The manifest and the interface have to agree: an adapter that does not
// declare approvals must refuse to answer one.
func TestRespondRefusedWithoutApprovals(t *testing.T) {
	a := fake.New()
	a.Capabilities.Capabilities[adapter.CapApprovals] = adapter.SupportNo
	sess, err := a.Start(context.Background(), spec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	err = sess.Respond(context.Background(), adapter.PermissionResponse{ID: "x", Allow: true})
	if !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("Respond = %v, want ErrNotSupported", err)
	}
	drain(t, sess)
	if _, err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestSendAfterCloseFails(t *testing.T) {
	a := fake.New()
	sess, err := a.Start(context.Background(), spec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain(t, sess)
	if _, err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := sess.Send(context.Background(), "late"); !errors.Is(err, adapter.ErrSessionClosed) {
		t.Fatalf("Send after close = %v, want ErrSessionClosed", err)
	}
}

func TestInterrupt(t *testing.T) {
	a := fake.New()
	a.Delay = 20 * time.Millisecond
	a.Script = []adapter.Event{
		{Kind: adapter.EventText, Text: "one"},
		{Kind: adapter.EventText, Text: "two"},
		{Kind: adapter.EventText, Text: "three"},
		{Kind: adapter.EventText, Text: "four"},
	}
	sess, err := a.Start(context.Background(), spec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	go func() {
		<-sess.Events() // let one event through, then stop it
		_ = sess.Interrupt(context.Background())
		for range sess.Events() { //nolint:revive // drain whatever is in flight
		}
	}()

	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Status != adapter.StatusInterrupted {
		t.Fatalf("Status = %q, want interrupted", res.Status)
	}
	if got := a.Sessions()[0].Interrupts(); got != 1 {
		t.Errorf("Interrupts = %d, want 1", got)
	}
}

func TestScriptedTerminalEvent(t *testing.T) {
	boom := errors.New("the agent fell over")
	a := fake.New()
	a.Script = []adapter.Event{
		{Kind: adapter.EventText, Text: "trying"},
		{Kind: adapter.EventError, Err: boom},
	}
	sess, err := a.Start(context.Background(), spec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs := drain(t, sess)
	if evs[len(evs)-1].Kind != adapter.EventError {
		t.Fatalf("final event = %q, want error", evs[len(evs)-1].Kind)
	}
	res, err := sess.Wait()
	if !errors.Is(err, boom) {
		t.Fatalf("Wait err = %v, want the scripted error", err)
	}
	// A failed session reports BOTH a non-succeeded status and an error, so a
	// caller inspecting either one learns the truth.
	if res.Status == adapter.StatusSucceeded {
		t.Errorf("Status = %q on a session that errored", res.Status)
	}
}

func TestSpecsRecorded(t *testing.T) {
	a := fake.New()
	s := spec(t)
	s.Model = "claude-opus-5"
	s.AllowedTools = []string{"Read", "Edit"}
	sess, err := a.Start(context.Background(), s)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain(t, sess)
	if _, err := sess.Wait(); err != nil {
		t.Fatal(err)
	}

	specs := a.Specs()
	if len(specs) != 1 {
		t.Fatalf("Specs returned %d, want 1", len(specs))
	}
	if specs[0].Model != "claude-opus-5" || specs[0].SystemPrompt != "you are a runner" {
		t.Errorf("the spec was not recorded faithfully: %+v", specs[0])
	}
	if got := a.Sessions()[0].Spec().RunID; got != "run-1" {
		t.Errorf("Session.Spec RunID = %q", got)
	}
}

func TestSecondFakeUnderAnotherKind(t *testing.T) {
	a := fake.New()
	a.Kind = "fake-two"
	if err := adapter.Register(a); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { adapter.Unregister("fake-two") })

	got, ok := adapter.Lookup("fake-two")
	if !ok {
		t.Fatal("Lookup(fake-two) = false")
	}
	// Changing Kind must carry into the manifest, or Register would have
	// rejected it for disagreeing with Name.
	if got.Manifest().Kind != "fake-two" {
		t.Fatalf("manifest kind = %q", got.Manifest().Kind)
	}
}

func TestFakeSatisfiesEverything(t *testing.T) {
	m := fake.New().Manifest()
	req := make([]string, 0, len(adapter.Capabilities()))
	for _, c := range adapter.Capabilities() {
		req = append(req, string(c))
	}
	if missing := m.Satisfies(req); len(missing) != 0 {
		t.Fatalf("the fake does not declare %v; a test should opt out deliberately, not by omission", missing)
	}
}
