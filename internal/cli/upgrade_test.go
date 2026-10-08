package cli

import (
	"fmt"
	"github.com/elk-work/rein/internal/runner"
	"github.com/spf13/cobra"
	"io"
	"testing"
)

func TestUpgradeExitCode(t *testing.T) {
	root := &cobra.Command{Use: "test", RunE: func(*cobra.Command, []string) error { return fmt.Errorf("wrapped: %w", runner.ErrRestartForUpgrade) }}
	if got := execute(root, Options{Stdout: io.Discard, Stderr: io.Discard}); got != 75 {
		t.Fatalf("exit = %d", got)
	}
}
