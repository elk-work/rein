package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/elk-work/rein/internal/runner"
)

// superviseAtLogin is `rein run --at-login`: the job a service manager does
// elsewhere, for Windows' sign-in task (ark:scout#4306). It runs one
// supervised runner and starts it again when it exits for an upgrade or a
// config change (exit 75) or crashes, until it is told to stop. The child is
// started from this binary's PATH each time, so an upgraded rein.exe (renamed
// into place) is the one that runs next.
func superviseAtLogin(ctx context.Context, logw io.Writer, childArgs []string,
	start func(ctx context.Context, args []string) (int, error)) error {
	backoff := 10 * time.Second
	for {
		code, err := start(ctx, childArgs)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case err == nil && code == 0:
			// A clean stop (nothing to serve, or told to stop) is not a crash.
			return nil
		case err == nil && code == runner.RestartExitCode:
			fmt.Fprintln(logw, "at-login: restarting the runner (upgrade or settings change)")
			continue
		default:
			fmt.Fprintf(logw, "at-login: the runner stopped (exit %d, %v); starting it again in %s\n", code, err, backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
	}
}

// atLoginChildArgs swaps --at-login for --supervised, keeping every other
// argument the task was registered with.
func atLoginChildArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--at-login" || a == "--at-login=true" {
			out = append(out, "--supervised")
			continue
		}
		out = append(out, a)
	}
	return out
}

func startChild(ctx context.Context, args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return -1, err
	}
	c := exec.CommandContext(ctx, exe, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	err = c.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}
