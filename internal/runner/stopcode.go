package runner

import (
	"errors"
	"os/exec"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/worktree"
)

// stopCode names why a run stopped before it started, for Elk to group runs
// stopped for one reason. line is the bootstrap's own one-line cause, when it
// named one. An error it cannot place gives no code.
func stopCode(err error) (code, line string) {
	var stop worktree.InitStop
	switch {
	case errors.As(err, &stop):
		return stop.Code, stop.Line
	case !errors.Is(err, adapter.ErrPreflight):
		return "", ""
	case errors.Is(err, exec.ErrNotFound):
		return "other", ""
	}
	// agentSetupFailure's sniff, plus a bare "not logged in".
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "login") || strings.Contains(text, "logged in") || strings.Contains(text, "auth") {
		return "agent_login", ""
	}
	return "other", ""
}
