package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/secretenv"
)

// Clone makes a single disposable checkout, using environment credentials.
// No local repository, hooks, bootstrap script or Ark state is inherited.
func (m *Manager) Clone(ctx context.Context, repo, runID string, env map[string]string) (*Worktree, error) {
	if !config.HostedRepoName(repo) {
		return nil, errors.New("hosted repository must be owner/name")
	}
	id := SafeID(runID)
	if id == "" || m.Root == "" {
		return nil, errors.New("hosted clone needs a work root and run id")
	}
	if err := os.MkdirAll(m.Root, 0700); err != nil {
		return nil, err
	}
	dir := filepath.Join(m.Root, id)
	// An existing checkout is a refusal, never a directory to overwrite.
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		return nil, errors.New("hosted clone directory already exists or is inaccessible")
	}
	red := secretenv.NewRedactor(env)
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, m.git(), args...)
		cmd.Env = secretenv.Filter(os.Environ())
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("hosted git %s: %w: %s", args[0], err, red.String(trimOutput(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	url := "https://github.com/" + repo + ".git"
	if _, err := run("clone", "--depth", "50", "--", url, dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	base, err := run("-C", dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return nil, err
	}
	sha, err := run("-C", dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	branch := BranchPrefix + id
	if _, err := run("-C", dir, "checkout", "-b", branch); err != nil {
		return nil, err
	}
	ok = true
	return &Worktree{Dir: dir, Repo: dir, Branch: branch, RunID: runID, BaseRef: base, BaseSHA: sha,
		BaseNote: "the freshly cloned remote default branch", Hosted: true,
		Ark: ArkState{Note: "a hosted clone has no local Ark records; do not run ark init"}}, nil
}
