package runner

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/worktree"
)

func TestStopCode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		code, line string
	}{
		{"init stop", fmt.Errorf("worktree: failed: %w", worktree.InitStop{Code: "repo_access", Line: "GitHub can't be reached"}), "repo_access", "GitHub can't be reached"},
		{"not logged in", fmt.Errorf("%w: claude is not logged in; run `claude auth login`", adapter.ErrPreflight), "agent_login", ""},
		{"bare not logged in", fmt.Errorf("%w: claude is not logged in", adapter.ErrPreflight), "agent_login", ""},
		{"auth status", fmt.Errorf("%w: claude auth status failed: boom", adapter.ErrPreflight), "agent_login", ""},
		{"not on PATH", fmt.Errorf("%w: %q is not on PATH: %w", adapter.ErrPreflight, "claude", exec.ErrNotFound), "other", ""},
		{"unrelated", errors.New("disk full"), "", ""},
	} {
		code, line := stopCode(tc.err)
		if code != tc.code || line != tc.line {
			t.Errorf("%s: stopCode = (%q, %q), want (%q, %q)", tc.name, code, line, tc.code, tc.line)
		}
	}
}
