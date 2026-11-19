package tasktrace_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/tasktrace"
)

const testToken = "signal-bearer-for-tests"

// receiver is a Stride receiver that keeps what it was sent.
type receiver struct {
	*httptest.Server
	mu      sync.Mutex
	status  int
	batches []batch
	auth    []string
	paths   []string
}

type batch struct {
	Source struct {
		System      string `json:"system"`
		Service     string `json:"service"`
		Environment string `json:"environment"`
		Version     string `json:"version"`
	} `json:"source"`
	Observations []observation `json:"observations"`
}

type observation struct {
	ID         string         `json:"id"`
	Time       time.Time      `json:"time"`
	Kind       string         `json:"kind"`
	Operation  string         `json:"operation"`
	Status     string         `json:"status"`
	DurationMS *int64         `json:"duration_ms"`
	Context    map[string]any `json:"context"`
	Attributes map[string]any `json:"attributes"`
	Raw        string         `json:"-"`
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusOK}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var b batch
		_ = json.Unmarshal(body, &b)
		for i := range b.Observations {
			b.Observations[i].Raw = string(body)
		}
		r.mu.Lock()
		r.batches = append(r.batches, b)
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.paths = append(r.paths, req.URL.Path)
		status := r.status
		r.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"accepted":1,"dropped":0}`))
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) observations() []observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []observation
	for _, b := range r.batches {
		out = append(out, b.Observations...)
	}
	return out
}

func (r *receiver) requests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.batches)
}

func closeNow(t *testing.T, e *tasktrace.Emitter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Close(ctx)
}

func keys(m map[string]any) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

var testRun = tasktrace.Run{
	TaskID:     "10047293-7724-4598-804e-1cf7158e0872",
	RunID:      "arun-6c757bc5",
	Queue:      "mac-claude",
	AgentKind:  "claude",
	TaskNumber: "#1987",
}

// The span as the contract with Elk's own spans has it (elk-work/scout
// docs/stride.md, "A task's time: one trace per task"): the task's id as the
// trace id, service rein, run_id on the span, `time` at the END with
// duration_ms reaching back, and ids and counts only.
func TestASpanIsFiledInTheTasksTraceWithIdsOnly(t *testing.T) {
	rx := newReceiver(t)
	e := tasktrace.New(tasktrace.Config{
		Endpoint: rx.URL + "/r/elk-stride/", Token: testToken, Version: "v9.9.9",
	})
	if !e.Enabled() {
		t.Fatal("an emitter with an endpoint and a token is off")
	}
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	end := start.Add(1500 * time.Millisecond)
	run := testRun
	run.Round = 2
	e.Span(tasktrace.OpDeliverableSent, run, start, end)
	closeNow(t, e)

	rx.mu.Lock()
	if len(rx.batches) != 1 {
		rx.mu.Unlock()
		t.Fatalf("batches = %d, want 1", len(rx.batches))
	}
	b, auth, path := rx.batches[0], rx.auth[0], rx.paths[0]
	rx.mu.Unlock()

	if path != "/r/elk-stride/v1/observations:batch" {
		t.Errorf("posted to %q", path)
	}
	if auth != "Bearer "+testToken {
		t.Error("the token did not travel as the bearer")
	}
	if b.Source.System != "elk" || b.Source.Service != "rein" {
		t.Errorf("source = %s/%s, want elk/rein", b.Source.System, b.Source.Service)
	}
	if b.Source.Environment != "production" || b.Source.Version != "v9.9.9" {
		t.Errorf("source environment %q version %q", b.Source.Environment, b.Source.Version)
	}
	if len(b.Observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(b.Observations))
	}
	o := b.Observations[0]
	if o.Kind != "operation_completed" || o.Status != "success" {
		t.Errorf("kind %q status %q", o.Kind, o.Status)
	}
	if o.Operation != "elk.task.rein.deliverable_sent" {
		t.Errorf("operation = %q", o.Operation)
	}
	if o.ID == "" {
		t.Error("no observation id: Signal's task view uses it as the span's id")
	}
	if got := o.Context["trace_id"]; got != testRun.TaskID {
		t.Errorf("trace_id = %v, want the task's id", got)
	}
	if got := o.Context["request_id"]; got != testRun.RunID {
		t.Errorf("request_id = %v, want the run id", got)
	}
	if !o.Time.Equal(end) {
		t.Errorf("time = %s, want the span's end %s", o.Time, end)
	}
	if o.DurationMS == nil || *o.DurationMS != 1500 {
		t.Errorf("duration_ms = %v, want 1500", o.DurationMS)
	}
	want := map[string]any{
		"run_id": testRun.RunID, "queue": "mac-claude", "agent_kind": "claude",
		"task_number": "#1987", "round": float64(2),
	}
	if keys(o.Attributes) != keys(want) {
		t.Errorf("attributes = %s, want exactly %s", keys(o.Attributes), keys(want))
	}
	for k, v := range want {
		if o.Attributes[k] != v {
			t.Errorf("attribute %s = %v, want %v", k, o.Attributes[k], v)
		}
	}
}

// task_number travels only when the claim carried one, and round only on a
// span that belongs to one. Absent is absent, never zero or empty.
func TestAbsentFactsAreLeftOffTheSpan(t *testing.T) {
	rx := newReceiver(t)
	e := tasktrace.New(tasktrace.Config{Endpoint: rx.URL, Token: testToken})
	now := time.Now()
	e.Span(tasktrace.OpClaimReceived, tasktrace.Run{TaskID: "task-1", RunID: "run-1"}, now, now)
	closeNow(t, e)

	obs := rx.observations()
	if len(obs) != 1 {
		t.Fatalf("observations = %d, want 1", len(obs))
	}
	if got := keys(obs[0].Attributes); got != "run_id" {
		t.Errorf("attributes = %s, want run_id alone", got)
	}
	if obs[0].DurationMS == nil || *obs[0].DurationMS < 1 {
		t.Errorf("duration_ms = %v: a stretch too short to measure still has one", obs[0].DurationMS)
	}
}

// No endpoint or no token means no attempt: no client, and no request.
func TestUnconfiguredIsOffAndAttemptsNothing(t *testing.T) {
	rx := newReceiver(t)
	for name, cfg := range map[string]tasktrace.Config{
		"nothing":     {},
		"no token":    {Endpoint: rx.URL},
		"no endpoint": {Token: testToken},
		"blank token": {Endpoint: rx.URL, Token: "  "},
	} {
		e := tasktrace.New(cfg)
		if e != nil || e.Enabled() {
			t.Errorf("%s: the emitter is on", name)
		}
		// Every method is safe on it.
		e.Span(tasktrace.OpClaimReceived, testRun, time.Now(), time.Now())
		closeNow(t, e)
	}
	if n := rx.requests(); n != 0 {
		t.Errorf("an unconfigured emitter made %d request(s)", n)
	}
}

// A span with no task or run to file it under is not sent, rather than sent
// into a trace of its own that nothing could find.
func TestASpanWithoutItsIdsIsNotSent(t *testing.T) {
	rx := newReceiver(t)
	e := tasktrace.New(tasktrace.Config{Endpoint: rx.URL, Token: testToken})
	now := time.Now()
	e.Span(tasktrace.OpClaimReceived, tasktrace.Run{RunID: "run-1"}, now, now)
	e.Span(tasktrace.OpClaimReceived, tasktrace.Run{TaskID: "task-1"}, now, now)
	e.Span(tasktrace.OpClaimReceived, tasktrace.Run{TaskID: "not an id\nat all", RunID: "run-1"}, now, now)
	e.Span(tasktrace.OpClaimReceived, tasktrace.Run{TaskID: strings.Repeat("x", 500), RunID: "run-1"}, now, now)
	e.Span(tasktrace.OpClaimReceived, testRun, time.Time{}, now)
	closeNow(t, e)
	if n := len(rx.observations()); n != 0 {
		t.Errorf("%d span(s) were sent without the ids to file them under", n)
	}
}

// Fail open. A receiver that refuses, errors, or is not there at all never
// makes Span wait, and Close gives up inside its deadline.
func TestAReceiverThatCannotTakeSpansNeverHoldsTheCallerUp(t *testing.T) {
	// A port nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()

	// A receiver that accepts the connection and never answers.
	hang := make(chan struct{})
	stuck := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	defer stuck.Close()
	defer close(hang)

	failing := newReceiver(t)
	failing.mu.Lock()
	failing.status = http.StatusInternalServerError
	failing.mu.Unlock()
	refusing := newReceiver(t)
	refusing.mu.Lock()
	refusing.status = http.StatusUnauthorized
	refusing.mu.Unlock()

	for name, endpoint := range map[string]string{
		"nothing listening": dead,
		"never answers":     stuck.URL,
		"answers 500":       failing.URL,
		"answers 401":       refusing.URL,
	} {
		t.Run(name, func(t *testing.T) {
			e := tasktrace.New(tasktrace.Config{Endpoint: endpoint, Token: testToken})
			began := time.Now()
			for i := 0; i < 50; i++ {
				e.Span(tasktrace.OpAgentStarted, testRun, began, time.Now())
			}
			if took := time.Since(began); took > time.Second {
				t.Errorf("50 spans took %s to record: recording a span waited on the receiver", took)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			began = time.Now()
			e.Close(ctx)
			if took := time.Since(began); took > 3*time.Second {
				t.Errorf("Close took %s against a 300ms deadline", took)
			}
		})
	}
}
