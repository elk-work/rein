package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newVersionCommand prints the version stamped in at build time.
func newVersionCommand(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "rein", version)
			return nil
		},
	}
}
