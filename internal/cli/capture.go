package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/elk-work/rein/internal/capture"
)

// newCaptureCommand photographs a window of a process this run launched, or a
// page it opened. Anything else on the screen is refused.
func newCaptureCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capture",
		Short: "Photograph a window or a page this run launched",
		Long: `Photograph a window of a process this run launched, or a page it opened
in the headless Chrome Rein gives a run. The person's screen, and every
window this run did not launch, stay off limits.

  rein capture window --pid <pid> --out shot.png
  rein capture site --out shot.png https://example.com

On macOS a run also has screencapture at the front of its PATH. It accepts
"-l <windowid>" only when that window's owner is a descendant of the rein
that started the run, and it refuses a capture of the display. Calling
/usr/sbin/screencapture by its path walks around that check; don't.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newCaptureWindowCommand(), newCaptureSiteCommand(), newCaptureScreencaptureCommand())
	return cmd
}

func newCaptureWindowCommand() *cobra.Command {
	var pid int
	var out string
	cmd := &cobra.Command{
		Use:   "window",
		Short: "Photograph the main window of a process this run launched",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				return fmt.Errorf("rein capture window needs --out")
			}
			return capture.LiveWindow(pid, out)
		},
	}
	cmd.Flags().IntVar(&pid, "pid", 0, "pid of a process this run launched")
	cmd.Flags().StringVar(&out, "out", "", "where to write the image")
	return cmd
}

func newCaptureSiteCommand() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "site <url>",
		Short: "Screenshot an http(s) page with the run's headless Chrome",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				return fmt.Errorf("rein capture site needs --out")
			}
			return capture.Site(args[0], out)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "where to write the image")
	return cmd
}

// The PATH shim's entry. Hidden because the command a person types is
// `screencapture`; this is the binary it execs.
func newCaptureScreencaptureCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "screencapture -- <args>",
		Short:  "The check in front of the system screencapture",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return capture.LiveScreencapture(args)
		},
	}
	return cmd
}
