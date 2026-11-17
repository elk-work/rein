package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runner"
)

// enrolRepos is what `--repo` and `--default-repo` ask of an enrolment
// (ark:rein#82). Without them a fresh enrolment leaves the queue online in Elk
// with no repository, and every run it claims ends stuck at preflight — which
// is exactly what the Mac app's "Run a worker on this Mac" produced, because
// it had nothing to pass.
type enrolRepos struct {
	// Repos are name → absolute checkout path, in flag order.
	Repos []namedRepo
	// Default is --default-repo as given: a name or a path.
	Default string
}

type namedRepo = runner.NamedRepo

func (r enrolRepos) empty() bool { return len(r.Repos) == 0 && r.Default == "" }

// parseEnrolRepos reads the flags and checks every path before anything is
// sent to Elk, so a typo stops the enrolment rather than the first run.
func parseEnrolRepos(flags []string, def string) (enrolRepos, error) {
	var out enrolRepos
	seen := map[string]string{}
	for _, f := range flags {
		name, path, ok := strings.Cut(f, "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if !ok || name == "" || path == "" {
			return enrolRepos{}, fmt.Errorf("--repo %q: want name=path, e.g. --repo elk-work/scout=~/dev/scout", f)
		}
		if strings.ContainsAny(name, " \t\n\"") {
			return enrolRepos{}, fmt.Errorf("--repo %q: a repository name has no spaces or quotes", f)
		}
		abs, err := runner.CheckoutPath(path)
		if err != nil {
			return enrolRepos{}, fmt.Errorf("--repo %s: %w", name, err)
		}
		if prev, dup := seen[name]; dup && prev != abs {
			return enrolRepos{}, fmt.Errorf("--repo %s is given twice, as %s and %s", name, prev, abs)
		}
		if _, dup := seen[name]; !dup {
			seen[name] = abs
			out.Repos = append(out.Repos, namedRepo{Name: name, Path: abs})
		}
	}
	out.Default = strings.TrimSpace(def)
	return out, nil
}

// applyEnrolRepos returns cfg and q with the requested repositories declared:
// each --repo goes into the top-level [repos] and onto the queue's own repos
// list, and --default-repo becomes the queue's repo. It only ever adds, so a
// re-enrolment keeps what the queue already served. cfg is not modified; the
// returned config has its own [repos] map.
//
// Declaring a list scopes the whole config (docs/workspace-repositories.md),
// so the caller must check the result strands no other queue.
func applyEnrolRepos(cfg config.Config, q config.Queue, req enrolRepos, force bool) (config.Config, config.Queue, error) {
	if req.empty() {
		return cfg, q, nil
	}
	cfg, q, err := runner.AddRepos(cfg, q, req.Repos, force)
	if err != nil {
		return cfg, q, err
	}

	switch def := req.Default; {
	case def != "":
		name, err := defaultRepoName(cfg, def)
		if err != nil {
			return cfg, q, err
		}
		q.Repo = name
		q.Repos = appendOnce(q.Repos, name)
	case q.Repo == "" && len(q.Repos) == 1:
		// One repository and no default: a run that names none can only
		// mean that one, and leaving it unset would strand it for nothing.
		q.Repo = q.Repos[0]
	}
	return cfg, q, nil
}

// defaultRepoName resolves --default-repo to a [repos] key: a key itself, or
// the path one maps. A path nothing maps needs a name, which only --repo gives.
func defaultRepoName(cfg config.Config, def string) (string, error) {
	if _, ok := cfg.Repos[def]; ok {
		return def, nil
	}
	if abs, err := runner.CheckoutPath(def); err == nil {
		keys := make([]string, 0, len(cfg.Repos))
		for k := range cfg.Repos {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if filepath.Clean(expandHome(cfg.Repos[k])) == abs {
				return k, nil
			}
		}
		return "", fmt.Errorf("--default-repo %s is not mapped to a name; declare it too with --repo <name>=%s", abs, abs)
	}
	return "", fmt.Errorf("--default-repo %q is neither a repository name from --repo or [repos] nor a git checkout", def)
}

// backupConfig copies the config file beside itself before Rein edits a
// queue the person did not name, so the edit can be undone by hand.
func backupConfig(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	backup := path + ".bak"
	if err := os.WriteFile(backup, data, 0o600); err != nil {
		return "", fmt.Errorf("could not back up %s before changing other queues: %w", path, err)
	}
	return backup, nil
}

func appendOnce(list []string, v string) []string {
	for _, have := range list {
		if have == v {
			return list
		}
	}
	return append(list, v)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// planEnrolment is the config and queue an enrolment will write: the queue
// filed under workspace with its agent kind, keeping every local setting a
// re-enrolment had, plus the requested repositories, and every other queue
// pinned to what it already serves (the labels of those come back too). It
// is computed once before Elk is called, when the workspace is known, so a bad
// --repo stops the enrolment before anything is bound, and again with Elk's
// answer.
func planEnrolment(cfg config.Config, workspace, queue, agentKind string, req enrolRepos, force bool) (config.Config, config.Queue, []string, error) {
	cfg.Workspace = firstNonEmpty(cfg.Workspace, workspace)
	cfg.Queues = append([]config.Queue(nil), cfg.Queues...)
	before := cfg
	q, _ := cfg.QueueIn(workspace, queue)
	q.Name = queue
	q.AgentKind = agentKind
	if workspace != cfg.Workspace {
		q.Workspace = workspace
	}
	next, q, err := applyEnrolRepos(cfg, q, req, force)
	if err != nil {
		return cfg, q, nil, err
	}
	next.Queues = append([]config.Queue(nil), next.Queues...)
	next.SetQueue(q)
	var pinned []string
	if !req.empty() {
		skip := runner.QueueKey{Workspace: workspace, Name: queue}
		next, pinned = runner.KeepOthersServed(before, next, skip)
		if err := runner.CheckReposStrandNothing(before, next, skip); err != nil {
			return cfg, q, nil, err
		}
		if err := next.Validate(); err != nil {
			return cfg, q, nil, err
		}
	}
	return next, q, pinned, nil
}

// planRevoke is the config a revoke writes: the queue gone, and every other
// queue pinned to what it served with it there, so dropping one queue never
// shrinks another (a revoke once left a queue with only its short repository
// names, and the running service refused to reload).
func planRevoke(cfg config.Config, workspace, queue string) (config.Config, []string, error) {
	before := cfg
	next := cfg
	next.Queues = append([]config.Queue(nil), cfg.Queues...)
	next.RemoveQueue(workspace + "/" + queue)
	skip := runner.QueueKey{Workspace: workspace, Name: queue}
	var held []string
	if before.RepositoryMode() == "scoped" && next.RepositoryMode() != "scoped" {
		// The revoked queue held the last list, so the config would fall back
		// to unscoped and every queue would reach all of [repos] — another
		// workspace's checkouts included. Each queue that served something
		// keeps exactly that as its own list, which keeps the config scoped.
		next.Queues = append([]config.Queue(nil), next.Queues...)
		for i, q := range next.Queues {
			was, _ := before.QueueIn(next.WorkspaceFor(q), q.Name)
			served := make([]string, 0)
			for name := range before.RepositoriesFor(was) {
				served = append(served, name)
			}
			if len(served) == 0 {
				continue
			}
			sort.Strings(served)
			next.Queues[i].Repos = served
			held = append(held, next.QueueLabel(q))
		}
	}
	next, pinned := runner.KeepOthersServed(before, next, skip)
	for _, label := range held {
		pinned = appendOnce(pinned, label)
	}
	if err := runner.CheckReposStrandNothing(before, next, skip); err != nil {
		return cfg, nil, err
	}
	return next, pinned, nil
}

// printPinned says which other queues an enrolment or revoke gave an explicit
// repos list, and where the file was before.
func printPinned(out io.Writer, pinned []string, backup string) {
	if len(pinned) == 0 {
		return
	}
	fmt.Fprintf(out, "kept    %s on the repositories they already work in (each now lists them in config.toml)\n",
		strings.Join(pinned, ", "))
	if backup != "" {
		fmt.Fprintf(out, "backup  %s\n", backup)
	}
}

// printEnrolRepos says what a run on the queue can be cut from, and says it
// loudly when the answer is nothing.
func printEnrolRepos(out io.Writer, cfg config.Config, q config.Queue) {
	names := make([]string, 0)
	for name := range cfg.RepositoriesFor(q) {
		names = append(names, name)
	}
	sort.Strings(names)
	def := "—"
	if res, err := runner.ResolveRepo(cfg, q, ""); err == nil {
		def = res.Path
	}
	if len(names) == 0 && def == "—" {
		fmt.Fprintln(out, "repos   NONE — every run this queue claims that names no checkout path will end stuck.")
		fmt.Fprintln(out, "        Re-run with --force --repo <name>=<path> [--default-repo <name>] to declare them.")
		return
	}
	fmt.Fprintf(out, "repos   %s\n", orDash(strings.Join(names, ", ")))
	fmt.Fprintf(out, "default %s\n", def)
}
