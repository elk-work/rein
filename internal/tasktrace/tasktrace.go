// Package tasktrace sends the machine's side of a task's time to Signal, as
// Stride spans in the task's own trace.
//
// Elk keeps every phase of a run in Postgres and sends each one to Signal as a
// span (elk-work/scout docs/stride.md, "A task's time: one trace per task"):
// waiting in line, held, running, in review, and its own side of the claim.
// What Elk cannot see is what happened on the machine between those phases. A
// run Elk counts as started twenty seconds before Rein heard about it looks,
// from Elk alone, like an agent that was slow to begin. These four spans are
// the other half:
//
//	elk.task.rein.claim_received    claim_run asked, until its reply arrived
//	elk.task.rein.worktree_ready    the reply, until the run's folder existed
//	elk.task.rein.agent_started     the folder, until the agent process started
//	elk.task.rein.deliverable_sent  the agent starting, until Elk took the deliverable
//
// Each span is the stretch that ENDS at the moment it is named for, and the
// four lie end to end, so together they cover the machine's time on a run and
// each one's length is the cost of one step. The moment itself is the span's
// `time`: on the wire `time` is when a span ended and `duration_ms` reaches
// back from it, as on Elk's own spans (scout `_shared/task_trace.ts`). The gap
// between the end of Elk's `elk.task.waiting` (the moment Elk recorded the
// claim) and the end of `claim_received` is the handover.
//
// # The contract with Elk's spans
//
//   - The trace id is the task's id: the action id in the claim packet.
//   - Every span carries `run_id`, and travels with the run id as its
//     request id as well. A span has no span id of its own on the wire;
//     Signal's task view falls back to the observation's id, which the SDK
//     mints fresh for each one.
//   - The service is `rein`; Elk's own are filed under `elk-tasks`.
//   - Attributes are ids and counts only: `run_id`, `queue`, `agent_kind`,
//     `round` on a deliverable, and `task_number` when the claim carried one.
//     Never a task's title, a direction or any report text. [Run] has no field
//     that could hold one, which is the guarantee; nothing here filters text
//     out after the fact.
//
// # Fail open
//
// As every Stride emitter does. With no endpoint or no token nothing is built
// and nothing is attempted. With both, a span is a lock-guarded append to the
// SDK's bounded buffer and a background goroutine does the sending, so a
// receiver that is slow, down or refusing never slows or fails a run; what
// cannot be sent is dropped. Every method is safe on a nil *Emitter.
package tasktrace

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/elk-work/stride-sdk/stride"
	"github.com/elk-work/stride-sdk/stridesdk"
)

// The operations, one per moment on the machine.
const (
	OpClaimReceived   = "elk.task.rein.claim_received"
	OpWorktreeReady   = "elk.task.rein.worktree_ready"
	OpAgentStarted    = "elk.task.rein.agent_started"
	OpDeliverableSent = "elk.task.rein.deliverable_sent"
)

const (
	// System and Service are the Stride source every span is filed under.
	System  = "elk"
	Service = "rein"

	// DefaultEnvironment is the source environment when none is configured.
	// A Rein serving a queue is doing real work on real tasks.
	DefaultEnvironment = "production"

	// DefaultFlushInterval is how long a span may wait in the buffer before
	// it is sent. It is longer than the SDK's five seconds on purpose.
	// Signal's task view takes another service's span only once it has seen
	// one of Elk's own for the same trace (elk-work/signal
	// transforms/0340_stg_stride_task_spans.sql, "What it can miss"), and
	// Elk sends its first, the waiting span, just after it answers the claim.
	// A claim-received span sent the instant the reply landed could reach
	// Signal first and be left out for good. Nothing reads these spans live,
	// so the wait costs nothing. [Emitter.Close] sends at once regardless.
	DefaultFlushInterval = 20 * time.Second

	// maxID bounds an id taken from a claim packet. The task's id is read
	// out of Elk's reply text, so it is checked before it is used as a
	// trace id rather than trusted to be one.
	maxID = 128
)

// Config is what an [Emitter] needs. The zero value is a valid "off".
type Config struct {
	// Endpoint is the Signal receiver's base for the Stride Signal; the SDK
	// posts to {Endpoint}/v1/observations:batch. Empty means off.
	Endpoint string
	// Token is the Signal's bearer token. Empty means off.
	Token string
	// Environment is the source environment; empty means [DefaultEnvironment].
	Environment string
	// Version is Rein's version, as the source version.
	Version string

	// HTTPClient overrides the SDK's transport, and FlushInterval overrides
	// [DefaultFlushInterval]. Tests use them.
	HTTPClient    *http.Client
	FlushInterval time.Duration
}

// Emitter sends task-time spans. Build one with [New].
type Emitter struct {
	client *stridesdk.Client
}

// New builds an emitter. With no endpoint or no token it returns nil, which is
// a working no-op: no client, no goroutine, no request.
func New(cfg Config) *Emitter {
	endpoint, token := strings.TrimSpace(cfg.Endpoint), strings.TrimSpace(cfg.Token)
	if endpoint == "" || token == "" {
		return nil
	}
	env := strings.TrimSpace(cfg.Environment)
	if env == "" {
		env = DefaultEnvironment
	}
	flush := cfg.FlushInterval
	if flush <= 0 {
		flush = DefaultFlushInterval
	}
	return &Emitter{client: stridesdk.New(stridesdk.Config{
		Endpoint: endpoint,
		Token:    token,
		Enabled:  true,
		Source: stride.SourceRef{
			System:      System,
			Service:     Service,
			Environment: env,
			Version:     cfg.Version,
		},
		HTTPClient:    cfg.HTTPClient,
		FlushInterval: flush,
	})}
}

// Enabled reports whether spans are being sent anywhere.
func (e *Emitter) Enabled() bool { return e != nil && e.client.Enabled() }

// Run identifies the run a span belongs to. Ids only, by construction.
type Run struct {
	// TaskID is the task's id, the action id from the claim: the trace id.
	TaskID string
	// RunID is Elk's run id.
	RunID string
	// Queue and AgentKind are the queue that claimed the run and the kind of
	// agent it drives.
	Queue     string
	AgentKind string
	// TaskNumber is the task's number as "#1984", or "" when the claim did
	// not carry one.
	TaskNumber string
	// Round is the review round a deliverable belongs to: 1 for the first,
	// 2 or more after Elk sent the run back. Zero is left out.
	Round int
}

// Span records one stretch of a run: op, from start to end. It returns at
// once and never fails; a run with no task id or no run id, or a stretch with
// no start or no end, is not recorded at all rather than recorded wrongly.
func (e *Emitter) Span(op string, run Run, start, end time.Time) {
	if !e.Enabled() || op == "" || start.IsZero() || end.IsZero() {
		return
	}
	if !usableID(run.TaskID) || !usableID(run.RunID) {
		return
	}
	// The SDK does not panic into its caller. This is the same promise made
	// a second time, because the caller here is a run.
	defer func() { _ = recover() }()

	d := end.Sub(start)
	if d < time.Millisecond {
		// The SDK leaves duration_ms off a span with no length. A stretch
		// that took no measurable time still happened.
		d = time.Millisecond
	}
	attrs := map[string]any{"run_id": run.RunID}
	if run.Queue != "" {
		attrs["queue"] = run.Queue
	}
	if run.AgentKind != "" {
		attrs["agent_kind"] = run.AgentKind
	}
	if run.TaskNumber != "" {
		attrs["task_number"] = run.TaskNumber
	}
	if run.Round > 0 {
		attrs["round"] = run.Round
	}
	ctx := stridesdk.ContextWithTrace(context.Background(), run.TaskID, run.RunID)
	e.client.Event(ctx, stride.KindOperationCompleted, stridesdk.Fields{
		Operation:  op,
		Status:     stride.StatusSuccess,
		Time:       end,
		Duration:   d,
		Attributes: attrs,
	})
}

// Close sends what is still buffered, bounded by ctx, and stops the sender.
// A Rein that exits right after a deliverable would otherwise lose that span.
func (e *Emitter) Close(ctx context.Context) {
	if e == nil {
		return
	}
	defer func() { _ = recover() }()
	_ = e.client.Close(ctx)
}

// usableID reports whether s can stand as an id on the wire: present, short,
// and one unbroken token.
func usableID(s string) bool {
	if s == "" || len(s) > maxID {
		return false
	}
	return !strings.ContainsAny(s, " \t\r\n")
}
