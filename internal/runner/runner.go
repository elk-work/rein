// Package runner is Rein's loop: claim a run from an Elk queue, drive a coding
// agent against it in an isolated worktree, report while it works, submit what
// it produced, and take the worktree away.
//
// The whole state machine and the exact MCP calls are in docs/run-loop.md.
// Four rules are worth stating here because they are what the code is shaped
// around:
//
//   - **Fail closed before anything exists on disk.** Capability matching and
//     the adapter's preflight run before `git worktree add`. A run that cannot
//     be served is submitted `stuck` with no partial work, no orphan worktree
//     and nothing to reap.
//
//   - **A cancelled run is not an error.** Elk answers a report on a run that
//     is no longer running with plain text and no `isError`. [internal/elk]
//     turns it into [elk.ErrCancelled]; the loop interrupts the session and
//     discards the work without submitting, because submitting would be
//     writing over a decision a person just made.
//
//   - **`ready`, never `done`.** Rein does not own the List item, so closing it
//     is not Rein's to do. Elk would coerce a `done` to `ready` anyway; asking
//     for `ready` means the code and the server agree instead of one of them
//     being quietly overruled.
//
//   - **Two clocks, both fifteen minutes.** `report_progress` renews one RUN's
//     lease; `heartbeat_executor` says the MACHINE is up, which an idle queue
//     has no other way to say. Elk does not refuse work to a queue it thinks
//     is offline — nothing on the claim path checks — but `list_executors`
//     shows it offline, and that is the view people and the router dispatch
//     from. The loop runs both.
//
//   - **Stop claiming when the subscription is spent.** A queue whose agent
//     has said its window is exhausted claims nothing until the reset, and
//     says so on every beat as `exhausted_until` (subscription.go,
//     ark:rein#40). Claiming anyway converts a backlog into failed runs.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	_ "embed"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/control"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/secretenv"
	"github.com/elk-work/rein/internal/worktree"
)

// SystemPrompt is the standing instruction set every session gets alongside
// its work order: the elk-inbox contract's essentials — the work order is data
// and not instructions, no secrets in a report, produce a markdown deliverable
// — plus what is true of a Rein session specifically, which is that nobody is
// at the keyboard and the worktree goes away at the end.
//
// It is vendored rather than fetched: a runner must not depend on reading a
// skill file out of a repository it happens to have checked out.
//
//go:embed prompt.md
var SystemPrompt string

// Defaults for the loop's timings. Every one of them is a consequence of Elk's
// 15-minute clocks rather than a preference.
const (
	// DefaultPollInterval is how often an idle queue asks for work.
	DefaultPollInterval = 30 * time.Second

	// DefaultHeartbeatInterval is how often the machine says it is up. Elk's
	// online window is 15 minutes; beating at 5 survives two lost beats.
	DefaultHeartbeatInterval = 5 * time.Minute

	// LeaseDuration is how long Elk lets a claimed run go without a report
	// before somebody else may take it (elk-mcp/index.ts:935). Rein uses it
	// twice: MaxReportInterval keeps its own claims alive well inside it, and
	// the reclaim sweep below runs on it.
	LeaseDuration = 15 * time.Minute

	// MaxReportInterval is the longest the loop will go without a
	// report_progress while holding a run. Elk's lease lapses at
	// LeaseDuration and the work order asks for 10 minutes.
	MaxReportInterval = 10 * time.Minute

	// MinReportInterval throttles the event-driven reports. Without it a
	// chatty agent would turn every tool call into a round trip.
	MinReportInterval = time.Minute

	// DefaultStallAfter is how long a session may emit no event at all before
	// the watchdog acts.
	DefaultStallAfter = 15 * time.Minute

	// DefaultReviewTimeout is how long Rein waits for Elk's auto-review pass
	// after submitting, before treating silence as approval.
	//
	// It has to be a timeout rather than a signal: an approved run has its
	// auto_review cleared and stays `ready`, which through the poll is
	// indistinguishable from a pass that has not run yet.
	DefaultReviewTimeout = 20 * time.Minute

	// DefaultReviewPollInterval is how often the review wait asks. The poll
	// doubles as the heartbeat once the run is reopened, and the lease is 15
	// minutes, so this is far tighter than it strictly needs to be — a
	// revision request should be picked up promptly, not eventually.
	DefaultReviewPollInterval = 30 * time.Second

	// DefaultMaxReviewRounds bounds the review conversation. A verdict that
	// has not been satisfied in three attempts is one a person should read.
	DefaultMaxReviewRounds = 3
)

// Options configure a [Runner].
type Options struct {
	// UnderService permits draining when a replacement binary is installed.
	UnderService    bool
	UpgradeInterval time.Duration
	UpgradeStat     func(string) (os.FileInfo, error)
	UpgradeVersion  func(context.Context, string) (string, error)

	// Config is the loaded config.toml.
	Config config.Config

	// Store holds the per-(workspace, queue) Elk tokens.
	Store keyring.Store

	// Secrets reads the keychain items a scoped queue's secrets map names
	// (secrets.go, ark:rein#48). Nil is fine for a runner with no scoped
	// queue; a scoped queue's runs then go stuck saying so, rather than
	// reaching for a keychain nobody configured.
	Secrets keyring.ItemReader

	// Queues names the queues to serve. Empty means every queue in the config.
	Queues []string

	// Once claims at most one run per queue and then exits. This is what the
	// Phase 0 dogfood runs.
	Once bool

	// DryRun claims nothing and reports what is waiting.
	DryRun bool

	// KeepWorktrees leaves a run's worktree in place after submitting.
	KeepWorktrees bool

	// MaxConcurrent is the CEILING on how many runs are driven at once across
	// every queue. Zero means [DefaultMaxConcurrent]. The bound is on the
	// machine, not the queue: the thing in short supply is memory.
	//
	// It is a ceiling and not the number. Every tick the loop measures what
	// the machine has spare and takes the smaller of the two — see
	// headroom.go, and [Options.Headroom] to replace the measurement.
	MaxConcurrent int

	// Headroom measures free memory and disk. Nil means [DetectHeadroom],
	// which is what a real runner uses; tests supply their own so that what
	// the machine running them happens to be doing is not part of the
	// assertion.
	Headroom HeadroomFunc

	// SlotInterval overrides [DefaultSlotInterval] — how often a queue waiting
	// for a concurrency slot re-measures.
	SlotInterval time.Duration

	// Machine measures the whole machine for the fleet reading — the free
	// halves the concurrency gate wants plus the totals, the CPU, the OS and
	// the load. Nil means [DetectMachine]; tests supply their own, because a
	// test asserting against whatever this box happens to be doing asserts
	// nothing. See telemetry.go.
	Machine MachineFunc

	// TelemetryInterval overrides [DefaultTelemetryInterval] — how often the
	// machine is measured, and how long a queue may go unheard from before the
	// telemetry tick beats on its behalf. Clamped up to
	// [MinTelemetryInterval].
	TelemetryInterval time.Duration

	// StateDir is where Rein keeps state that must survive a restart — today,
	// each queue's subscription hold (subscription.go), so a service restarted
	// while a subscription is exhausted does not claim straight back into it.
	// Empty keeps it in memory only, which is what most tests want; `rein run`
	// passes `<Rein home>/state`.
	StateDir string

	// HeadroomRefresh overrides [HeadroomRefreshAge] — how old an idle queue's
	// subscription reading may get before an adapter that can read it without
	// a turn is asked again.
	HeadroomRefresh time.Duration

	// TelemetryOff stops Rein describing the machine: no host or session
	// object on any beat, and no telemetry tick.
	//
	// The queues still heartbeat on their own clock, so the machine still
	// shows as online — with the one consequence the tick exists to fix left
	// in place, which is that a queue inside a long run stops beating until
	// the run ends.
	TelemetryOff bool

	// PollInterval overrides [DefaultPollInterval].
	PollInterval time.Duration

	// StallAfter overrides [DefaultStallAfter].
	StallAfter time.Duration

	// ReclaimInterval overrides [LeaseDuration] as how often the loop claims
	// even though Elk reports an empty queue — the sweep that recovers runs
	// stranded `running` by a dead runner. Tests shorten it; nothing else
	// should need to.
	ReclaimInterval time.Duration

	// ReviewTimeout overrides [DefaultReviewTimeout] — how long to wait for
	// Elk's auto-review pass after a submit.
	ReviewTimeout time.Duration

	// ReviewPollInterval overrides [DefaultReviewPollInterval].
	ReviewPollInterval time.Duration

	// MaxReviewRounds overrides [DefaultMaxReviewRounds].
	MaxReviewRounds int

	// Version is Rein's version, reported to Elk.
	Version string

	// Out receives the loop's log.
	Out io.Writer

	// Logs is where the per-run event logs are written — the stream `rein
	// tail` watches. Nil disables them, which is what most tests want and
	// what a runner with an unwritable Rein home falls back to; every call
	// site takes the same path either way.
	Logs *runlog.Store

	// LogKeepRuns is how many run logs to keep. Zero means keep everything,
	// which is the right default for a nil-store test and never what a real
	// runner passes — `rein run` passes [config.Config.KeepRuns].
	LogKeepRuns int

	// ControlListener is the local endpoint `rein attach` connects to. Nil
	// means no control plane, and then a run cannot be taken over — which is
	// what most tests want and what a runner whose endpoint is already held
	// by another process falls back to.
	//
	// A listener rather than a path, because the two platforms disagree about
	// what the endpoint even is (a unix socket, a named pipe) and because a
	// test then needs no filesystem at all.
	ControlListener net.Listener

	// NewClient builds the Elk client for a queue. Tests replace it; the
	// default is [elk.New].
	NewClient func(url, token string) (*elk.Client, error)

	// Worktrees creates and reaps worktrees. Tests replace it.
	Worktrees WorktreeManager

	// Landing reads what a run left on the remote, for the post-run check a
	// `pr` or `branch` queue gets (landing.go, ark:rein#47). Nil means
	// [GitHubLanding] — git and `gh` in the run's worktree. Tests replace it,
	// so that no assertion depends on GitHub.
	Landing LandingInspector

	// HostCapabilities is what this MACHINE holds — the environment half of a
	// packet's required_capabilities. Nil means detect it at startup, which is
	// what a real runner does; tests supply their own so that whether the box
	// happens to have `gh` installed is not part of the assertion.
	HostCapabilities *HostCapabilities

	// Rand seeds the poll jitter. Zero uses the process clock.
	Rand *rand.Rand
}

// WorktreeManager is the slice of internal/worktree the loop uses.
type WorktreeManager interface {
	Create(ctx context.Context, req worktree.Request) (*worktree.Worktree, error)
	Reap(ctx context.Context, wt *worktree.Worktree) error
}

// Runner serves one or more queues.
type Runner struct {
	upgradeMu      sync.Mutex
	draining       bool
	inFlight       int
	upgradeDone    chan struct{}
	upgradeStarted chan struct{}

	opts   Options
	queues []config.Queue
	gate   *slotGate
	host   *HostCapabilities
	logMu  sync.Mutex
	rnd    *rand.Rand
	rndMu  sync.Mutex

	// sessions is every session being driven right now, which is what the
	// control plane hands to `rein attach`.
	sessions *sessionRegistry

	// qrs is the queue runners [Runner.Run] started, kept so the telemetry
	// tick can beat for one whose own loop is inside a run and cannot.
	// Written once in Run before the tick starts, read-only afterwards.
	qrs []*queueRunner

	// machines caches one machine sample for every queue's beat to share.
	machines machineCache
}

// HostCapabilities returns what this runner believes the machine holds.
func (r *Runner) HostCapabilities() *HostCapabilities { return r.host }

// New builds a runner over the configured queues.
func New(opts Options) (*Runner, error) {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.NewClient == nil {
		opts.NewClient = func(url, token string) (*elk.Client, error) {
			return elk.New(url, token, elk.WithVersion(opts.Version))
		}
	}
	if opts.Worktrees == nil {
		opts.Worktrees = &worktree.Manager{Root: opts.Config.WorkDir}
	}
	if opts.Landing == nil {
		opts.Landing = GitHubLanding{}
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.StallAfter <= 0 {
		opts.StallAfter = DefaultStallAfter
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = DefaultMaxConcurrent
	}
	if opts.Headroom == nil {
		opts.Headroom = DetectHeadroom
	}
	if opts.Machine == nil {
		opts.Machine = DetectMachine
	}
	if opts.SlotInterval <= 0 {
		opts.SlotInterval = DefaultSlotInterval
	}
	if opts.Config.Elk.MCPURL == "" {
		return nil, errors.New("runner: no Elk endpoint in the config — run `rein enrol`")
	}

	queues, err := selectQueues(opts.Config, opts.Queues)
	if err != nil {
		return nil, err
	}
	rnd := opts.Rand
	if rnd == nil {
		rnd = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	// Detected once, at startup, rather than per run: the probes shell out, and
	// what a machine has does not change between two claims on the same poll.
	host := opts.HostCapabilities
	if host == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		host = DetectHostCapabilities(ctx, opts.Config)
	}
	r := &Runner{opts: opts, queues: queues, rnd: rnd, host: host, sessions: newSessionRegistry(), upgradeDone: make(chan struct{}), upgradeStarted: make(chan struct{})}
	// The disk half of the measurement is about the filesystem worktrees land
	// on, which is the work directory and not "/" — on a Mac with an external
	// scratch disk those are different volumes with different answers.
	workDir := opts.Config.WorkDir
	if workDir == "" {
		if dir, err := config.Dir(); err == nil {
			workDir = dir
		}
	}
	r.gate = &slotGate{
		hard:    opts.MaxConcurrent,
		workDir: workDir,
		probe:   opts.Headroom,
		logf:    r.logf,
		retry:   opts.SlotInterval,
	}
	return r, nil
}

func selectQueues(cfg config.Config, want []string) ([]config.Queue, error) {
	if len(cfg.Queues) == 0 {
		return nil, errors.New("runner: no queues configured — run `rein enrol`")
	}
	if len(want) == 0 {
		return cfg.Queues, nil
	}
	var out []config.Queue
	for _, name := range want {
		q, ok := cfg.Queue(name)
		if !ok {
			return nil, fmt.Errorf("runner: no queue %q in the config", name)
		}
		out = append(out, q)
	}
	return out, nil
}

// jitter spreads a poll interval by up to 20% so several queues on one
// machine, or several machines restarted together, do not all ask Elk at the
// same instant.
func (r *Runner) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	r.rndMu.Lock()
	defer r.rndMu.Unlock()
	spread := float64(d) * 0.2
	return d + time.Duration(r.rnd.Float64()*spread) - time.Duration(spread/2)
}

func (r *Runner) logf(format string, args ...any) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	fmt.Fprintf(r.opts.Out, time.Now().Format("15:04:05")+" "+format+"\n", args...)
}

// Run serves every selected queue until ctx is cancelled, or — with
// [Options.Once] — until each queue has had one claim attempt.
func (r *Runner) Run(ctx context.Context) error {
	// One cancellable context for everything that must not outlive the loop,
	// cancelled when Run returns. The queue goroutines keep the caller's own
	// ctx: they are what ENDS Run, and cancelling them from here would be the
	// loop stopping itself.
	inner, stopInner := context.WithCancel(ctx)
	defer stopInner()

	// The control plane, if one was given. It is served for the life of the
	// loop and stopped with it: an attachment outliving the runner would be a
	// person talking to a process that is not there.
	if ln := r.opts.ControlListener; ln != nil {
		go func() {
			if err := control.Serve(inner, ln, r.sessions, r.logf); err != nil && inner.Err() == nil {
				r.logf("attach: the control plane stopped: %v", err)
			}
		}()
	}

	r.startUpgradeWatch(inner)
	var wg sync.WaitGroup
	errs := make([]error, len(r.queues))
	for i, q := range r.queues {
		qr, err := r.newQueueRunner(q)
		if err != nil {
			// One misconfigured queue must not stop the others: a machine
			// serving three agents should keep serving two.
			r.logf("%s: %v", q.Name, err)
			errs[i] = err
			continue
		}
		r.qrs = append(r.qrs, qr)
		wg.Add(1)
		go func(i int, qr *queueRunner) {
			defer wg.Done()
			errs[i] = qr.serve(ctx)
		}(i, qr)
	}

	// The daemon's one periodic loop. It is started after the queue runners
	// exist and before they are waited on, so that r.qrs is complete before
	// anything reads it — and skipped under --dry-run, which exists to claim
	// nothing and say what is waiting, not to open a second conversation with
	// Elk about the machine.
	if len(r.qrs) > 0 && !r.opts.DryRun && !r.opts.TelemetryOff {
		go r.telemetryTick(inner)
	}

	wg.Wait()
	if r.isDraining() {
		return ErrRestartForUpgrade
	}
	return errors.Join(errs...)
}

func (r *Runner) newQueueRunner(q config.Queue) (*queueRunner, error) {
	ws := r.opts.Config.WorkspaceFor(q)
	if ws == "" {
		return nil, errors.New("no workspace — set `workspace` in the config or re-enrol")
	}
	token, err := r.opts.Store.Get(ws, q.Name)
	if err != nil {
		return nil, fmt.Errorf("no stored token: %w — run `rein enrol`", err)
	}
	client, err := r.opts.NewClient(r.opts.Config.Elk.MCPURL, token)
	if err != nil {
		return nil, err
	}
	mode := adapter.PermissionMode(q.PermissionModeOrDefault())
	if !mode.Valid() {
		return nil, fmt.Errorf("permission_mode %q is not one of read_only, ask, accept_edits, full", mode)
	}
	host := r.host.withExtra("config.toml, this queue", q.Capabilities...)
	// The Wrangler's routing capability is reserved for the opt-in, even if
	// the environment lists it. On the wire it is still `pm` — Elk's
	// api_set_pm_executor checks that name and declared_capabilities persists
	// it — and `wrangler`, which is no capability at all, is never declared.
	delete(host.how, "pm")
	delete(host.how, "wrangler")
	if q.IsWrangler() {
		// Declared for routing; granted per run, to Wrangler cycles only
		// (drive.go, wranglerCycle).
		host = host.withExtra(wranglerOptIn, "pm", "mcp:elk")
	}
	if q.Scoped() {
		// What a scoped queue's runs actually hold: no vault, no stray
		// credential names, and each name in its map (secrets.go).
		host = host.scopedTo(q)
	}
	qr := &queueRunner{r: r, q: q, workspace: ws, elk: client, mode: mode, host: host}
	// The per-run MCP allow-list: what every run on this queue is handed, and
	// the `mcp:<name>` it declares for each, resolved together (mcp.go).
	if a, ok := adapter.Lookup(q.AgentKind); ok {
		qr.mcp = resolveMCP(r.opts.Config, q, a)
		if q.IsWrangler() {
			qr.mcp = mcpAllowList{declared: []string{"mcp:elk"}}
		}
		if len(qr.mcp.declared) > 0 {
			qr.host = qr.host.withExtra("config.toml mcp_servers, this queue", qr.mcp.declared...)
		}
	}
	qr.loadSubscription()
	return qr, nil
}

// queueRunner serves one queue.
//
// One run at a time: [queueRunner.serve] calls [queueRunner.claimAndDrive]
// serially, which is what lets the current run's event log live in a field
// rather than being threaded through every method that might want to record
// something. The concurrency in this loop is between queues, never within one.
type queueRunner struct {
	r         *Runner
	q         config.Queue
	workspace string
	elk       *elk.Client
	mode      adapter.PermissionMode
	host      *HostCapabilities

	// live is the session being driven right now, registered so `rein attach`
	// can reach it, and nil between runs.
	live *liveSession

	// human is what a person did to the current run through `rein attach`,
	// accumulated across attachments and across a stall restart's two
	// sessions. Guarded because the control plane's goroutine writes it.
	human   human
	humanMu sync.Mutex

	// log is the current run's event log, and logStatus is how the run will
	// be recorded as having ended. Both are set by drive and cleared by it.
	// A nil log is a working no-op — see [runlog.Writer].
	log       *runlog.Writer
	logStatus runlog.Status

	// tel is this queue's fleet reading state, and lastReplyShape is what Elk
	// last said back. Both are guarded because the telemetry tick reads and
	// writes them from a goroutine that is not the one driving the run — see
	// telemetry.go.
	telMu          sync.Mutex
	tel            queueTelemetry
	lastReplyShape string
	preflightState string

	// sub is this queue's subscription headroom: the last reading, carried
	// forward, and whether the queue is holding off claiming because the
	// subscription has none left. See subscription.go.
	sub subscription

	// mcp is this queue's per-run MCP allow-list, resolved at startup. See
	// mcp.go.
	mcp mcpAllowList

	// redact is the current run's secret redactor — nil between runs and for
	// a queue with no secrets map (secrets.go). Atomic because the control
	// plane's goroutine logs through logf while the run's goroutine sets it.
	redact atomic.Pointer[secretenv.Redactor]

	// slot is the concurrency slot the current run holds — nil between runs,
	// and released (but still set) while Elk's review pass has the run
	// (drive.go, retakeSlot). Like log, it lives in a field because this
	// queue drives one run at a time.
	slot *slotHold

	// lent and lentAt are a Wrangler queue lending its place at the front of
	// the line (claimPriority): set when a claim made at Wrangler priority
	// turned out to be a build, cleared once any later claim on the machine
	// has had a slot. Only this queue's own goroutine touches them.
	lent   bool
	lentAt uint64

	// claimErr is the last claim failure, as reported, and when it was
	// logged — so a queue Elk keeps refusing (a parked agent, a database
	// timing out) says so once a lease rather than on every poll.
	claimErr   string
	claimErrAt time.Time
}

// logf writes one line to the daemon's log, through the current run's
// redactor: an error a session surfaces can carry the agent's own output.
func (qr *queueRunner) logf(format string, args ...any) {
	line := fmt.Sprintf(qr.q.Name+": "+format, args...)
	qr.r.logf("%s", qr.redactString(line))
}

func (qr *queueRunner) serve(ctx context.Context) error {
	qr.checkAgentSetup(ctx)
	qr.logf("serving %s in %q, permission mode %s, landing policy %s",
		qr.q.AgentKind, qr.workspace, qr.mode, qr.q.LandOrDefault())
	qr.logf("model %s, effort %s", orCLIDefault(qr.q.Model), orCLIDefault(qr.q.Effort))
	qr.logf("host capabilities: %s", strings.Join(qr.host.Describe(), ", "))
	if !qr.q.Scoped() {
		if names := qr.passEnv(len(qr.mcp.servers) > 0); len(names) > 0 {
			qr.logf("environment: system variables plus %s", strings.Join(names, ", "))
		} else {
			qr.logf("environment: system variables only")
		}
	}
	if len(qr.mcp.servers) > 0 {
		names := make([]string, 0, len(qr.mcp.servers))
		for _, c := range qr.mcp.declared {
			names = append(names, strings.TrimPrefix(c, "mcp:"))
		}
		qr.logf("MCP servers every run gets: %s", strings.Join(names, ", "))
	}
	for _, s := range qr.mcp.skipped {
		qr.logf("MCP server left out: %s", s)
	}
	if qr.q.Scoped() {
		names := secretNames(qr.q)
		if len(names) == 0 {
			qr.logf("scoped secrets: none — runs get no credentials from this machine")
		} else {
			qr.logf("scoped secrets, read from the keychain at each run: %s", strings.Join(names, ", "))
		}
	}
	if qr.q.IsWrangler() {
		qr.logf("Wrangler queue: its claims take the next free slot on this machine ahead of build claims")
	}

	if qr.r.opts.DryRun {
		return qr.dryRun(ctx)
	}

	// The zero value makes the first poll claim whatever the depth says. A
	// runner starting up is a runner that may just have crashed, and its own
	// stranded runs are the ones it should sweep first.
	var lastClaim time.Time

	// -1 so the first heartbeat always says something: a service's log has to
	// show it came up, not only that it did not fall over.
	lastDepth, lastDepthLog := -1, time.Time{}

	// heldLogged is the hold last reported as "not claiming", so a held queue
	// says so once per hold rather than on every poll.
	var heldLogged time.Time

	for {
		// Headroom before the beat, so the beat carries it: an adapter that
		// can read its subscription window without a turn does so here, when
		// the reading is due (subscription.go). The others carry forward what
		// their last run saw.
		select {
		case <-qr.r.upgradeDone:
			return nil
		default:
		}
		qr.refreshHeadroom(ctx)

		// The heartbeat is the poll. It is what puts this queue in
		// `list_executors` as online — Elk does not itself refuse to hand
		// work to a queue it thinks is offline, but the router and the people
		// dispatching read that view — and its reply carries the queue depth,
		// which means one call answers both "am I visible" and "is there
		// anything".
		depth, known := qr.heartbeat(ctx)

		// Say so in the log. A daemon that writes three lines at startup and
		// then nothing for a week is indistinguishable from a daemon that
		// died, and under the service the log is the only window there is.
		// Every poll would be 120 lines an hour per queue, so: whenever the
		// answer changes, and otherwise once a lease.
		if known && (depth != lastDepth || time.Since(lastDepthLog) >= LeaseDuration) {
			qr.logf("online — %d waiting", depth)
			lastDepth, lastDepthLog = depth, time.Now()
		}
		if known && depth == 0 {
			// Nothing is waiting, so there is nothing to explain.
			qr.setWaiting("")
		}

		// But the depth does not answer the whole question. Elk's
		// `queue_depth` counts `status='queued'` rows only, while
		// `claim_run`'s CAS also reclaims a `running` row whose lease has
		// lapsed (elk-mcp/index.ts:935). So depth == 0 means "nothing NEW to
		// do", not "nothing to do" — and the gap between those two is exactly
		// the state a crashed or killed runner leaves behind.
		//
		// Gating on depth alone was therefore blindest precisely when
		// recovery mattered most: `--once` exited 0 without asking, while two
		// runs sat `running` with half-hour-old leases
		// (ark:rein#23 (01M182VPNT1X9E1JA60MRQ9BXS)). So:
		//
		//   - `--once` always asks. Someone requesting one run wants the
		//     question put to the server, not answered from a counter that
		//     cannot see half the answer.
		//   - the loop asks anyway once a lease has passed since it last did.
		//     One extra round trip per queue per 15 minutes, bounded and
		//     cheap, and the only thing that recovers a lapsed run.
		sweepDue := time.Since(lastClaim) >= qr.reclaimInterval()
		wantClaim := qr.r.opts.Once || !known || depth > 0 || sweepDue

		// Back off while the subscription is exhausted (ark:rein#40). Every
		// run claimed now would start an agent that fails on its first
		// request; not claiming leaves the work queued for when the window
		// resets — or for another agent, once Elk's router reads the
		// `exhausted_until` this queue reports on every beat. The sweep for
		// lapsed runs is skipped too: recovering a stranded run onto a spent
		// subscription only fails it.
		until, reason, held := qr.holding()
		if held && wantClaim {
			if !until.Equal(heldLogged) {
				qr.logf("not claiming until %s: %s", until.UTC().Format(time.RFC3339), reason)
				heldLogged = until
			}
			wantClaim = false
			// exhausted_until and exhausted_reason already say why, on
			// every beat; a second sentence would only disagree with them.
			qr.setWaiting("")
		}

		var (
			claimed bool
			err     error
		)
		if wantClaim && qr.r.isDraining() && (!known || depth > 0) {
			qr.setWaiting("not claiming: a newer Rein is installed, and this one restarts into it " +
				"once the runs it holds are finished")
		}
		if wantClaim && !qr.r.isDraining() {
			lastClaim = time.Now()
			var clear string
			claimed, clear, err = qr.claimAndDrive(ctx)
			if ctx.Err() == nil {
				qr.noteClaimOutcome(claimed, clear, err, depth, known)
			}
		}
		if qr.r.opts.Once {
			if !held && !claimed && err == nil && ctx.Err() == nil {
				// Say it. Otherwise the log just ends, and an operator cannot
				// tell "asked, and the queue was empty" from "never asked".
				qr.logf("nothing waiting to claim")
			}
			return err
		}
		wait := qr.r.jitter(qr.r.opts.PollInterval)
		if claimed {
			// Work was just done; ask again straight away rather than
			// sleeping through a queue that may still be full.
			wait = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-qr.r.upgradeDone:
			return nil
		case <-time.After(wait):
		}
	}
}

// dryRun reports what is waiting without claiming any of it. The heartbeat
// reply carries the queue depth, which is why it can answer at all.
func (qr *queueRunner) dryRun(ctx context.Context) error {
	hb, err := qr.elk.HeartbeatExecutor(ctx, qr.heartbeatRequest())
	if err != nil {
		if errors.As(err, new(*elk.UnknownToolError)) {
			qr.logf("dry run: this Elk has no heartbeat_executor, and asking any other way would claim work")
			return nil
		}
		return err
	}
	qr.logf("dry run: %d waiting", hb.Waiting)
	return nil
}

// reclaimInterval is how long the loop will trust an empty queue before
// claiming anyway, to sweep up runs left `running` by a dead runner.
func (qr *queueRunner) reclaimInterval() time.Duration {
	if d := qr.r.opts.ReclaimInterval; d > 0 {
		return d
	}
	return LeaseDuration
}

func (qr *queueRunner) heartbeatRequest() elk.HeartbeatRequest {
	req := elk.HeartbeatRequest{
		Workspace: qr.workspace,
		Queue:     qr.q.Name,
		HostID:    qr.r.opts.Config.MachineID,
		AgentKind: qr.q.AgentKind,
	}
	// Both namespaces. Elk's declared_capabilities is advisory input to Pace —
	// "so Elk avoids handing you work you cannot do" — and a packet's
	// requirements are drawn from both, so declaring only the runtime half
	// would tell Pace nothing about the half that varies between machines.
	if a, ok := adapter.Lookup(qr.q.AgentKind); ok {
		req.DeclaredCapabilities = DeclaredCapabilities(a.Manifest(), qr.host)
	} else {
		req.DeclaredCapabilities = qr.host.Names()
	}
	// The fleet reading rides every beat, from whichever clock sent it. It is
	// free here — the call is happening anyway — and never costs the beat: Elk
	// counts the machine alive whether or not it could store the reading.
	if !qr.r.opts.TelemetryOff {
		req.Telemetry = qr.telemetry()
	}
	return req
}

// heartbeat marks the machine online and reports how many runs are waiting.
//
// known is false when Elk did not answer with a depth — the tool is not
// deployed, or the call failed — and the caller then claims unconditionally
// rather than treating silence as an empty queue. It never fails the loop: a
// blip must not stop a runner that can still claim and report.
func (qr *queueRunner) heartbeat(ctx context.Context) (depth int, known bool) {
	hb, err := qr.elk.HeartbeatExecutor(ctx, qr.heartbeatRequest())
	switch {
	case err == nil:
		qr.noteBeat()
		qr.noteHeartbeatReply(hb.Text)
		return hb.Waiting, true
	case ctx.Err() != nil:
	case errors.As(err, new(*elk.UnknownToolError)):
		// Expected against an Elk that has not shipped it yet.
	default:
		qr.logf("heartbeat: %v", err)
	}
	return 0, false
}

// noteHeartbeatReply logs Elk's answer the first time it says something new.
//
// A reading Elk could not store comes back as a NOTE IN THE REPLY rather than
// as an error — the beat must always stay open — so reading what the server
// said is the only way to find out that telemetry is being refused. Rein does
// not try to recognise the wording, which would go stale the moment Elk
// rephrased it; it prints the sentence and lets a person read it.
//
// The dedupe is on the reply with its numbers blanked, because the only thing
// that changes in an ordinary reply is the queue depth, and a runner that
// logged the same sentence twice a minute for a week would have drowned the
// one line that mattered.
func (qr *queueRunner) noteHeartbeatReply(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	shape := digitsRE.ReplaceAllString(text, "#")
	qr.telMu.Lock()
	changed := shape != qr.lastReplyShape
	qr.lastReplyShape = shape
	qr.telMu.Unlock()
	if changed {
		qr.logf("heartbeat: %s", collapseLines(text))
	}
}

// digitsRE blanks the numbers in a heartbeat reply so that a changing queue
// depth does not read as a changing message.
var digitsRE = regexp.MustCompile(`\d+`)

// collapseLines puts a multi-line reply on one log line.
func collapseLines(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// claimAndDrive takes the concurrency slot BEFORE claiming, not after.
// Claiming first and then queueing for a slot would hold a lease Rein is not
// yet working, and a lease held without a report lapses in fifteen minutes.
//
// The slot is granted against live headroom rather than a fixed count, and in
// order (slotgate.go): a Wrangler queue's claim goes ahead of every build
// claim, so waiting here can mean "the machine cannot afford another run yet",
// "every slot is taken", or "a Wrangler cycle is next". Which one is reported to
// Elk on the queue's beat while it waits (ark:rein#59).
//
// clear is Elk's own sentence when claim_run handed nothing over, so the serve
// loop can tell a heartbeat that counted waiting work it could not claim.
func (qr *queueRunner) claimAndDrive(ctx context.Context) (claimed bool, clear string, err error) {
	// Waiting for headroom is not an in-flight run. Drain cancels only
	// this wait, never the context passed to a claimed run.
	gateCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-gateCtx.Done():
		case <-qr.r.upgradeStarted:
			cancel()
		}
	}()
	prio := qr.claimPriority()
	logged := false
	hold, ok := qr.r.gate.enter(gateCtx, slotRequest{queue: qr.q.Name, prio: prio}, func(reason string) {
		qr.setWaiting(reason)
		if !logged {
			qr.logf("%s", reason)
			logged = true
		}
	})
	if !ok {
		return false, "", nil
	}
	qr.slot = hold
	defer qr.releaseSlot()
	if logged {
		// The slot wait is over. Any other reason stands until the claim
		// says something new (noteClaimOutcome).
		qr.setWaiting("")
	}
	if hold.waited >= time.Minute {
		qr.logf("a slot is free after %s waiting", span(hold.waited))
	}
	if !qr.r.beginClaim() {
		return false, "", nil
	}
	defer qr.r.endClaim()

	wo, err := qr.elk.ClaimRun(ctx, elk.ClaimRequest{Workspace: qr.workspace, Queue: qr.q.Name})
	switch {
	case errors.Is(err, elk.ErrNoWork):
		return false, strings.TrimPrefix(err.Error(), elk.ErrNoWork.Error()+": "), nil
	case err != nil:
		return false, "", fmt.Errorf("claim: %w", err)
	}
	hold.setRun(wo.RunID)
	qr.setWaiting("")
	for _, n := range wo.Notices {
		qr.logf("workspace notice: %s", n)
	}
	qr.logf("claimed run %s — %s", wo.RunID, firstLine(wo.Direction))
	if prio == prioWrangler && !wranglerCycle(qr.q, wo.RequiredCapabilities) {
		// The front of the line was spent on a build. Lend it back until
		// another claim on this machine has had a slot, so a queue that is
		// both the Wrangler and a busy build queue cannot take every turn.
		qr.lent, qr.lentAt = true, hold.grant
		qr.logf("run %s is not a Wrangler cycle; this queue's next claim waits its turn behind other queues", wo.RunID)
	}
	return true, "", qr.drive(ctx, wo)
}

// claimPriority is where this queue's next claim stands in line for a slot
// (ark:rein#60).
//
// A Wrangler queue goes first. That is decided by the QUEUE, not the run,
// because the run is not known until it is claimed — Elk's claim_run hands
// over the next run on a queue and the heartbeat says only how many there are
// — and mac-claude is both Elk Scout's Wrangler and an ordinary Claude build
// queue. So a Wrangler queue's place at the front can be spent on a build.
// When it is, the queue lends it back: its next claim stands in line like any
// build until some other claim has been granted a slot, and then it is first
// again. At worst a Wrangler queue that is also a busy build queue takes every
// other turn; it never takes every turn.
//
// The alternative ark:rein#60 offered — a slot kept free for Wrangler cycles
// even at headroom one — was not taken for the same reason: the run that slot
// claimed could be a build, and a build in a slot the machine was measured not
// to afford is the 2026-08-20 freeze.
func (qr *queueRunner) claimPriority() slotPriority {
	if !qr.q.IsWrangler() {
		return prioBuild
	}
	if qr.lent && qr.r.gate.grantCount() > qr.lentAt {
		qr.lent = false
	}
	if qr.lent {
		return prioBuild
	}
	return prioWrangler
}

// releaseSlot gives back whatever slot the current run holds. Safe when the
// slot was already given back for a review wait.
func (qr *queueRunner) releaseSlot() {
	qr.slot.release()
	qr.slot = nil
}

// noteClaimOutcome records why work Elk counted on this queue was not
// started, so it rides the next beat instead of going unsaid (ark:rein#59).
//
// Three outcomes need a sentence. A claim that failed: Elk refused it (a
// parked agent says so in its own words) or could not be reached. And a claim
// that came back empty while the heartbeat had just counted runs waiting —
// the shape of the 2026-10-06 report, where a queue said "2 waiting" for hours
// while claiming other, newer work. Claim ORDER within a queue is Elk's:
// Rein never names a run, it asks claim_run for the queue's next one, and Elk
// hands over the oldest group first. When the count and the claim disagree,
// Rein cannot see which runs were passed over, but it can say that some were.
func (qr *queueRunner) noteClaimOutcome(claimed bool, clear string, err error, depth int, known bool) {
	switch {
	case err != nil:
		why := claimFailure(err)
		if why != qr.claimErr || time.Since(qr.claimErrAt) >= LeaseDuration {
			// The local log keeps the whole error; Elk gets the safe sentence.
			qr.logf("%v", err)
			qr.claimErr, qr.claimErrAt = why, time.Now()
		}
		qr.setWaiting(why)
		return
	case qr.claimErr != "":
		qr.logf("claim_run is answering again")
		qr.claimErr = ""
	}
	if !claimed && clear != "" && known && depth > 0 {
		why := fmt.Sprintf("Elk's heartbeat counted %d waiting on this queue, but claim_run handed none over: %s",
			depth, clear)
		if qr.setWaiting(why) {
			qr.logf("%s", why)
		}
		return
	}
	qr.setWaiting("")
}

// claimFailure is a claim error as a sentence Elk may be shown.
//
// Only words Elk itself wrote, or Rein's own, ever leave the machine: a
// transport error carries the connector URL, and the connector URL carries the
// queue's token. The full error still goes to the local log.
func claimFailure(err error) string {
	var (
		tier *elk.TierRefusalError
		tool *elk.ToolError
		rpc  *elk.RPCError
		hErr *elk.HTTPError
	)
	switch {
	case errors.As(err, &tier):
		return fmt.Sprintf("claim_run refused this queue's token: it is %s, and claim_run needs %s — re-enrol the queue",
			tier.Held, tier.Required)
	case errors.As(err, &tool):
		return "claim_run refused: " + clip(firstLine(tool.Text), 300)
	case errors.As(err, &rpc):
		return fmt.Sprintf("claim_run failed: Elk answered error %d: %s", rpc.Code, clip(firstLine(rpc.Message), 200))
	case errors.As(err, &hErr):
		return "claim_run failed: Elk answered " + hErr.Status
	case errors.As(err, new(*elk.UnknownToolError)):
		return "claim_run is not offered by this Elk"
	}
	return "claim_run failed: Elk could not be reached"
}

// clip shortens s to at most n bytes for a one-line reason, on a rune
// boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
