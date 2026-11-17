package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	geminiadapter "github.com/elk-work/rein/internal/adapter/gemini"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
)

// ManagedSecretService is the keychain service the Elk app's secrets live
// under: `rein secret set NAME` stores a value at (elk-secret, NAME), and a
// name chosen in the app maps to "elk-secret/NAME" in the queue's secrets map.
// Entries under any other service were written by a person and are never
// touched by Elk's settings.
const ManagedSecretService = "elk-secret"

// ApplyElkSettings folds what the worker's person chose in the Elk app into
// one queue's config (scout 0599, ark:scout#4308). It reports whether anything
// changed, and refuses — changing nothing — a result the config would reject,
// so a bad choice in the app can never leave a file this computer cannot load.
//
//   - Land: set when Elk names one; nil leaves the queue's own.
//   - Permission: read_only or full when Elk names one; nil leaves the queue's
//     own. Anything else is refused.
//   - Secrets: the managed entries become exactly the names Elk sent. A name
//     the person already mapped to their own keychain item keeps that item.
func ApplyElkSettings(cfg config.Config, workspace, queue string, s elk.WorkerSettings) (config.Config, bool, error) {
	q, ok := cfg.QueueIn(workspace, queue)
	if !ok {
		return cfg, false, fmt.Errorf("no queue %s/%s in the config", workspace, queue)
	}
	next := q
	if s.Land != nil {
		land := strings.ToLower(strings.TrimSpace(*s.Land))
		switch land {
		case "":
		case config.LandMerge, config.LandPR, config.LandBranch:
			next.Land = land
		default:
			return cfg, false, fmt.Errorf("Elk sent landing policy %q; want merge, pr or branch", *s.Land)
		}
	}

	if s.Permission != nil {
		switch p := strings.ToLower(strings.TrimSpace(*s.Permission)); p {
		case "":
		case "read_only", "full":
			next.PermissionMode = p
		default:
			return cfg, false, fmt.Errorf("Elk sent permission %q; want read_only or full", *s.Permission)
		}
	}

	secrets := map[string]string{}
	for name, item := range q.Secrets {
		if !strings.HasPrefix(item, ManagedSecretService+"/") {
			secrets[name] = item
		}
	}
	for _, name := range s.Secrets {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		// The Google key is the gemini adapter's own, read for gemini runs
		// alone (gemini.go) and never a queue secret: a queue naming it is
		// refused, so taking it here would make every later choice from the
		// app fail too.
		if name == geminiadapter.KeyName {
			continue
		}
		if _, mine := secrets[name]; !mine {
			secrets[name] = ManagedSecretService + "/" + name
		}
	}
	if len(secrets) > 0 && len(q.InheritEnv) > 0 && !sameSecrets(secrets, q.Secrets) {
		return cfg, false, errors.New("this queue inherits variables by name (inherit_env), which a secrets map replaces; " +
			"remove inherit_env from config.toml to let the Elk app choose its secrets")
	}
	if len(secrets) == 0 {
		secrets = nil
	}
	next.Secrets = secrets

	if next.Land == q.Land && next.PermissionMode == q.PermissionMode && sameSecrets(next.Secrets, q.Secrets) {
		return cfg, false, nil
	}
	out := cfg
	out.Queues = append([]config.Queue(nil), cfg.Queues...)
	out.SetQueue(next)
	if err := out.Validate(); err != nil {
		return cfg, false, fmt.Errorf("the settings chosen in Elk would not load here: %w", err)
	}
	return out, true, nil
}

// AddElkRepos gives one queue each repository (owner/name) the worker's person
// chose for it in the Elk app (ark:rein#160). It only ever adds: a repository
// missing from wanted is never taken away. For each one the queue does not
// already serve, a [repos] entry for it is used when there is one (its key, or
// a checkout whose origin it is); otherwise clone fetches it and it is declared
// as owner/name. A clone that fails skips that repository alone; clone
// reports it. Every queue keeps what it served, so a running service will
// restart into the result.
func AddElkRepos(cfg config.Config, workspace, queue string, wanted []string, clone func(name string) (string, error)) (config.Config, bool, error) {
	q, ok := cfg.QueueIn(workspace, queue)
	if !ok {
		return cfg, false, fmt.Errorf("no queue %s/%s in the config", workspace, queue)
	}
	served := cfg
	served.Repos = cfg.RepositoriesFor(q)
	next, nq := cfg, q
	nq.Repos = append([]string(nil), q.Repos...)
	var cloned []NamedRepo
	for _, name := range wanted {
		name = strings.TrimSpace(name)
		if strings.Count(name, "/") != 1 {
			continue
		}
		if _, ok := lookupRepoScoped(served, name, true); ok {
			continue
		}
		if path, ok := lookupRepoScoped(cfg, name, true); ok {
			nq.Repos = appendOnce(nq.Repos, repoKeyFor(cfg, path))
			continue
		}
		if slices.ContainsFunc(cloned, func(r NamedRepo) bool { return strings.EqualFold(r.Name, name) }) {
			continue
		}
		if dir, err := clone(name); err == nil {
			cloned = append(cloned, NamedRepo{Name: name, Path: dir})
		}
	}
	next, nq, err := AddRepos(next, nq, cloned, false)
	if err != nil {
		return cfg, false, err
	}
	if len(nq.Repos) == len(q.Repos) {
		return cfg, false, nil
	}
	next.Queues = append([]config.Queue(nil), cfg.Queues...)
	next.SetQueue(nq)
	next, _ = KeepOthersServed(cfg, next, QueueKey{})
	if err := CheckReposStrandNothing(cfg, next, QueueKey{}); err != nil {
		return cfg, false, err
	}
	if err := next.Validate(); err != nil {
		return cfg, false, fmt.Errorf("the repositories chosen in Elk would not load here: %w", err)
	}
	return next, true, nil
}

func sameSecrets(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// settingsSignature is a stable string for "already applied this", so a
// refused setting is logged once rather than on every beat.
func settingsSignature(s elk.WorkerSettings) string {
	land := "-"
	if s.Land != nil {
		land = *s.Land
	}
	names := append([]string(nil), s.Secrets...)
	sort.Strings(names)
	perm := "-"
	if s.Permission != nil {
		perm = *s.Permission
	}
	repos := append([]string(nil), s.Repos...)
	sort.Strings(repos)
	return land + "|" + perm + "|" + strings.Join(names, ",") + "|" + strings.Join(repos, ",")
}

// applySettings writes Elk's settings for this queue into config.toml. The
// file change is what takes effect: under the service, checkConfigChange
// drains the runner and the service manager restarts it on the new file
// between tasks, exactly as for an enrolment (ark:rein#66).
//
// Repositories chosen in the app are added after the other settings, and a
// missing one is cloned into ~/Developer here, which holds up this beat.
func (qr *queueRunner) applySettings(ctx context.Context, s *elk.WorkerSettings) {
	path := qr.r.opts.ConfigPath
	if s == nil || path == "" {
		return
	}
	sig := settingsSignature(*s)
	qr.r.settingsMu.Lock()
	defer qr.r.settingsMu.Unlock()
	if qr.settingsSeen == sig {
		return
	}
	qr.settingsSeen = sig
	// A clone is remembered for the second pass below, which re-reads the
	// file: a clone takes long enough for `rein enrol` or `rein repos add` to
	// have written it meanwhile, and their change must not be lost.
	type result struct {
		dir string
		err error
	}
	clones := map[string]result{}
	clone := func(name string) (string, error) {
		if r, ok := clones[name]; ok {
			return r.dir, r.err
		}
		// Its own bound: a clone that waits on a credential prompt must not
		// hold every queue's settings forever.
		cctx, cancel := context.WithTimeout(ctx, cloneTimeout)
		defer cancel()
		dir, cloned, err := CloneRepo(cctx, name, "", "~/Developer", false)
		switch {
		case err != nil:
			qr.logf("settings: queue %s: could not add %s, skipping it: %v", qr.q.Name, name, err)
		case cloned:
			qr.logf("settings: queue %s: cloned %s into %s", qr.q.Name, name, dir)
		}
		clones[name] = result{dir, err}
		return dir, err
	}
	var next config.Config
	var changed bool
	for pass := 0; pass < 2; pass++ {
		cloned := len(clones)
		cfg, err := config.LoadFile(path)
		if err != nil {
			qr.logf("settings: cannot read %s to apply the Elk app's settings: %v", path, err)
			return
		}
		next, changed, err = ApplyElkSettings(cfg, qr.workspace, qr.q.Name, *s)
		if err != nil {
			if pass == 0 {
				qr.logf("settings: not applying the Elk app's settings: %v", err)
			}
			next = cfg
		}
		withRepos, added, err := AddElkRepos(next, qr.workspace, qr.q.Name, s.Repos, clone)
		switch {
		case err != nil:
			qr.logf("settings: not adding the repositories chosen in Elk to queue %s: %v", qr.q.Name, err)
		case added:
			next, changed = withRepos, true
		}
		if len(clones) == cloned {
			break
		}
	}
	if !changed {
		return
	}
	if err := next.SaveFile(path); err != nil {
		qr.logf("settings: could not save the Elk app's settings: %v", err)
		qr.settingsSeen = ""
		return
	}
	q, _ := next.QueueIn(qr.workspace, qr.q.Name)
	qr.logf("settings: took the Elk app's settings for %s (finishes by: %s; permission: %s; %d secret(s); repositories: %s)",
		qr.q.Name, landOf(next, qr.workspace, qr.q.Name), permissionOf(next, qr.workspace, qr.q.Name), len(s.Secrets), strings.Join(q.Repos, ", "))
	if !qr.r.canSelfRestart() {
		qr.logf("settings: restart Rein to use them (it is not running as the service)")
	}
}

// cloneTimeout bounds one clone of a repository chosen in the Elk app.
const cloneTimeout = 10 * time.Minute

func landOf(cfg config.Config, workspace, name string) string {
	q, _ := cfg.QueueIn(workspace, name)
	return q.LandOrDefault()
}

func permissionOf(cfg config.Config, workspace, name string) string {
	q, _ := cfg.QueueIn(workspace, name)
	return q.PermissionModeOrDefault()
}
