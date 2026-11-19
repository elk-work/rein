package runner_test

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/runner"
	"github.com/elk-work/rein/internal/tasktrace"
)

const strideToken = "signal-bearer-for-tests"

// span is one observation as Signal's receiver would read it.
type span struct {
	Time       time.Time      `json:"time"`
	Kind       string         `json:"kind"`
	Operation  string         `json:"operation"`
	DurationMS int64          `json:"duration_ms"`
	Context    map[string]any `json:"context"`
	Attributes map[string]any `json:"attributes"`
}

// signal is a Stride receiver that keeps the spans it is sent and every body
// as it arrived.
type signal struct {
	*httptest.Server
	mu     sync.Mutex
	spans  []span
	bodies []string
}

func newSignal(t *testing.T) *signal {
	t.Helper()
	s := &signal{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b struct {
			Observations []span `json:"observations"`
		}
		_ = json.Unmarshal(body, &b)
		s.mu.Lock()
		s.spans = append(s.spans, b.Observations...)
		s.bodies = append(s.bodies, string(body))
		s.mu.Unlock()
		_, _ = w.Write([]byte(`{"accepted":1,"dropped":0}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *signal) got() ([]span, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]span(nil), s.spans...), strings.Join(s.bodies, "\n")
}

func operations(spans []span) string {
	var ops []string
	for _, sp := range spans {
		ops = append(ops, strings.TrimPrefix(sp.Operation, "elk.task.rein."))
	}
	return strings.Join(ops, ",")
}

// A run Rein claims, starts and hands in sends its four moments to Signal, in
// the task's own trace, end to end, and carrying ids only.
func TestAClaimedRunSendsItsFourMomentsInTheTasksTrace(t *testing.T) {
	h := newHarness(t)
	sig := newSignal(t)
	h.elk.Reply("claim_run", elktest.Reply{Text: order("run-1"), Structured: map[string]any{"task_number": 1987}})

	before := time.Now()
	err := h.run(runner.Options{TaskTrace: tasktrace.New(tasktrace.Config{Endpoint: sig.URL, Token: strideToken})})
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	h.submitted()

	// The loop sends what is waiting before it returns, so nothing here polls.
	spans, bodies := sig.got()
	if got := operations(spans); got != "claim_received,worktree_ready,agent_started,deliverable_sent" {
		t.Fatalf("spans = %s\nlog:\n%s", got, h.log)
	}
	var lastEnd time.Time
	for _, sp := range spans {
		if sp.Kind != "operation_completed" {
			t.Errorf("%s: kind = %q", sp.Operation, sp.Kind)
		}
		if got := sp.Context["trace_id"]; got != "act-1" {
			t.Errorf("%s: trace_id = %v, want the task's id from the claim", sp.Operation, got)
		}
		for k, want := range map[string]any{
			"run_id": "run-1", "queue": queue, "agent_kind": kind, "task_number": "#1987",
		} {
			if sp.Attributes[k] != want {
				t.Errorf("%s: %s = %v, want %v", sp.Operation, k, sp.Attributes[k], want)
			}
		}
		for k := range sp.Attributes {
			switch k {
			case "run_id", "queue", "agent_kind", "task_number", "round":
			default:
				t.Errorf("%s carries %q, which is not one of the ids and counts a span may carry", sp.Operation, k)
			}
		}
		if sp.Time.Before(before) || sp.Time.Before(lastEnd) {
			t.Errorf("%s ended at %s, before the span ahead of it", sp.Operation, sp.Time)
		}
		// Each span starts where the one before ended: together they are the
		// machine's whole time on the run, with nothing counted twice.
		start := sp.Time.Add(-time.Duration(sp.DurationMS) * time.Millisecond)
		if !lastEnd.IsZero() && (start.Before(lastEnd.Add(-2*time.Millisecond)) || start.After(lastEnd.Add(2*time.Millisecond))) {
			t.Errorf("%s starts at %s, not where the last span ended (%s)", sp.Operation, start, lastEnd)
		}
		lastEnd = sp.Time
	}
	if _, has := spans[0].Attributes["round"]; has {
		t.Error("the claim carries a review round; it belongs to none")
	}
	if spans[3].Attributes["round"] != float64(1) {
		t.Errorf("the deliverable's round = %v, want 1", spans[3].Attributes["round"])
	}

	// Ids and counts only: nothing of the work order, and nothing the agent
	// wrote, is anywhere in what was sent.
	for _, private := range []string{"Port the due-date sheet", "Direction", "Windows", "Starting"} {
		if strings.Contains(bodies, private) {
			t.Errorf("what was sent to Signal contains %q", private)
		}
	}
}

// A run stopped before a worktree exists has a claim and a deliverable, and
// sends no span for the moments it never reached.
func TestARunRefusedAtPreflightSendsOnlyTheMomentsItReached(t *testing.T) {
	h := newHarness(t)
	sig := newSignal(t)
	h.elk.Text("claim_run", order("run-1", "elk-connector", "xcode"))

	err := h.run(runner.Options{TaskTrace: tasktrace.New(tasktrace.Config{Endpoint: sig.URL, Token: strideToken})})
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if got := h.submitted().Arg("status"); got != "stuck" {
		t.Fatalf("status = %q, want stuck", got)
	}
	spans, _ := sig.got()
	if got := operations(spans); got != "claim_received,deliverable_sent" {
		t.Errorf("spans = %s, want the claim and the deliverable alone", got)
	}
}

// Fail open: a Signal that is not there changes nothing about the run.
func TestARunIsUntouchedWhenSignalCannotBeReached(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()

	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	began := time.Now()
	err = h.run(runner.Options{TaskTrace: tasktrace.New(tasktrace.Config{Endpoint: dead, Token: strideToken})})
	if err != nil {
		t.Fatalf("a run failed because Signal was unreachable: %v\nlog:\n%s", err, h.log)
	}
	if got := h.submitted().Arg("status"); got != "ready" {
		t.Errorf("status = %q, want ready", got)
	}
	if took := time.Since(began); took > 15*time.Second {
		t.Errorf("the run took %s with Signal unreachable", took)
	}
}

// A packet from an Elk that names no task has no trace to join, so nothing is
// sent for it and the run goes on as ever.
func TestARunWithNoTaskIdSendsNothing(t *testing.T) {
	h := newHarness(t)
	sig := newSignal(t)
	h.elk.Text("claim_run", strings.Replace(order("run-1"), "**Action id:** `act-1`\n", "", 1))

	err := h.run(runner.Options{TaskTrace: tasktrace.New(tasktrace.Config{Endpoint: sig.URL, Token: strideToken})})
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	h.submitted()
	if spans, _ := sig.got(); len(spans) != 0 {
		t.Errorf("%d span(s) sent for a run with no task id", len(spans))
	}
}

type itemsFunc func(service, account string) (string, error)

func (f itemsFunc) ReadItem(service, account string) (string, error) { return f(service, account) }
func (itemsFunc) Name() string                                       { return "test" }

func TestResolveTaskTrace(t *testing.T) {
	const url = "https://signal.example/r/elk-stride"
	items := itemsFunc(func(service, account string) (string, error) {
		if service == "stride-elk-token" && account == "rein" {
			return " " + strideToken + "\n", nil
		}
		return "", keyring.ErrItemNotFound
	})
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	cases := []struct {
		name   string
		cfg    config.Stride
		items  keyring.ItemReader
		env    map[string]string
		wantOn bool
		say    string
	}{
		{name: "nothing configured", say: "no stride.ingest_url"},
		{name: "url and no token", cfg: config.Stride{IngestURL: url}, items: items, say: "no stride.token"},
		{name: "config and keychain", cfg: config.Stride{IngestURL: url, Token: "stride-elk-token"}, items: items, wantOn: true},
		{name: "item missing", cfg: config.Stride{IngestURL: url, Token: "stride-elk-token/other"}, items: items, say: "no keychain item"},
		{name: "keychain unreadable", cfg: config.Stride{IngestURL: url, Token: "stride-elk-token"},
			items: itemsFunc(func(string, string) (string, error) { return "", errors.New("boom " + strideToken) }),
			say:   "could not be read"},
		{name: "no keychain reader", cfg: config.Stride{IngestURL: url, Token: "stride-elk-token"}, say: "no keychain reader"},
		{name: "environment alone", env: map[string]string{"STRIDE_INGEST_URL": url, "STRIDE_TOKEN": strideToken}, wantOn: true},
		{name: "disabled wins", cfg: config.Stride{IngestURL: url, Token: "stride-elk-token", Disabled: true}, items: items,
			env: map[string]string{"STRIDE_INGEST_URL": url, "STRIDE_TOKEN": strideToken}, say: "stride.disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := runner.ResolveTaskTrace(tc.cfg, tc.items, env(tc.env), "v1.2.3")
			on := got.Endpoint != "" && got.Token != ""
			if on != tc.wantOn {
				t.Errorf("on = %t, want %t (%s)", on, tc.wantOn, why)
			}
			if tc.wantOn {
				if got.Endpoint != url || got.Token != strideToken || got.Version != "v1.2.3" {
					t.Errorf("resolved endpoint %q version %q, or the wrong token", got.Endpoint, got.Version)
				}
			} else if (got != tasktrace.Config{}) {
				t.Error("an off answer still carries settings")
			}
			if !strings.Contains(why, tc.say) {
				t.Errorf("said %q, want it to mention %q", why, tc.say)
			}
			// The line is logged at start-up: it names neither.
			if strings.Contains(why, strideToken) || strings.Contains(why, "signal.example") {
				t.Errorf("the start-up line gives away the token or the URL: %q", why)
			}
		})
	}
}

// The file-backed keychain a headless machine uses is read the same way.
func TestResolveTaskTraceReadsTheFileKeychain(t *testing.T) {
	path := filepath.Join(t.TempDir(), keyring.SecretsFileName)
	items := keyring.NewFileItems(path)
	if err := items.WriteItem("stride-elk-token", "rein", strideToken); err != nil {
		t.Fatal(err)
	}
	got, why := runner.ResolveTaskTrace(config.Stride{IngestURL: "https://signal.example/r/elk-stride", Token: "stride-elk-token"},
		items, nil, "")
	if got.Token != strideToken {
		t.Errorf("the token was not read from the file keychain: %s", why)
	}
}

// A run Elk's review sends back starts the agent again and hands in again.
// Both are marked a second time, with the round they belong to, and the
// claim and the worktree are not.
func TestASentBackRunMarksItsSecondRound(t *testing.T) {
	h := reviewHarness(t, askWaiting, askRevisions)
	sig := newSignal(t)
	opts := reviewOptions()
	opts.TaskTrace = tasktrace.New(tasktrace.Config{Endpoint: sig.URL, Token: strideToken})

	if err := h.run(opts); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spans, _ := sig.got()
	want := "claim_received,worktree_ready,agent_started,deliverable_sent,agent_started,deliverable_sent"
	if got := operations(spans); got != want {
		t.Fatalf("spans = %s\nwant    %s\nlog:\n%s", got, want, h.log)
	}
	for i, round := range map[int]float64{2: 1, 3: 1, 4: 2, 5: 2} {
		if spans[i].Attributes["round"] != round {
			t.Errorf("%s (span %d): round = %v, want %v", spans[i].Operation, i, spans[i].Attributes["round"], round)
		}
	}
	// The second round is measured from when the agent was sent back in, not
	// from the first deliverable: the wait on Elk's review is Elk's time.
	gap := spans[4].Time.Add(-time.Duration(spans[4].DurationMS) * time.Millisecond).Sub(spans[3].Time)
	if gap < -2*time.Millisecond {
		t.Errorf("round 2 starts %s before round 1's deliverable was sent", -gap)
	}
}
