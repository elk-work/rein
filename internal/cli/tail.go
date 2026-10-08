package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/tail"
	"github.com/spf13/cobra"
)

// newTailCommand: the live view. ark:rein#31 (01M18KYXF21NKKJS4X30PXJWWK).
//
// It reads the run logs and talks to nothing else — not Elk, not the daemon.
// That is what makes it safe to point at a machine whose service is running:
// there is no way for a person watching a run to disturb it.
func newTailCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tail [run|queue]",
		Short: "Watch runs and their agent event streams live",
		Long: `Watch what the agents on this machine are actually doing.

With no arguments it is the all-queues view: one line per run in flight —
queue, run id, direction, elapsed, last event, tokens — refreshing in place
over a scrolling pane of the agents' own output as it happens.

  rein tail                    every run in flight
  rein tail mac-claude         every run on one queue
  rein tail a1b2c3d4           one run, by id or by a unique prefix
  rein tail a1b2c3d4 --raw     the same, as the log's own JSON

A run that has already finished replays from its log and exits, so this is
also how to read back what happened in a worktree that no longer exists.
Assistant text is shown verbatim; a tool call is one line; a permission
request or a question is highlighted, because those are the two things that
mean the run is waiting on somebody.

The logs are ~/.rein/log/runs/<run id>.jsonl, written by ` + "`rein run`" + `. Nothing
here talks to Elk or to the daemon, so it is safe to watch a service that is
mid-run.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			return runTail(cmd, ref)
		},
	}
	f := cmd.Flags()
	f.Bool("raw", false, "print the log's own JSON, one record per line, instead of rendering it")
	f.Duration("since", 0, "replay this much history before following (default: 2m for a live run, all of it for a finished one)")
	f.Duration("interval", 0, "how often to re-read the logs (default: 400ms)")
	f.Bool("no-color", false, "never use ANSI colour, even on a terminal")
	f.Int("width", 0, "override the terminal width")
	return cmd
}

func runTail(cmd *cobra.Command, ref string) error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	store, err := runlog.Open(dir)
	if err != nil {
		return err
	}

	f := cmd.Flags()
	raw, _ := f.GetBool("raw")
	since, _ := f.GetDuration("since")
	interval, _ := f.GetDuration("interval")
	noColor, _ := f.GetBool("no-color")
	width, _ := f.GetInt("width")

	out := cmd.OutOrStdout()
	tty, detected := false, tail.DefaultWidth
	if stdout, ok := out.(*os.File); ok {
		tty, detected = tail.Terminal(stdout)
	}
	if width <= 0 {
		width = detected
	}
	// NO_COLOR is the informal standard every CLI in this org honours; a pipe
	// gets no colour whatever anyone asked for, because the escapes end up in
	// the file.
	color := tty && !noColor && os.Getenv("NO_COLOR") == ""

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = tail.Run(ctx, tail.Options{
		Store:    store,
		Ref:      ref,
		Since:    since,
		SinceSet: f.Changed("since"),
		Raw:      raw,
		Interval: interval,
		Out:      out,
		TTY:      tty,
		Width:    width,
		Color:    color,
	})
	if err != nil {
		return fmt.Errorf("%w\n\nRun logs live in %s. `rein status` lists the queues this machine serves.",
			err, store.Dir)
	}
	return nil
}
