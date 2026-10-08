package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/control"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/tail"
	"github.com/spf13/cobra"
)

// newAttachCommand: the human-takeover hatch.
// ark:rein#26 (01M1895B4M85C301MB5N90AT96).
//
// It was half of ark:rein#8 until the service installer landed; a takeover of
// a session already running in another process is its own problem, and pinning
// it to the installer's task was hiding that.
func newAttachCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attach [run|queue]",
		Short: "Take over a live agent session in a terminal",
		Long: `Take over the live coding-agent session for a run. The daemon steps back —
its watchdog is paused for as long as you are here — and resumes when you
detach. It keeps reporting to Elk throughout, so the run does not lose its
lease while you think.

  rein attach                  the only run in flight
  rein attach mac-claude       the run on one queue
  rein attach 2e0a3e94         one run, by id or by a unique prefix
  rein attach 2e0a3e94 --read-only

You type lines; the agent answers, and its stream is rendered as ` + "`rein tail`" + `
renders it. Lines beginning with a colon are commands:

  :detach        leave; the daemon resumes driving
  :allow  [why]  approve the permission request the agent is waiting on
  :deny   [why]  refuse it
  :status        what this run is and what you can do to it
  :help

Ctrl-D detaches too, and so does anything that closes the terminal — the
attachment lasts exactly as long as the connection, so there is no state left
behind by a dropped ssh session.

This is the takeover hatch, not the primary drive: Rein watches structured
events, never scrollback. Adapters that take no mid-run input — grok, which
declares no ` + "`steer`" + ` capability — can only be watched, and attach says so
before you type rather than after.

When the session ends, the run continues exactly as it would have: Rein
submits the deliverable, and its footer records that a person took part and
how much they said. What you typed stays in this machine's run log.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			readOnly, _ := cmd.Flags().GetBool("read-only")
			return runAttach(cmd, ref, readOnly)
		},
	}
	cmd.Flags().Bool("read-only", false, "follow the session without attaching, so the daemon keeps driving")
	return cmd
}

func runAttach(cmd *cobra.Command, ref string, readOnly bool) error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --read-only is not a weaker attachment; it is no attachment at all.
	// Following a run without taking it over is exactly `rein tail`, and
	// pretending otherwise would mean the daemon stepping back for somebody
	// who has no intention of driving.
	if readOnly {
		fmt.Fprintf(out, "Following %s read-only. The daemon keeps driving; nothing you type reaches it.\n",
			orDash(ref))
		return followRun(ctx, cmd, dir, ref)
	}

	client, err := control.Dial(ctx, dir)
	if err != nil {
		if errors.Is(err, control.ErrNoRunner) {
			return fmt.Errorf("%w\n\nNothing is listening for an attach on this machine. Either no `rein run` "+
				"is going — `rein service status` says whether the service is up — or it is an older build "+
				"with no control plane. `rein tail` reads the logs either way.", err)
		}
		return err
	}
	defer client.Close()

	att, err := client.Attach(ref)
	if err != nil {
		return err
	}
	run := att.Run

	fmt.Fprintf(out, "Attached to %s on %s — %s\n", runlog.Short(run.RunID), run.Queue, run.Direction)
	if run.SessionID != "" {
		fmt.Fprintf(out, "%s session %s\n", run.AgentKind, run.SessionID)
	}
	fmt.Fprintln(out, att.Note)
	if run.Interactive {
		fmt.Fprintln(out, "Type to talk to it. `:detach` or Ctrl-D leaves; `:help` lists the rest.")
	}
	fmt.Fprintln(out)

	// The stream is the run's log, rendered exactly as `rein tail` renders it,
	// and it is what ends this command: when the run settles, the follow
	// returns and the attachment goes with it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if in, ok := cmd.InOrStdin().(io.Reader); ok {
		go readCommands(ctx, cancel, in, out, client, run)
	}
	return followRun(ctx, cmd, dir, run.RunID)
}

// followRun renders one run's stream. It is `rein tail <run>` with the flags
// fixed, deliberately: one renderer, and a run looks the same whether or not
// anybody is driving it.
func followRun(ctx context.Context, cmd *cobra.Command, home, ref string) error {
	store, err := runlog.Open(home)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	tty, width := false, tail.DefaultWidth
	if stdout, ok := out.(*os.File); ok {
		tty, width = tail.Terminal(stdout)
	}
	err = tail.Run(ctx, tail.Options{
		Store: store,
		Ref:   ref,
		Out:   out,
		TTY:   tty,
		Width: width,
		Color: tty && os.Getenv("NO_COLOR") == "",
	})
	if errors.Is(err, runlog.ErrNotFound) {
		// Attached, but with no log to read: an older runner, or a run whose
		// first record has not landed. Not a failure of the attach.
		fmt.Fprintln(out, "(no run log to follow — this runner is not writing one)")
		<-ctx.Done()
		return nil
	}
	return err
}

// readCommands is the person's half of the conversation: one line at a time,
// no raw mode.
//
// Line-oriented rather than a PTY because there is no terminal to broker. A
// vendor CLI in stream-json mode is a pipe, and every adapter that takes input
// at all takes it as structured text through Send — so keystrokes have nowhere
// to go, and the shell's own line editing is better than anything a takeover
// would reimplement.
func readCommands(ctx context.Context, stop context.CancelFunc, in io.Reader, out io.Writer,
	client *control.Client, run control.RunInfo) {

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			if done := attachCommand(line, out, client, run); done {
				stop()
				return
			}
			continue
		}
		if !run.Interactive {
			fmt.Fprintf(out, "! not sent: the %s adapter takes no input after a session starts.\n", run.AgentKind)
			continue
		}
		if err := client.Input(line); err != nil {
			fmt.Fprintf(out, "! %v\n", err)
			continue
		}
		fmt.Fprintln(out, "→ sent")
	}
	// EOF on stdin — Ctrl-D, or a pipe running out — is a detach.
	stop()
}

// attachCommand runs one colon command and reports whether to detach.
func attachCommand(line string, out io.Writer, client *control.Client, run control.RunInfo) bool {
	verb, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	switch verb {
	case ":detach", ":quit", ":q":
		fmt.Fprintln(out, "detaching — the daemon takes over again")
		return true

	case ":allow", ":deny":
		if !run.Approvals {
			fmt.Fprintf(out, "! the %s adapter does not honour permission answers, so there is nothing to allow or deny.\n",
				run.AgentKind)
			return false
		}
		allow := verb == ":allow"
		if err := client.Respond("", allow, rest); err != nil {
			fmt.Fprintf(out, "! %v\n", err)
			return false
		}
		if allow {
			fmt.Fprintln(out, "→ allowed")
		} else {
			fmt.Fprintln(out, "→ denied")
		}

	case ":status":
		fmt.Fprintf(out, "  run %s on %s (%s), session %s\n",
			runlog.Short(run.RunID), run.Queue, run.AgentKind, orDash(run.SessionID))
		fmt.Fprintf(out, "  input: %s   permission answers: %s\n",
			yesNo(run.Interactive), yesNo(run.Approvals))

	case ":help", ":?":
		fmt.Fprintln(out, "  :detach        leave; the daemon resumes driving")
		fmt.Fprintln(out, "  :allow  [why]  approve the permission request the agent is waiting on")
		fmt.Fprintln(out, "  :deny   [why]  refuse it")
		fmt.Fprintln(out, "  :status        what this run is and what you can do to it")
		fmt.Fprintln(out, "  anything else  is sent to the agent")

	default:
		fmt.Fprintf(out, "! unknown command %q — `:help` lists them. Send a line beginning with a colon "+
			"by putting a space in front of it.\n", verb)
	}
	return false
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
