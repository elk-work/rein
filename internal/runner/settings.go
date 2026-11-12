package runner

import (
	"errors"
	"fmt"
	"sort"
	"strings"

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
	return land + "|" + perm + "|" + strings.Join(names, ",")
}

// applySettings writes Elk's settings for this queue into config.toml. The
// file change is what takes effect: under the service, checkConfigChange
// drains the runner and the service manager restarts it on the new file
// between tasks, exactly as for an enrolment (ark:rein#66).
func (qr *queueRunner) applySettings(s *elk.WorkerSettings) {
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
	cfg, err := config.LoadFile(path)
	if err != nil {
		qr.logf("settings: cannot read %s to apply the Elk app's settings: %v", path, err)
		qr.settingsSeen = sig
		return
	}
	next, changed, err := ApplyElkSettings(cfg, qr.workspace, qr.q.Name, *s)
	qr.settingsSeen = sig
	switch {
	case err != nil:
		qr.logf("settings: not applying the Elk app's settings: %v", err)
	case !changed:
	default:
		if err := next.SaveFile(path); err != nil {
			qr.logf("settings: could not save the Elk app's settings: %v", err)
			qr.settingsSeen = ""
			return
		}
		qr.logf("settings: took the Elk app's settings for %s (finishes by: %s; permission: %s; %d secret(s))",
			qr.q.Name, landOf(next, qr.workspace, qr.q.Name), permissionOf(next, qr.workspace, qr.q.Name), len(s.Secrets))
		if !qr.r.canSelfRestart() {
			qr.logf("settings: restart Rein to use them (it is not running as the service)")
		}
	}
}

func landOf(cfg config.Config, workspace, name string) string {
	q, _ := cfg.QueueIn(workspace, name)
	return q.LandOrDefault()
}

func permissionOf(cfg config.Config, workspace, name string) string {
	q, _ := cfg.QueueIn(workspace, name)
	return q.PermissionModeOrDefault()
}
