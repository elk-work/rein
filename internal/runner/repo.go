package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/elk-work/rein/internal/config"
)

// Elk's work order names no repository.
//
// This is worth stating plainly because it is the single biggest gap between
// what a runner needs and what the queue contract provides. `claim_run`
// returns the handoff packet, the action's identity, the requester, the camp
// and goal context, prior deliverables, sibling runs, a tool map and the
// reporting contract — and nothing that says which repository the work is in.
// `HandshakePacket` has no repo field, `links[]` is a submit-side concept, and
// the only `links` a claim carries is a COUNT on sibling runs.
//
// So Rein has to resolve it, and it does so from four places in order, each
// more specific than the last is general:
//
//  1. an explicit `repo:` line in the work order — the one a person can write
//     into a Do direction and have work;
//  2. a github.com URL anywhere in the work order, reduced to `owner/name`;
//  3. the queue's own `repo` in config.toml;
//  4. `default_repo` in config.toml.
//
// A name from (1) or (2) is looked up in the config's `[repos]` table, then by
// its last path segment, and finally treated as a path if one exists on
// legacy single-workspace configs. Scoped configs require an exact declared
// name or mapped checkout path. A
// resolution that fails is not a guess: the run is submitted `stuck` naming
// what it looked for and how to tell it, because a runner that picks the wrong
// repository does the work in the wrong place and reports success.

// repoHintRE matches an explicit hint line: "repo: elk-work/scout",
// "Repo: scout", "repository: ~/dev/scout". Anchored to a line start so a
// sentence mentioning the word does not become a hint.
// The asterisk runs are for the shapes a person actually writes in a Do
// direction, all of which mean the same thing: `repo: x`, `**repo**: x`,
// `**Repo:** x`.
var repoHintRE = regexp.MustCompile(`(?mi)^\s*\*{0,2}(?:repo|repository)\*{0,2}\s*[:=]\*{0,2}\s*` +
	"`?" + `([^\s` + "`" + `*]+)` + "`?" + `\**\s*$`)

// githubURLRE matches a GitHub repository URL and captures owner/name. The
// trailing group tolerates the rest of a URL — /pull/12, .git, a trailing
// slash — because a work order cites a PR far more often than a bare repo.
var githubURLRE = regexp.MustCompile(`https?://(?:www\.)?github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?(?:[/#?]|\s|$)`)

// RepoSource records which of the four places an answer came from, so a log
// line and a `stuck` reason can both say why this repository and not another.
type RepoSource string

const (
	RepoFromHint    RepoSource = "a repo: line in the work order"
	RepoFromURL     RepoSource = "a github.com URL in the work order"
	RepoFromQueue   RepoSource = "the queue's configured repo"
	RepoFromDefault RepoSource = "default_repo in the config"
)

// RepoResolution is where a run will be cut from.
type RepoResolution struct {
	// Path is the local checkout.
	Path string
	// Name is what the work order called it, when it called it anything.
	Name string
	// Source says which rule matched.
	Source RepoSource
}

func (r RepoResolution) String() string {
	if r.Name != "" && r.Name != r.Path {
		return fmt.Sprintf("%s (%s, from %s)", r.Path, r.Name, r.Source)
	}
	return fmt.Sprintf("%s (from %s)", r.Path, r.Source)
}

// UnresolvedRepoError reports that Rein could not tell which repository a run
// belongs in. Its message is written to be pasted into a `stuck` deliverable:
// it says what it looked for, what it found, and the two ways to fix it.
type UnresolvedRepoError struct {
	// Name is what the work order named, or "" when it named nothing.
	Name string
	// Queue is the queue the run arrived on.
	Queue string
	// Known is the config's `[repos]` keys, so the reader can see the options.
	Known []string
}

func (e *UnresolvedRepoError) Error() string {
	var b strings.Builder
	if e.Name != "" {
		fmt.Fprintf(&b, "this run names repository %q, and this machine has no checkout mapped to that name.", e.Name)
	} else {
		b.WriteString("this run does not say which repository it is in, and Elk's work order carries no repository field.")
	}
	b.WriteString("\n\nTwo ways to fix it, either is enough:\n\n")
	fmt.Fprintf(&b, "1. Put a line in the Do direction: `repo: <name or path>` — or cite a github.com URL.\n")
	fmt.Fprintf(&b, "2. Give the queue a default in this machine's ~/.rein/config.toml:\n\n")
	fmt.Fprintf(&b, "       [[queues]]\n       name = %q\n       repo = \"/path/to/checkout\"\n", e.Queue)
	if len(e.Known) > 0 {
		fmt.Fprintf(&b, "\nNames this machine already knows: %s.\n", strings.Join(e.Known, ", "))
	}
	return b.String()
}

// ResolveRepo picks the repository for a work order. See the commentary at the
// top of this file for the order and why there is one at all.
func ResolveRepo(cfg config.Config, q config.Queue, workOrder string) (RepoResolution, error) {
	scoped := cfg.RepositoryScopeRequired(q)
	cfg.Repos = cfg.RepositoriesFor(q)
	if name := hintedRepo(workOrder); name != "" {
		path, ok := lookupRepoScoped(cfg, name, scoped)
		if !ok {
			if scoped {
				return RepoResolution{}, fmt.Errorf("repository %q is not declared for queue %q in workspace %q", name, q.Name, cfg.WorkspaceFor(q))
			}
			return RepoResolution{}, &UnresolvedRepoError{Name: name, Queue: q.Name, Known: repoNames(cfg)}
		}
		return RepoResolution{Path: path, Name: name, Source: RepoFromHint}, nil
	}
	if name := githubRepo(workOrder); name != "" {
		if path, ok := lookupRepoScoped(cfg, name, scoped); ok {
			return RepoResolution{Path: path, Name: name, Source: RepoFromURL}, nil
		}
		return RepoResolution{}, fmt.Errorf("repository %q is not declared for queue %q in workspace %q", name, q.Name, cfg.WorkspaceFor(q))
	}
	if q.Repo != "" {
		if scoped {
			if path, ok := lookupRepoScoped(cfg, q.Repo, true); ok {
				return RepoResolution{Path: path, Source: RepoFromQueue}, nil
			}
			return RepoResolution{}, fmt.Errorf("queue repository is not declared for workspace %q", cfg.WorkspaceFor(q))
		}
		return RepoResolution{Path: expand(q.Repo), Source: RepoFromQueue}, nil
	}
	if cfg.DefaultRepo != "" && !scoped {
		return RepoResolution{Path: expand(cfg.DefaultRepo), Source: RepoFromDefault}, nil
	}
	return RepoResolution{}, &UnresolvedRepoError{Name: githubRepo(workOrder), Queue: q.Name, Known: repoNames(cfg)}
}

func hintedRepo(text string) string {
	m := repoHintRE.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func githubRepo(text string) string {
	m := githubURLRE.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2]
}

// lookupRepo turns a name from a work order into a local path: the `[repos]`
// table first by exact key, then case-insensitively, then by last path
// segment ("elk-work/scout" finds a `scout` entry), and finally the name
// itself if it is a directory that exists.
func lookupRepoScoped(cfg config.Config, name string, scoped bool) (string, bool) {
	if scoped {
		for k, v := range cfg.Repos {
			if strings.EqualFold(k, name) || expand(v) == expand(name) {
				return expand(v), true
			}
		}
		return "", false
	}
	if p, ok := cfg.Repos[name]; ok {
		return expand(p), true
	}
	lower := strings.ToLower(name)
	for k, v := range cfg.Repos {
		if strings.ToLower(k) == lower {
			return expand(v), true
		}
	}
	if _, base, ok := strings.Cut(name, "/"); ok {
		for k, v := range cfg.Repos {
			if strings.EqualFold(k, base) || strings.EqualFold(lastSegment(k), base) {
				return expand(v), true
			}
		}
	}
	if p := expand(name); looksLikePath(name) {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

func looksLikePath(name string) bool {
	return strings.HasPrefix(name, "/") || strings.HasPrefix(name, "~") ||
		strings.HasPrefix(name, ".") || filepath.IsAbs(name)
}

func lastSegment(s string) string {
	if _, after, ok := strings.Cut(s, "/"); ok {
		return after
	}
	return s
}

func repoNames(cfg config.Config) []string {
	out := make([]string, 0, len(cfg.Repos))
	for k := range cfg.Repos {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// expand resolves a leading ~ so a config written by a person works.
func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// ResolveHostedRepo considers only explicit hints and the queue fallback.
func ResolveHostedRepo(cfg config.Config, q config.Queue, text string) (RepoResolution, error) {
	name, source := hintedRepo(text), RepoFromHint
	if name == "" {
		name, source = q.Repo, RepoFromQueue
	}
	if !config.HostedRepoName(name) {
		return RepoResolution{}, fmt.Errorf("hosted repository must be owner/name from repo: or the queue repo")
	}
	for _, allow := range cfg.Hosted.AllowRepos {
		if name == allow {
			return RepoResolution{Name: name, Source: source}, nil
		}
	}
	return RepoResolution{}, fmt.Errorf("hosted repository %q is outside hosted.allow_repos", name)
}
