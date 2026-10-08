package runner

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

func TestAgentSetupFailureDoesNotForwardOutput(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("missing: %w", exec.ErrNotFound), "Missing agent capability"},
		{errors.New("auth failed: sensitive vendor output"), "Agent authentication required"},
		{errors.New("unexpected output: sensitive vendor output"), "The agent is not ready on this machine"},
	} {
		if got := agentSetupFailure(tc.err); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}
