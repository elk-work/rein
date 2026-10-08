package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// recordedRateLimitsRead is the `account/rateLimits/read` result codex-cli
// 0.159.0 returned for a ChatGPT Pro login on 2026-09-29, with the account id
// removed. One weekly window, no five-hour window — which is what the spec
// says a Pro plan has (scout docs/specs/pace-dispatch.md §7.5).
const recordedRateLimitsRead = `{
 "ordinaryUsageAllowed": true,
 "rateLimits": {
  "limitId": "codex", "limitName": null, "normalModelSlug": null,
  "primary": {"usedPercent": 12, "windowDurationMins": 10080, "resetsAt": 1791067737},
  "secondary": null,
  "credits": {"hasCredits": false, "unlimited": false, "balance": "0"},
  "individualLimit": null, "spendControlReached": false, "planType": "pro",
  "rateLimitReachedType": null
 },
 "rateLimitsByLimitId": {"codex": {"limitId": "codex", "primary": {"usedPercent": 12, "windowDurationMins": 10080, "resetsAt": 1791067737}}},
 "rateLimitResetCredits": {"availableCount": 4, "credits": null},
 "rateLimitUpsell": null
}`

func i64(v int64) *int64 { return &v }
func yes() *bool         { b := true; return &b }
func no() *bool          { b := false; return &b }

func TestReadHeadroomAsksTheAppServerAndSpendsNoTurn(t *testing.T) {
	at := time.Date(2026, 9, 29, 23, 40, 0, 0, time.UTC)
	a := New()
	a.clock = func() time.Time { return at }
	a.probeExitGrace = 10 * time.Millisecond
	var p *peer
	a.spawnProbe = func(ctx context.Context) (*process, error) {
		var proc *process
		p, proc = newPeer(t, []opStep{
			{kind: opReply, method: methodInitialize,
				result: json.RawMessage(`{"codexHome":"/tmp/x","platformFamily":"unix","platformOs":"macos","userAgent":"rein/0"}`)},
			{kind: opNotify, method: "account/updated", params: json.RawMessage(`{"authMode":"chatgpt","planType":"pro"}`)},
			{kind: opReply, method: methodRateLimitsRead, result: json.RawMessage(recordedRateLimitsRead)},
		})
		return proc, nil
	}

	rl, err := a.ReadHeadroom(context.Background())
	if err != nil {
		t.Fatalf("ReadHeadroom: %v", err)
	}
	if rl.Exhausted {
		t.Errorf("12%% of the week used read as exhausted: %+v", rl)
	}
	w, ok := rl.Windows["seven_day"]
	if !ok {
		t.Fatalf("the 10080-minute window was not reported as seven_day: %+v", rl.Windows)
	}
	if w.Utilization != 0.12 || w.Minutes != 10080 || !w.ResetsAt.Equal(time.Unix(1791067737, 0).UTC()) {
		t.Errorf("seven_day = %+v", w)
	}
	if _, five := rl.Windows["five_hour"]; five {
		t.Error("a five_hour window was invented; Pro has none")
	}
	if rl.Source != adapter.SourceCodexRateLimits || !rl.SampledAt.Equal(at) {
		t.Errorf("source/sampled = %q/%s", rl.Source, rl.SampledAt)
	}

	// A background poll skips the reset-credit detail lookup, and nothing in
	// a headroom read may start a thread — that would be a turn.
	call, ok := p.clientCall(methodRateLimitsRead)
	if !ok {
		t.Fatal("account/rateLimits/read was never sent")
	}
	if !strings.Contains(string(call.Params), `"excludeResetCreditDetails":true`) {
		t.Errorf("params = %s", call.Params)
	}
	for _, m := range []string{methodThreadStart, methodThreadResume, methodTurnStart} {
		if _, sent := p.clientCall(m); sent {
			t.Errorf("a headroom read sent %s", m)
		}
	}
}

func TestReadHeadroomReportsAnAppServerError(t *testing.T) {
	a := New()
	a.probeExitGrace = 10 * time.Millisecond
	a.spawnProbe = func(ctx context.Context) (*process, error) {
		_, proc := newPeer(t, []opStep{
			{kind: opReply, method: methodInitialize, result: json.RawMessage(`{}`)},
			{kind: opReply, method: methodRateLimitsRead,
				rpcErr: json.RawMessage(`{"code":-32000,"message":"not logged in with ChatGPT"}`)},
		})
		return proc, nil
	}
	if _, err := a.ReadHeadroom(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "not logged in with ChatGPT") {
		t.Fatalf("err = %v; want the app server's own reason", err)
	}
}

func TestToRateLimitVerdicts(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	week := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	five := time.Date(2026, 9, 29, 14, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name      string
		snap      rateLimitSnapshot
		allowed   *bool
		exhausted bool
		until     time.Time
		windows   []string
	}{
		{
			name:    "room in both windows",
			snap:    rateLimitSnapshot{Primary: &rateLimitWindow{UsedPercent: 40, WindowDurationMins: i64(300), ResetsAt: i64(five.Unix())}, Secondary: &rateLimitWindow{UsedPercent: 70, WindowDurationMins: i64(10080), ResetsAt: i64(week.Unix())}},
			allowed: yes(), windows: []string{"five_hour", "seven_day"},
		},
		{
			name:      "the week is full and nothing pays past it",
			snap:      rateLimitSnapshot{Primary: &rateLimitWindow{UsedPercent: 100, WindowDurationMins: i64(10080), ResetsAt: i64(week.Unix())}, Credits: &creditsSnapshot{}},
			exhausted: true, until: week, windows: []string{"seven_day"},
		},
		{
			name:    "full, but credits carry it",
			snap:    rateLimitSnapshot{Primary: &rateLimitWindow{UsedPercent: 100, WindowDurationMins: i64(10080), ResetsAt: i64(week.Unix())}, Credits: &creditsSnapshot{HasCredits: true}},
			windows: []string{"seven_day"},
		},
		{
			name:      "both full: held until the later reset",
			snap:      rateLimitSnapshot{Primary: &rateLimitWindow{UsedPercent: 100, WindowDurationMins: i64(300), ResetsAt: i64(five.Unix())}, Secondary: &rateLimitWindow{UsedPercent: 100, WindowDurationMins: i64(10080), ResetsAt: i64(week.Unix())}},
			exhausted: true, until: week, windows: []string{"five_hour", "seven_day"},
		},
		{
			name:      "the backend says no, with no window to explain it",
			snap:      rateLimitSnapshot{Primary: &rateLimitWindow{UsedPercent: 30, WindowDurationMins: i64(10080), ResetsAt: i64(week.Unix())}},
			allowed:   no(),
			exhausted: true, windows: []string{"seven_day"},
		},
		{
			name:      "a limit reached type",
			snap:      rateLimitSnapshot{RateLimitReachedType: "workspace_member_usage_limit_reached"},
			exhausted: true,
		},
		{
			name:    "a window of an unfamiliar length keeps its slot name",
			snap:    rateLimitSnapshot{Primary: &rateLimitWindow{UsedPercent: 5, WindowDurationMins: i64(1440)}},
			windows: []string{"primary"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rl := toRateLimit(tc.snap, tc.allowed, now)
			if rl.Exhausted != tc.exhausted {
				t.Errorf("Exhausted = %v, want %v (%+v)", rl.Exhausted, tc.exhausted, rl)
			}
			if !rl.ExhaustedUntil.Equal(tc.until) {
				t.Errorf("ExhaustedUntil = %s, want %s", rl.ExhaustedUntil, tc.until)
			}
			if len(rl.Windows) != len(tc.windows) {
				t.Errorf("windows = %v, want %v", rl.Windows, tc.windows)
			}
			for _, name := range tc.windows {
				if _, ok := rl.Windows[name]; !ok {
					t.Errorf("window %q missing from %v", name, rl.Windows)
				}
			}
		})
	}
}

func TestASessionReportsTheWindowCodexSendsMidRun(t *testing.T) {
	a, _ := replayAdapter(t, "pong.jsonl")
	sess, err := a.Start(context.Background(), baseSpec(adapter.PermissionFull))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, sess)
	tel, ok := sess.(adapter.Telemeter)
	if !ok {
		t.Fatal("a Codex session does not implement adapter.Telemeter")
	}
	rl := tel.Telemetry().RateLimit
	if rl == nil {
		t.Fatal("the recorded account/rateLimits/updated did not reach the session's telemetry")
	}
	w, ok := rl.Windows["seven_day"]
	if !ok || w.Utilization != 0 || !w.ResetsAt.Equal(time.Unix(1788647597, 0).UTC()) || w.Minutes != 10080 {
		t.Errorf("seven_day from the recording = %+v (windows %v)", w, rl.Windows)
	}
	if rl.Exhausted {
		t.Error("an empty window read as exhausted")
	}
}
