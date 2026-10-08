package claude

import (
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// Exhaustion (ark:rein#40): the adapter's verdict on a rate_limit_event, which
// is what makes the run loop stop claiming. Only Claude's own `rejected`
// counts, and not when extra usage is paying for the request.

func rateLimitLine(info string) []byte {
	return []byte(`{"type":"rate_limit_event","rate_limit_info":` + info + `,"session_id":"s1"}`)
}

func TestDecodeARejectedWindowIsExhaustedUntilItsReset(t *testing.T) {
	at := time.Date(2026, 9, 29, 23, 0, 0, 0, time.UTC)
	d := &decoder{clock: func() time.Time { return at }}
	if _, _, err := d.decode(rateLimitLine(`{"status":"rejected","resetsAt":1790737200,` +
		`"rateLimitType":"five_hour","overageStatus":"rejected","isUsingOverage":false,` +
		`"unifiedWindows":{"five_hour":{"utilization":1.0,"resetsAt":1790737200},` +
		`"seven_day":{"utilization":0.81,"resetsAt":1791100000}}}`)); err != nil {
		t.Fatal(err)
	}
	rl := d.telemetry().RateLimit
	if rl == nil || !rl.Exhausted {
		t.Fatalf("a rejected five_hour window is not exhausted: %+v", rl)
	}
	if want := time.Unix(1790737200, 0).UTC(); !rl.ExhaustedUntil.Equal(want) {
		t.Errorf("ExhaustedUntil = %s, want the binding window's reset %s", rl.ExhaustedUntil, want)
	}
	if rl.Source != adapter.SourceClaudeEvent || !rl.SampledAt.Equal(at) {
		t.Errorf("source/sampled = %q/%s", rl.Source, rl.SampledAt)
	}
}

func TestDecodeARejectionWithNoTopLevelResetUsesTheFullWindow(t *testing.T) {
	d := &decoder{}
	if _, _, err := d.decode(rateLimitLine(`{"status":"rejected",` +
		`"unifiedWindows":{"five_hour":{"utilization":0.4,"resetsAt":1790700000},` +
		`"seven_day":{"utilization":1.0,"resetsAt":1791100000}}}`)); err != nil {
		t.Fatal(err)
	}
	rl := d.telemetry().RateLimit
	if !rl.Exhausted || !rl.ExhaustedUntil.Equal(time.Unix(1791100000, 0).UTC()) {
		t.Errorf("want exhausted until the full seven_day window resets, got %+v", rl)
	}
}

func TestDecodeRoomOrOverageIsNotExhausted(t *testing.T) {
	for name, info := range map[string]string{
		"allowed":                `{"status":"allowed","resetsAt":1790737200,"rateLimitType":"five_hour"}`,
		"close, but allowed":     `{"status":"allowed_warning","resetsAt":1790737200,"rateLimitType":"five_hour","utilization":0.95}`,
		"rejected, on overage":   `{"status":"rejected","resetsAt":1790737200,"isUsingOverage":true}`,
		"rejected, overageInUse": `{"status":"rejected","resetsAt":1790737200,"overageInUse":true}`,
		"rejected, overage open": `{"status":"rejected","resetsAt":1790737200,"overageStatus":"allowed"}`,
	} {
		d := &decoder{}
		if _, _, err := d.decode(rateLimitLine(info)); err != nil {
			t.Fatal(err)
		}
		if rl := d.telemetry().RateLimit; rl == nil || rl.Exhausted {
			t.Errorf("%s: read as exhausted: %+v", name, rl)
		}
	}
}

func TestDecodeTheOlderUsageLimitResultIsExhausted(t *testing.T) {
	d := &decoder{}
	// An allowed event earlier in the run, then the result that says the limit
	// was hit, the way builds before the rejected event reported it.
	if _, _, err := d.decode(rateLimitLine(`{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.99}}}`)); err != nil {
		t.Fatal(err)
	}
	_, res, err := d.decode([]byte(`{"type":"result","subtype":"success","is_error":true,` +
		`"result":"Claude AI usage limit reached|1790737200","session_id":"s1"}`))
	if err != nil || res == nil || res.Status != adapter.StatusFailed {
		t.Fatalf("result = %+v, %v", res, err)
	}
	rl := d.telemetry().RateLimit
	if rl == nil || !rl.Exhausted || !rl.ExhaustedUntil.Equal(time.Unix(1790737200, 0).UTC()) {
		t.Fatalf("the usage-limit result did not mark the account exhausted: %+v", rl)
	}
	if _, kept := rl.Windows["five_hour"]; !kept {
		t.Error("the windows the earlier event reported were dropped")
	}

	// And an ordinary failure is not a rate limit.
	d2 := &decoder{}
	if _, _, err := d2.decode([]byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"it broke"}`)); err != nil {
		t.Fatal(err)
	}
	if rl := d2.telemetry().RateLimit; rl != nil {
		t.Errorf("an ordinary failure produced a rate-limit reading: %+v", rl)
	}
}

func TestDecodeRecordedAllowedEventsAreNotExhausted(t *testing.T) {
	for _, f := range []string{"hello.jsonl", "tools.jsonl", "multiturn.jsonl", "permission_denied.jsonl"} {
		if rl := replay(t, f).dec.telemetry().RateLimit; rl == nil || rl.Exhausted {
			t.Errorf("%s: recorded allowed windows read as exhausted: %+v", f, rl)
		}
	}
}
