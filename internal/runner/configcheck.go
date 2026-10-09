package runner

import (
	"errors"
	"fmt"
	"sort"

	"github.com/elk-work/rein/internal/config"
)

// ConfigCheckSchema is the version of [ConfigReport]'s JSON shape. A running
// service reads a newly installed binary's report before restarting into it
// (upgrade.go), so the shape is a contract between releases: add fields, never
// rename or repurpose one, and bump this only for a change an older reader
// must refuse to interpret.
const ConfigCheckSchema = 1

// ConfigReport is what `rein config check --json` prints: how this build reads
// a config file, queue by queue (ark:rein#67).
type ConfigReport struct {
	// Schema is [ConfigCheckSchema]. A reader that finds none was not talking
	// to a config check at all.
	Schema int `json:"schema"`
	// Version is the Rein that wrote the report.
	Version string `json:"version,omitempty"`
	// Config is the file that was read.
	Config string `json:"config,omitempty"`
	// OK is false when the file does not load, or when a queue has a problem.
	OK bool `json:"ok"`
	// Error is why the file does not load. A service handed this config would
	// not start.
	Error string `json:"error,omitempty"`
	// RepositoryMode is unscoped, compatibility or scoped — see
	// [config.Config.RepositoryMode].
	RepositoryMode string `json:"repository_mode,omitempty"`
	// Warnings are worth fixing but strand nothing.
	Warnings []string `json:"warnings,omitempty"`
	// Queues is every queue in the file, in file order.
	Queues []QueueReport `json:"queues"`
}

// QueueReport is one queue as a build reads it.
type QueueReport struct {
	// Queue is the label local commands take: workspace/name on a
	// multi-workspace machine, else the name.
	Queue     string `json:"queue"`
	Workspace string `json:"workspace"`
	Name      string `json:"name"`
	// Repositories are the [repos] keys a run on this queue may name. Never
	// null: an empty list is the answer "none".
	Repositories []string `json:"repositories"`
	// Default is the checkout a run that names no repository is cut from, or
	// empty when such a run would end stuck.
	Default string `json:"default,omitempty"`
	// Problems are what strand this queue's runs.
	Problems []string `json:"problems,omitempty"`
}

// CheckConfigFile loads a config file and reports how this build reads it.
// A file that does not load is reported, not returned as an error: that is
// the most important answer the check gives.
func CheckConfigFile(path, version string) ConfigReport {
	cfg, err := config.LoadFile(path)
	if err != nil {
		if errors.Is(err, config.ErrNotExist) {
			err = fmt.Errorf("no config at %s", path)
		}
		return ConfigReport{Schema: ConfigCheckSchema, Version: version, Config: path, Error: err.Error(), Queues: []QueueReport{}}
	}
	r := CheckConfig(cfg, version)
	r.Config = path
	return r
}

// CheckConfig reports how this build reads an already loaded config.
func CheckConfig(cfg config.Config, version string) ConfigReport {
	r := ConfigReport{
		Schema:         ConfigCheckSchema,
		Version:        version,
		OK:             true,
		RepositoryMode: cfg.RepositoryMode(),
		Queues:         []QueueReport{},
	}
	if w := cfg.RepositoryCompatibilityWarning(); w != "" {
		r.Warnings = append(r.Warnings, w)
	}
	for _, q := range cfg.Queues {
		qr := QueueReport{
			Queue:        cfg.QueueLabel(q),
			Workspace:    cfg.WorkspaceFor(q),
			Name:         q.Name,
			Repositories: []string{},
		}
		for name := range cfg.RepositoriesFor(q) {
			qr.Repositories = append(qr.Repositories, name)
		}
		sort.Strings(qr.Repositories)
		// What a run with no repo line and no github.com URL gets: the queue's
		// own repo, then default_repo, by the same code a claimed run uses.
		res, err := ResolveRepo(cfg, q, "")
		if err == nil {
			qr.Default = res.Path
		}
		switch {
		// An unscoped queue with nothing mapped can still be handed a path,
		// so only a scoped one is certain to strand.
		case cfg.RepositoryScopeRequired(q) && len(qr.Repositories) == 0 && qr.Default == "":
			qr.Problems = append(qr.Problems, fmt.Sprintf(
				"no repository: workspace %q declares none for this queue and it has no default, so every run on it would end stuck",
				qr.Workspace))
		case q.Repo != "" && err != nil:
			qr.Problems = append(qr.Problems, fmt.Sprintf(
				"repo %q is not declared for workspace %q, so every run that names no repository would end stuck",
				q.Repo, qr.Workspace))
		case q.IsWrangler() && qr.Default == "":
			qr.Problems = append(qr.Problems,
				"no default repository: a Wrangler cycle names none, so every cycle would end stuck; set repo on this queue")
		}
		if len(qr.Problems) > 0 {
			r.OK = false
		}
		r.Queues = append(r.Queues, qr)
	}
	return r
}

// strandedBy lists what a move from the running build's reading of a config
// (cur) to a newly installed build's reading of the same file (next) would
// take away from the queues being served: a file the new build cannot load,
// a queue it drops, a repository a queue can no longer use, a default a queue
// loses. Empty means the upgrade strands nothing.
//
// It compares rather than reading next.OK, so a queue that is already broken
// on the running build does not hold every later upgrade hostage — only a
// change the upgrade itself would make counts.
func strandedBy(cur, next ConfigReport) []string {
	if next.Error != "" {
		// The detail is in the log; this sentence may reach Elk.
		return []string{"it cannot load config.toml"}
	}
	type key struct{ ws, name string }
	after := make(map[key]QueueReport, len(next.Queues))
	for _, q := range next.Queues {
		after[key{q.Workspace, q.Name}] = q
	}
	var out []string
	for _, q := range cur.Queues {
		n, ok := after[key{q.Workspace, q.Name}]
		if !ok {
			out = append(out, q.Queue+" would not be served")
			continue
		}
		have := make(map[string]bool, len(n.Repositories))
		for _, name := range n.Repositories {
			have[name] = true
		}
		var lost []string
		for _, name := range q.Repositories {
			if !have[name] {
				lost = append(lost, name)
			}
		}
		switch {
		case len(q.Repositories) > 0 && len(n.Repositories) == 0:
			out = append(out, q.Queue+" would have no repositories")
		case len(lost) > 0:
			out = append(out, fmt.Sprintf("%s would lose %v", q.Queue, lost))
		}
		if q.Default != "" && n.Default == "" {
			out = append(out, q.Queue+" would have no default repository, so runs that name none would end stuck")
		}
	}
	return out
}
