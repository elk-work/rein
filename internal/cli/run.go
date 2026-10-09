package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/control"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
	"github.com/elk-work/rein/internal/service"
	"github.com/elk-work/rein/internal/worktree"
	"github.com/spf13/cobra"
)

// newRunCommand: the daemon loop — claim → worktree → spawn → watch → report →
// submit → reap. ark:rein#7 (01M17TTZN8T7QCZJ2CS9N9F2HP).
func newRunCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Claim and drive dispatched runs (the daemon loop)",
		Long: `Poll each configured queue for dispatched runs, claim one atomically,
create an isolated git worktree for it, drive the queue's coding agent against
the packet, report progress, submit the deliverable, and reap the worktree.

This is the loop the service runs. Run it in the foreground to watch it.

  rein run --queue mac-claude --once

claims at most one run per queue and exits — the shape the Phase 0 dogfood
uses. Ctrl-C stops the loop: a run in flight is interrupted and its claim is
left to lapse, so the run returns to the queue rather than being reported as an
outcome that did not happen.

The state machine, the exact MCP calls and how a run's repository is chosen are
in docs/run-loop.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRun(cmd)
		},
	}
	f := cmd.Flags()
	f.StringSlice("queue", nil, "queue(s) to serve (default: every queue in the config)")
	f.Bool("once", false, "claim and drive at most one run per queue, then exit")
	f.Duration("poll-interval", 0, "how often to poll for dispatched runs (default: 30s, jittered)")
	f.Int("max-concurrent", 0, "ceiling on runs driven at once across every queue (default: 4, and lower whenever the machine cannot afford it)")
	f.String("log-file", "", "append the loop's log to this file instead of stdout")
	f.Bool("service", false, "run under this platform's service manager (what `rein service install` sets up)")
	f.Bool("dry-run", false, "claim nothing; report what is waiting")
	f.Bool("keep-worktrees", false, "leave each run's worktree in place instead of reaping it after the submit")
	f.Duration("stall-after", 0, "how long a session may produce no events before the watchdog acts (default: 15m)")
	f.Duration("review-timeout", 0, "how long to wait for Elk's auto-review after submitting (default: 20m)")
	f.Int("max-review-rounds", 0, "how many rounds of Elk's review revisions to work before giving up (default: 3)")
	return cmd
}

func runRun(cmd *cobra.Command) error {
	path, err := resolveConfigPath(cmd)
	if err != nil {
		return err
	}
	cfg, err := config.LoadFile(path)
	if errors.Is(err, config.ErrNotExist) {
		return fmt.Errorf("no config at %s — run `rein enrol` first", path)
	}
	if err != nil {
		return err
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	store, err := keyring.Open(dir)
	if err != nil {
		return err
	}
	// The keychain items scoped queues' secrets maps name (ark:rein#48). Read
	// per run, never here: opening the reader touches no item.
	items, err := keyring.OpenItems(dir)
	if err != nil {
		return err
	}

	f := cmd.Flags()
	queues, _ := f.GetStringSlice("queue")
	once, _ := f.GetBool("once")
	dryRun, _ := f.GetBool("dry-run")
	keep, _ := f.GetBool("keep-worktrees")
	maxConcurrent, _ := f.GetInt("max-concurrent")
	pollInterval, _ := f.GetDuration("poll-interval")
	stallAfter, _ := f.GetDuration("stall-after")
	reviewTimeout, _ := f.GetDuration("review-timeout")
	maxReviewRounds, _ := f.GetInt("max-review-rounds")

	asService, _ := f.GetBool("service")
	logFile, _ := f.GetString("log-file")

	out := cmd.OutOrStdout()
	if logFile != "" {
		// A Windows service has no stdout to redirect and a launchd agent's
		// is a file nobody remembers the name of, so the loop writes its own
		// log where `rein service status` can point at it.
		w, err := logWriter(logFile)
		if err != nil {
			return err
		}
		defer w.Close()
		out = w
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = dir
	}
	// The per-run event log — what `rein tail` watches. A store that cannot be
	// opened is reported and then ignored: a runner that refused to serve
	// queues because it could not write a log would be trading the work for
	// the account of the work.
	logs, err := runlog.Open(dir)
	if err != nil {
		fmt.Fprintf(out, "run logs disabled: %v\n", err)
		logs = nil
	}
	// The control endpoint `rein attach` connects to. Reported and skipped
	// when it cannot be opened — usually because another `rein run` already
	// holds this Rein home, in which case the right thing is to keep serving
	// the queues rather than to refuse over a hatch nobody may use.
	controlLn, err := control.Listen(dir)
	if err != nil {
		fmt.Fprintf(out, "attach unavailable: %v\n", err)
		controlLn = nil
	} else {
		defer controlLn.Close()
		fmt.Fprintf(out, "attach ready on %s\n", control.Endpoint(dir))
	}
	r, err := runner.New(runner.Options{
		Config:          cfg,
		ConfigPath:      absPath(path),
		UnderService:    asService && !service.Interactive(),
		Store:           store,
		Secrets:         items,
		Queues:          queues,
		Once:            once,
		DryRun:          dryRun,
		KeepWorktrees:   keep,
		MaxConcurrent:   maxConcurrent,
		PollInterval:    pollInterval,
		StallAfter:      stallAfter,
		ReviewTimeout:   reviewTimeout,
		MaxReviewRounds: maxReviewRounds,
		// Telemetry is configured rather than flagged: it describes the
		// machine, and the machine is a service nobody types a command line
		// at. See internal/runner/telemetry.go and the [telemetry] block in
		// internal/config.
		TelemetryInterval: cfg.Telemetry.Interval.Duration(),
		TelemetryOff:      cfg.Telemetry.Disabled,
		// Where a queue's subscription hold is remembered across a restart
		// (internal/runner/subscription.go, ark:rein#40).
		StateDir:        filepath.Join(dir, "state"),
		Version:         cmd.Root().Version,
		Out:             out,
		Logs:            logs,
		LogKeepRuns:     cfg.KeepRuns(),
		ControlListener: controlLn,
		Worktrees: &worktree.Manager{
			Root: workDir,
			Logf: func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) },
		},
	})
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	if asService {
		// Under a service manager the stop signal is the manager's, not a
		// terminal's — and on Windows it is not a signal at all but a control
		// request the process has seconds to answer or be killed.
		return service.Run(ctx, service.Config{
			Home:       dir,
			ConfigPath: mustString(cmd, "config"),
		}, func(ctx context.Context) error {
			return driveRunner(ctx, r, out)
		})
	}

	// Ctrl-C and SIGTERM stop the loop cleanly: an in-flight session is
	// interrupted and nothing is submitted for it, so the claim lapses and Elk
	// hands the run to whoever asks next. Reporting an outcome on the way out
	// would be reporting work that did not finish.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return driveRunner(ctx, r, out)
}

// driveRunner runs the loop and turns "the context was cancelled" into a clean
// exit rather than an error, because a stop is not a failure whether it came
// from Ctrl-C or from launchd.
func driveRunner(ctx context.Context, r *runner.Runner, out io.Writer) error {
	start := time.Now()
	err := r.Run(ctx)
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		fmt.Fprintf(out, "stopped after %s\n", time.Since(start).Round(time.Second))
		return nil
	}
	return err
}

// absPath makes a --config path absolute, so the binary a service later asks
// to check it (upgrade.go) reads the same file whatever its directory.
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// mustString reads a string flag, ignoring the "no such flag" error that
// cannot happen for a flag declared on the same command.
func mustString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}
