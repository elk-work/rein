package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/elk-work/rein/internal/config"
)

// The repository edits `rein repos add` / `rein enrol --repo` make, shared with
// the runner, which adds the repositories chosen in the Elk app
// (ark:rein#160).

// NamedRepo is one [repos] entry: a name and its checkout's absolute path.
type NamedRepo struct{ Name, Path string }

// QueueKey names one queue in one workspace. The zero key names none.
type QueueKey struct{ Workspace, Name string }

// AddRepos returns cfg and q with each repository in [repos] and on the
// queue's own repos list. It only ever adds. A name [repos] already maps to a
// different checkout is refused unless force. cfg is not modified; the
// returned config has its own [repos] map.
//
// Declaring a list scopes the whole config (docs/workspace-repositories.md),
// so the caller must check the result strands no other queue.
func AddRepos(cfg config.Config, q config.Queue, add []NamedRepo, force bool) (config.Config, config.Queue, error) {
	repos := make(map[string]string, len(cfg.Repos)+len(add))
	for k, v := range cfg.Repos {
		repos[k] = v
	}
	cfg.Repos = repos
	q.Repos = append([]string(nil), q.Repos...)
	for _, r := range add {
		if have, ok := cfg.Repos[r.Name]; ok && filepath.Clean(expand(have)) != r.Path && !force {
			return cfg, q, fmt.Errorf("--repo %s: [repos] already maps %s to %s, and other queues may use it; pass --force to point it at %s",
				r.Name, r.Name, have, r.Path)
		}
		cfg.Repos[r.Name] = r.Path
		q.Repos = appendOnce(q.Repos, r.Name)
	}
	return cfg, q, nil
}

// KeepOthersServed returns after with every queue but skip given an explicit
// repos list wide enough to keep serving what it served under before, and
// the labels of the queues it changed. The zero skip keeps every queue.
//
// The first repos list on a machine scopes the whole config, and lists in one
// workspace combine, so declaring a list for one queue or dropping a queue
// that had one changes what the OTHER queues see (docs/workspace-repositories.md).
// A worker set up by hand with no list would lose every repository the moment
// a second agent joined with one; a worker whose short names only reached the
// full ones through a peer's list would lose those when the peer was revoked.
// Pinning each such queue to what it serves today is the edit a person would
// otherwise have had to make in config.toml by hand. It only ever adds.
func KeepOthersServed(before, after config.Config, skip QueueKey) (config.Config, []string) {
	after.Queues = append([]config.Queue(nil), after.Queues...)
	var changed []string
	for i, q := range after.Queues {
		key := QueueKey{after.WorkspaceFor(q), q.Name}
		if key == skip {
			continue
		}
		was, ok := before.QueueIn(key.Workspace, key.Name)
		if !ok {
			continue
		}
		served := after.RepositoriesFor(q)
		var missing []string
		for name := range before.RepositoriesFor(was) {
			if _, have := served[name]; !have {
				if _, declared := after.Repos[name]; declared {
					missing = append(missing, name)
				}
			}
		}
		sort.Strings(missing)
		pinned := q
		pinned.Repos = append([]string(nil), q.Repos...)
		for _, name := range missing {
			pinned.Repos = appendOnce(pinned.Repos, name)
		}
		// default_repo stops applying once the config is scoped, so a queue
		// that relied on it gets its checkout as its own repo.
		if res, err := ResolveRepo(before, was, ""); err == nil && q.Repo == "" {
			if _, err := ResolveRepo(after, pinned, ""); err != nil {
				if name := repoKeyFor(after, res.Path); name != "" {
					pinned.Repo = name
					pinned.Repos = appendOnce(pinned.Repos, name)
				}
			}
		}
		if len(pinned.Repos) == len(q.Repos) && pinned.Repo == q.Repo {
			continue
		}
		after.Queues[i] = pinned
		// Each list added widens its workspace for the queues already looked
		// at, so the loop never needs a second pass: it only ever adds.
		changed = append(changed, after.QueueLabel(pinned))
	}
	return after, changed
}

// repoKeyFor is the [repos] key that maps path, or "".
func repoKeyFor(cfg config.Config, path string) string {
	for _, k := range repoNames(cfg) {
		if filepath.Clean(expand(cfg.Repos[k])) == filepath.Clean(path) {
			return k
		}
	}
	return ""
}

// CheckReposStrandNothing refuses a change to the repository declarations
// that would still take a repository or a default away from any queue but
// skip once [KeepOthersServed] has done what it can. The queue being enrolled
// or revoked is exempt: its change is what was asked for.
func CheckReposStrandNothing(before, after config.Config, skip QueueKey) error {
	cur := CheckConfig(before, "")
	next := CheckConfig(after, "")
	kept := cur.Queues[:0:0]
	for _, q := range cur.Queues {
		if q.Workspace == skip.Workspace && q.Name == skip.Name {
			continue
		}
		kept = append(kept, q)
	}
	cur.Queues = kept
	if lost := StrandedBy(cur, next); len(lost) > 0 {
		return fmt.Errorf("this would change what other queues on this machine can work in, and then %s.\n"+
			"       Nothing was changed. `rein config check` shows how each queue reads the config",
			strings.Join(lost, "; "))
	}
	return nil
}

// CheckoutPath turns a path into an absolute one and insists it is a git
// checkout: a worktree is cut from it, and nothing else will do.
func CheckoutPath(p string) (string, error) {
	abs, err := filepath.Abs(expand(p))
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	if _, err := os.Stat(filepath.Join(abs, ".git")); err != nil {
		return "", fmt.Errorf("%s is not a git checkout (no .git)", abs)
	}
	return abs, nil
}

// CloneRepo downloads owner/name from `from` (default
// https://github.com/owner/name.git) into into/<name>, or reuses the checkout
// already there. cloned says which. Without prompt, git fails rather than ask
// for credentials on a terminal. A clone that fails leaves nothing behind, so
// a half-finished checkout is never reused.
func CloneRepo(ctx context.Context, name, from, into string, prompt bool) (dir string, cloned bool, err error) {
	if from == "" {
		if strings.Count(name, "/") != 1 {
			return "", false, fmt.Errorf("--clone needs owner/name to know where to download from, or --from")
		}
		from = "https://github.com/" + name + ".git"
	}
	dest := filepath.Join(expand(into), name[strings.LastIndex(name, "/")+1:])
	if _, err := os.Stat(dest); err == nil {
		dir, err := CheckoutPath(dest)
		return dir, false, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", false, err
	}
	c := exec.CommandContext(ctx, "git", "clone", "--quiet", from, dest)
	if !prompt {
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	}
	if out, err := c.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dest)
		return "", false, fmt.Errorf("git clone %s: %v\n%s", from, err, strings.TrimSpace(string(out)))
	}
	dir, err = CheckoutPath(dest)
	return dir, err == nil, err
}

func appendOnce(list []string, v string) []string {
	for _, have := range list {
		if have == v {
			return list
		}
	}
	return append(list, v)
}
