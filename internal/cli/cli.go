// Package cli is Rein's command tree.
//
// v0 ships the tree, the flags and the help — the shape Lane B's adapters, the
// run loop and the service installer plug into — with `status` and `version`
// working and `enrol`, `run`, `attach` and `service` stubbed. A stub prints
// "not implemented in v0" and exits [ExitNotImplemented]; it never pretends to
// have done anything.
//
// Exit codes are part of the contract, because a service manager and a shell
// script both read them:
//
//	0   success
//	1   an error
//	2   not implemented in v0
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/elk-work/rein/internal/runner"
	"github.com/spf13/cobra"
)

// ExitNotImplemented is the status a v0 stub exits with. Distinct from 1 so a
// caller can tell "this command does not exist yet" from "this command ran and
// failed".
const ExitNotImplemented = 2

// notImplementedError marks a stub. Execute turns it into
// [ExitNotImplemented]; nothing else produces that code.
//
// Nothing in the tree is a stub today — `attach` was the last, and ark:rein#26
// implemented it. This is kept because the exit code is part of the published
// contract that a service manager and a shell script both read, and the next
// command to land ahead of its implementation should answer with 2 rather than
// inventing something. cli_test.go exercises it against a synthetic stub.
type notImplementedError struct {
	command string
	task    string // the Ark task that will implement it
}

func (e *notImplementedError) Error() string {
	return fmt.Sprintf("rein %s: not implemented in v0 — %s", e.command, e.task)
}

// notImplemented returns the RunE a v0 stub uses.
func notImplemented(command, task string) func(*cobra.Command, []string) error {
	return func(*cobra.Command, []string) error {
		return &notImplementedError{command: command, task: task}
	}
}

// Options are the process-level inputs Execute needs. Keeping them explicit
// lets the tests drive the whole tree without touching os.Args or the real
// streams.
type Options struct {
	Version string
	Args    []string
	Stdout  io.Writer
	Stderr  io.Writer
}

// NewRootCommand builds the command tree. Exported for tests and for anything
// that wants to inspect the tree without running it.
func NewRootCommand(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "rein",
		Short: "The Elk runner: drive coding agents on dispatched runs",
		Long: `Rein is the Elk runner. It enrols this machine into an Elk workspace as
one named queue per agent kind, claims the runs dispatched to those queues,
drives a coding-agent session against each one in an isolated git worktree,
reports progress and the deliverable back to Elk, and recycles the worktree.

Work records live in Ark, repository 01M17TTM53XJKBZW2M9E04HHYP.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// A bare `rein` prints help and exits 0 rather than erroring: a service
		// manager that launches the binary with no arguments should get a
		// legible answer, not a usage failure.
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("rein {{.Version}}\n")

	root.PersistentFlags().String("config", "", "path to config.toml (default: $REIN_HOME/config.toml, else ~/.rein/config.toml)")

	root.AddCommand(
		newEnrolCommand(),
		newRunCommand(),
		newAttachCommand(),
		newTailCommand(),
		newStatusCommand(),
		newVersionCommand(version),
		newServiceCommand(),
		newConfigCommand(),
	)
	return root
}

// Execute runs the command tree and returns the process exit code.
func Execute(opts Options) int {
	return execute(NewRootCommand(opts.Version), opts)
}

// execute runs a prepared tree. Split from [Execute] so a test can put a
// command in the tree that [NewRootCommand] does not build.
func execute(root *cobra.Command, opts Options) int {
	root.SetOut(opts.Stdout)
	root.SetErr(opts.Stderr)
	root.SetArgs(opts.Args)

	err := root.Execute()
	if err == nil {
		return 0
	}
	if errors.Is(err, runner.ErrRestartForUpgrade) {
		return runner.RestartExitCode
	}
	var ni *notImplementedError
	if errors.As(err, &ni) {
		fmt.Fprintln(opts.Stderr, ni.Error())
		return ExitNotImplemented
	}
	fmt.Fprintln(opts.Stderr, "rein:", err)
	return 1
}
