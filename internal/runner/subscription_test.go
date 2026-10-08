package runner_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/fake"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// Subscription headroom and the claim gate (ark:rein#40). The failure these
// pin down: a queue whose subscription window has run out kept claiming runs
// and failing every one of them until the reset.

// failOnRateLimit scripts the agent the way a spent subscription looks from
// the run loop: the session fails, and its reading says the window is out.
func failOnRateLimit(h *harness, rl *adapter.RateLimit) {
	h.agent.Script = []adapter.Event{{Kind: adapter.EventProgress, Text: "starting"}}
	h.agent.Result = adapter.Result{Status: adapter.StatusFailed, Summary: "API Error: 429"}
	h.agent.WaitErr = errors.New("claude: the session failed: usage limit reached")
	h.agent.RateLimit = rl
}

// sessionsBeaten returns the `session` half of every heartbeat Elk received.
func sessionsBeaten(h *harness) []map[string]any {
	var out []map[string]any
	for _, c := range h.elk.CallsTo("heartbeat_executor") {
		if s, ok := c.Args["session"].(map[string]any); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestAnExhaustedWindowStopsClaiming(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	until := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	failOnRateLimit(h, &adapter.RateLimit{
		Status: "rejected", Type: "five_hour", ResetsAt: until,
		Source: adapter.SourceClaudeEvent,
		Windows: map[string]adapter.RateLimitWindow{
			"five_hour": {Utilization: 1, ResetsAt: until},
			"seven_day": {Utilization: 0.71},
		},
		Exhausted: true, ExhaustedUntil: until,
	})

	// The heartbeat says a run is waiting on every poll, and claim_run would
	// hand one over every time. Without the gate this loop claims a run per
	// poll and fails each one.
	err := h.runLoop(runner.Options{PollInterval: 20 * time.Millisecond, TelemetryInterval: time.Hour}, 800*time.Millisecond)
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	if n := len(h.elk.CallsTo("claim_run")); n != 1 {
		t.Fatalf("claim_run called %d times, want exactly 1 — the queue kept claiming on a spent subscription\nlog:\n%s", n, h.log)
	}

	// The one run that found out is reported as what it is, with the time.
	sub := h.submitted()
	if sub.Arg("status") != "stuck" {
		t.Errorf("status = %q, want stuck", sub.Arg("status"))
	}
	d := sub.Arg("deliverable")
	for _, want := range []string{"subscription", "exhausted", until.Format(time.RFC3339), "exhausted_until"} {
		if !strings.Contains(d, want) {
			t.Errorf("the stuck deliverable does not mention %q:\n%s", want, d)
		}
	}

	// And every beat after it says so, so Elk's router can route around it.
	beats := sessionsBeaten(h)
	last := beats[len(beats)-1]
	if got, _ := last["exhausted_until"].(string); got != until.Format(time.RFC3339) {
		t.Errorf("the last beat's exhausted_until = %q, want %s\nsession: %#v", got, until.Format(time.RFC3339), last)
	}
	if reason, _ := last["exhausted_reason"].(string); !strings.Contains(reason, "Claude") {
		t.Errorf("exhausted_reason = %q", reason)
	}
	rl, _ := last["rate_limit"].(map[string]any)
	if rl["exhausted"] != true || rl["source"] != adapter.SourceClaudeEvent || rl["sampled_at"] == "" {
		t.Errorf("rate_limit on an idle beat = %#v, want the carried reading marked exhausted", rl)
	}
	limits, _ := last["limits"].(map[string]any)
	if _, ok := limits["five_hour"]; !ok {
		t.Errorf("the idle beat dropped the windows: %#v", last)
	}
	if !strings.Contains(h.log.String(), "not claiming until "+until.Format(time.RFC3339)) {
		t.Errorf("the log does not say why the queue stopped claiming:\n%s", h.log)
	}
}

func TestClaimingResumesWhenTheHoldEnds(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	until := time.Now().Add(400 * time.Millisecond)
	failOnRateLimit(h, &adapter.RateLimit{
		Status: "rejected", Source: adapter.SourceClaudeEvent,
		Exhausted: true, ExhaustedUntil: until,
	})

	err := h.runLoop(runner.Options{PollInterval: 20 * time.Millisecond, TelemetryInterval: time.Hour}, 1500*time.Millisecond)
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	claims := h.elk.CallsTo("claim_run")
	// One before the hold, one after it. The second run reports the same
	// (now past) reset, which holds only briefly — but long enough to outlast
	// this test, so there is no third.
	if len(claims) != 2 {
		t.Fatalf("claim_run called %d times, want 2 (one each side of the hold)\nlog:\n%s", len(claims), h.log)
	}
	if !strings.Contains(h.log.String(), "hold ended") {
		t.Errorf("the log does not show the queue coming back:\n%s", h.log)
	}
}

func TestAHoldSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	state := t.TempDir()
	until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	failOnRateLimit(h, &adapter.RateLimit{
		Status: "rejected", Source: adapter.SourceClaudeEvent,
		Exhausted: true, ExhaustedUntil: until,
	})
	if err := h.run(runner.Options{StateDir: state}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.elk.CallsTo("claim_run")); n != 1 {
		t.Fatalf("first runner: %d claims, want 1", n)
	}

	// A restarted service — a new runner over the same state directory —
	// must not claim straight back into a subscription it knows is spent.
	if err := h.run(runner.Options{StateDir: state}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.elk.CallsTo("claim_run")); n != 1 {
		t.Fatalf("after a restart the queue claimed again (%d claims total)\nlog:\n%s", n, h.log)
	}
	if strings.Count(h.log.String(), "not claiming until "+until.Format(time.RFC3339)+":") != 1 {
		t.Errorf("the restarted runner does not say it is holding:\n%s", h.log)
	}
	if strings.Contains(h.log.String(), "nothing waiting to claim") {
		t.Errorf("a held --once run claimed to have found nothing waiting:\n%s", h.log)
	}

	// And `rein status` can read it without asking the service.
	s, ok := runner.ReadSubscriptionState(state, queue)
	if !ok || !s.Until.Equal(until) {
		t.Fatalf("ReadSubscriptionState = %+v, %v", s, ok)
	}
	if got := s.Summary(time.Now()); got != "exhausted until "+until.Format(time.RFC3339) {
		t.Errorf("Summary = %q", got)
	}
}

func TestSubscriptionSummaryReadsLikeASentence(t *testing.T) {
	now := time.Date(2026, 9, 29, 18, 0, 0, 0, time.UTC)
	s := runner.SubscriptionState{Last: &adapter.RateLimit{
		SampledAt: now.Add(-12 * time.Minute),
		Windows: map[string]adapter.RateLimitWindow{
			"seven_day": {Utilization: 0.12, ResetsAt: now.Add(72 * time.Hour)},
			// Reset since it was read: says nothing about the window now.
			"five_hour": {Utilization: 0.97, ResetsAt: now.Add(-time.Hour)},
		},
	}}
	if got := s.Summary(now); got != "seven_day 12%, read 12m ago" {
		t.Errorf("Summary = %q", got)
	}
	if got := (runner.SubscriptionState{}).Summary(now); got != "—" {
		t.Errorf("empty Summary = %q", got)
	}
}

func TestNoResetGivenHoldsUntilTheStatedWeeklyReset(t *testing.T) {
	h := newHarness(t)
	h.cfg.Queues[0].WeeklyReset = "mon 09:00 UTC"
	h.elk.Text("claim_run", order("run-1"))
	// Grok's shape: exhausted, and nothing about when.
	failOnRateLimit(h, &adapter.RateLimit{
		Status: "rate_limit", Source: adapter.SourceGrokStopFailure, Exhausted: true,
	})
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	// The --once runner does not beat again after the run, so read the hold
	// from the log and the deliverable.
	now := time.Now().UTC()
	days := (int(time.Monday) - int(now.Weekday()) + 7) % 7
	want := time.Date(now.Year(), now.Month(), now.Day()+days, 9, 0, 0, 0, time.UTC)
	if !want.After(now) {
		want = want.AddDate(0, 0, 7)
	}
	d := h.submitted().Arg("deliverable")
	if !strings.Contains(d, want.Format(time.RFC3339)) || !strings.Contains(d, "weekly reset") {
		t.Errorf("the hold is not the stated weekly reset %s:\n%s", want.Format(time.RFC3339), d)
	}
}

func TestNoResetAndNoneStatedHoldsADay(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	failOnRateLimit(h, &adapter.RateLimit{
		Status: "rate_limit", Source: adapter.SourceGrokStopFailure, Exhausted: true,
	})
	before := time.Now()
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	d := h.submitted().Arg("deliverable")
	if !strings.Contains(d, "Grok") || !strings.Contains(d, "24 hours") {
		t.Errorf("the deliverable does not say it is holding a day on Grok's word:\n%s", d)
	}
	// The time itself: a day from now, to the second.
	i := strings.Index(d, "until ")
	if i < 0 {
		t.Fatalf("no hold time in:\n%s", d)
	}
	stamp := strings.Fields(d[i+len("until "):])[0]
	stamp = strings.TrimRight(stamp, ",.")
	got, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("hold time %q: %v", stamp, err)
	}
	if got.Before(before.Add(runner.ExhaustedFallback).Add(-2*time.Second)) ||
		got.After(time.Now().Add(runner.ExhaustedFallback).Add(2*time.Second)) {
		t.Errorf("held until %s, want 24 hours from now", got)
	}
}

func TestARunWithRoomLeavesTheQueueClaiming(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.RateLimit = &adapter.RateLimit{
		Status: "allowed_warning", Type: "five_hour", Source: adapter.SourceClaudeEvent,
		Windows: map[string]adapter.RateLimitWindow{"five_hour": {Utilization: 0.93, ResetsAt: time.Now().Add(time.Hour)}},
	}
	err := h.runLoop(runner.Options{PollInterval: 20 * time.Millisecond, TelemetryInterval: time.Hour}, 400*time.Millisecond)
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	// 93% is close, not out: the router prefers a queue with more room, but
	// this one still works.
	if n := len(h.elk.CallsTo("claim_run")); n < 2 {
		t.Fatalf("claim_run called %d times; a queue with room stopped claiming\nlog:\n%s", n, h.log)
	}
	for _, s := range sessionsBeaten(h) {
		if _, held := s["exhausted_until"]; held {
			t.Fatalf("a queue with room reported exhausted_until: %#v", s)
		}
	}
}

func TestAnIdleReaderIsReadAndHeldWithoutClaiming(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))

	// A Codex-shaped adapter: it can read its window with no run at all.
	var reads atomic.Int32
	until := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Second)
	reader := fake.NewReader(func(context.Context) (*adapter.RateLimit, error) {
		reads.Add(1)
		return &adapter.RateLimit{
			Status: "rate_limit_reached", Type: "seven_day",
			Source: adapter.SourceCodexRateLimits, SampledAt: time.Now(),
			Windows: map[string]adapter.RateLimitWindow{
				"seven_day": {Utilization: 1, ResetsAt: until, Minutes: 10080},
			},
			Exhausted: true, ExhaustedUntil: until,
		}, nil
	})
	reader.Kind = "codex"
	if err := adapter.Register(reader); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adapter.Unregister("codex") })
	h.cfg.Queues[0].AgentKind = "codex"

	err := h.runLoop(runner.Options{PollInterval: 20 * time.Millisecond, TelemetryInterval: time.Hour}, 400*time.Millisecond)
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if reads.Load() != 1 {
		t.Errorf("the idle queue read its window %d times in 400ms; want once, then not again for %s",
			reads.Load(), runner.HeadroomRefreshAge)
	}
	if n := len(h.elk.CallsTo("claim_run")); n != 0 {
		t.Fatalf("a queue whose window was read as spent claimed %d runs", n)
	}
	beats := sessionsBeaten(h)
	if len(beats) == 0 {
		t.Fatal("no beats")
	}
	// The read happens before the first beat, so even the first says so.
	first := beats[0]
	if first["exhausted_until"] != until.Format(time.RFC3339) {
		t.Errorf("first beat exhausted_until = %v, want %s", first["exhausted_until"], until.Format(time.RFC3339))
	}
	limits, _ := first["limits"].(map[string]any)
	week, _ := limits["seven_day"].(map[string]any)
	if week["utilization"] != 1.0 || week["window_minutes"] != 10080.0 || week["resets_at"] != until.Format(time.RFC3339) {
		t.Errorf("seven_day window on the beat = %#v", week)
	}
}

func TestAReaderIsAskedAgainBeforeClaimingAfterAHold(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))

	// First read: out, until a moment from now. Every read after: room.
	var reads atomic.Int32
	until := time.Now().Add(300 * time.Millisecond)
	reader := fake.NewReader(func(context.Context) (*adapter.RateLimit, error) {
		n := reads.Add(1)
		rl := &adapter.RateLimit{Source: adapter.SourceCodexRateLimits, SampledAt: time.Now()}
		if n == 1 {
			rl.Exhausted, rl.ExhaustedUntil = true, until
		}
		return rl, nil
	})
	reader.Kind = "codex"
	if err := adapter.Register(reader); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adapter.Unregister("codex") })
	h.cfg.Queues[0].AgentKind = "codex"

	err := h.runLoop(runner.Options{PollInterval: 20 * time.Millisecond, TelemetryInterval: time.Hour}, 1200*time.Millisecond)
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if reads.Load() < 2 {
		t.Fatalf("the hold ended without the window being read again (%d reads)\nlog:\n%s", reads.Load(), h.log)
	}
	claims := h.elk.CallsTo("claim_run")
	if len(claims) == 0 {
		t.Fatalf("the queue never claimed after its window came back\nlog:\n%s", h.log)
	}
	if !strings.Contains(h.log.String(), "room again") {
		t.Errorf("the log does not say the window came back:\n%s", h.log)
	}
}

func TestAFailingReadCarriesTheLastReadingAndSaysSoOnce(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", `Queue "`+queue+`" is clear: no runs waiting in "`+space+`".`)
	h.elk.Text("heartbeat_executor", `Heartbeat recorded for queue "`+queue+`". 0 runs waiting.`)
	reader := fake.NewReader(func(context.Context) (*adapter.RateLimit, error) {
		return nil, errors.New("codex: headroom read: initialize: context deadline exceeded")
	})
	reader.Kind = "codex"
	if err := adapter.Register(reader); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adapter.Unregister("codex") })
	h.cfg.Queues[0].AgentKind = "codex"

	err := h.runLoop(runner.Options{
		PollInterval: 10 * time.Millisecond, TelemetryInterval: time.Hour, HeadroomRefresh: 30 * time.Millisecond,
	}, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if reader.Reads() < 2 {
		t.Fatalf("reads = %d; the refresh age was not honoured", reader.Reads())
	}
	if n := strings.Count(h.log.String(), "headroom read failed"); n != 1 {
		t.Errorf("a repeating failure was logged %d times, want once\nlog:\n%s", n, h.log)
	}
}

// keep the elktest import honest if a helper above stops using it.
var _ = elktest.Reply{}
