package grok

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// preflightSignIns checks the existing logins without reading credentials or
// reporting command output (which can contain account details and private URLs).
// credential list is an authenticated service request; ark status only checks
// local repository state and cannot prove that a login is present or valid.
func preflightSignIns(ctx context.Context, spec adapter.RunSpec) error {
	for _, check := range []struct {
		name, binary string
		args         []string
	}{
		{"GitHub", "gh", []string{"auth", "status"}},
		{"Ark", "ark", []string{"credential", "list"}},
	} {
		checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(checkCtx, check.binary, check.args...)
		cmd.Dir = spec.WorktreeDir
		cmd.Env = mergeEnv(spec.InheritedEnv(), spec.Env)
		err := cmd.Run()
		cancel()
		if err != nil {
			return fmt.Errorf("%w: Grok %s sign-in check failed; ensure %s is installed and signed in for the runner user and repository", adapter.ErrPreflight, check.name, check.binary)
		}
	}
	return nil
}
