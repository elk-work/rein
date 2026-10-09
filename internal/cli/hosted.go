package cli

import (
	"github.com/elk-work/rein/internal/hosted"
	"github.com/spf13/cobra"
)

func newHostedCredentialCommand() *cobra.Command {
	return &cobra.Command{Use: "hosted-credential operation", Hidden: true, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return hosted.Credential(args[0], cmd.InOrStdin(), cmd.OutOrStdout())
		}}
}
