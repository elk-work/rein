package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/elk-work/rein/internal/runner"
	"github.com/spf13/cobra"
)

// errConfigProblems is what `rein config check` returns when the report is
// not OK, so the process exits 1 after printing it.
var errConfigProblems = errors.New("config check: problems found")

// newConfigCommand groups commands about config.toml. ark:rein#67.
func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect config.toml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newConfigCheckCommand())
	return cmd
}

func newConfigCheckCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Say how this Rein reads config.toml, queue by queue",
		Long: `Load config.toml the way ` + "`rein run`" + ` would and report, for every queue,
the repositories its runs may name and the checkout a run that names none is
cut from. Exits 1 when the file does not load or a queue would strand its runs.

A running service asks a newly installed binary for this report, with --json,
before it restarts into it, and stays on the old version when the restart
would take repositories away from a queue. See docs/service.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(cmd)
			if err != nil {
				return err
			}
			asJSON, _ := cmd.Flags().GetBool("json")
			rep := runner.CheckConfigFile(path, cmd.Root().Version)
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
			} else if err := printConfigReport(cmd, rep); err != nil {
				return err
			}
			if !rep.OK {
				return errConfigProblems
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "print the report as JSON (what a running service reads before an upgrade)")
	return cmd
}

func printConfigReport(cmd *cobra.Command, rep runner.ConfigReport) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "config  %s\n", rep.Config)
	if rep.Error != "" {
		fmt.Fprintf(out, "error   %s\n", rep.Error)
		return nil
	}
	fmt.Fprintf(out, "repository rules  %s\n", rep.RepositoryMode)
	for _, w := range rep.Warnings {
		fmt.Fprintf(out, "\nWARNING: %s\n", w)
	}
	if len(rep.Queues) == 0 {
		fmt.Fprintln(out, "\nqueues  none — run `rein enrol`")
		return nil
	}
	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "QUEUE\tREPOSITORIES\tDEFAULT")
	for _, q := range rep.Queues {
		repos := strings.Join(q.Repositories, ", ")
		fmt.Fprintf(w, "%s\t%s\t%s\n", q.Queue, orDash(repos), orDash(q.Default))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, q := range rep.Queues {
		for _, p := range q.Problems {
			fmt.Fprintf(out, "\nPROBLEM %s: %s\n", q.Queue, p)
		}
	}
	if rep.OK {
		fmt.Fprintln(out, "\nok")
	}
	return nil
}
