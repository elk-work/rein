package runner

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
)

// openPersonalFolders lists each of the person's own folders once when any
// queue this process serves allows them, and reports which it opened and
// which macOS (or anything else) refused. Listing is what makes macOS ask, so
// doing it as the daemon starts puts the question there rather than in the
// middle of a run (Elk #1832). The caller runs it off the start path: macOS
// can hold the listing until the person answers.
func openPersonalFolders(queues []config.Queue, home func() (string, error)) (opened, refused []string) {
	allowed := false
	for _, q := range queues {
		allowed = allowed || q.PersonalFolders
	}
	if !allowed {
		return nil, nil
	}
	h, err := home()
	if err != nil || h == "" {
		return nil, nil
	}
	for _, folder := range adapter.PersonalFolders {
		path := filepath.Join(h, folder)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		if _, err := os.ReadDir(path); err != nil {
			refused = append(refused, folder)
		} else {
			opened = append(opened, folder)
		}
	}
	return opened, refused
}

// openRepositoryFolders opens each checkout the queues this process serves
// may cut a run from, once, when the daemon starts, and reports which it
// opened and which it could not read. A repository the person keeps in
// Documents (or any other guarded folder) is theirs to use whatever the
// personal-folders choice, and the agent's deny rules leave it open; but the
// first `git worktree add` at a claim would otherwise be what makes macOS
// ask, out of the blue, in the middle of a run (follow-up 9 of the review on
// rein#86). Listing the checkout and its `.git` here puts the question at
// start, next to the personal folders' own. Like openPersonalFolders, the
// caller runs it off the start path.
func openRepositoryFolders(cfg config.Config, queues []config.Queue) (opened, refused []string) {
	seen := map[string]bool{}
	for _, path := range repositoryPaths(cfg, queues) {
		if seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		if _, err := os.ReadDir(path); err != nil {
			refused = append(refused, path)
			continue
		}
		// `.git` is a directory in a checkout and a file in a worktree; both
		// are read by `git worktree add`.
		if _, err := os.Stat(filepath.Join(path, ".git")); err != nil && !os.IsNotExist(err) {
			refused = append(refused, path)
			continue
		}
		opened = append(opened, path)
	}
	return opened, refused
}

// repositoryPaths is every local checkout a run of these queues can be cut
// from, in the order ResolveRepo would reach them: the queue's repository
// map, a queue `repo` given as a path, and the config's default repository
// when the config is unscoped. Names are expanded, never resolved twice.
func repositoryPaths(cfg config.Config, queues []config.Queue) []string {
	var paths []string
	for _, q := range queues {
		repos := cfg.RepositoriesFor(q)
		names := make([]string, 0, len(repos))
		for name := range repos {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			paths = append(paths, expand(repos[name]))
		}
		if q.Repo != "" && looksLikePath(q.Repo) {
			paths = append(paths, expand(q.Repo))
		}
		if cfg.DefaultRepo != "" && looksLikePath(cfg.DefaultRepo) && !cfg.RepositoryScopeRequired(q) {
			paths = append(paths, expand(cfg.DefaultRepo))
		}
	}
	return paths
}
