package runner

import (
	"errors"
	"os/exec"
	"regexp"
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
	case errors.Is(err, adapter.ErrNoProviderKey):
		// The gemini adapter's missing Google key: the person's own
		// credential, so a sign-in in all but name.
		return "agent_login", ""
	}
	// agentSetupFailure's sniff, plus a bare "not logged in".
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "login") || strings.Contains(text, "logged in") || strings.Contains(text, "auth") {
		return "agent_login", ""
	}
	return "other", ""
}

// Only an explicit declaration at the start of the final account is a stop.
// Never scan the work order, tool output, or quoted examples for this marker.
var wrongRepoDeclaration = regexp.MustCompile(`\Astop_code: wrong_repo\r?\nrepo: ([A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*)(?:\r?\n|$)`)

func declaresWrongRepo(account string, repo RepoResolution) bool {
	m := wrongRepoDeclaration.FindStringSubmatch(strings.TrimSpace(account))
	return m != nil && !strings.EqualFold(m[1], repo.Name)
}
