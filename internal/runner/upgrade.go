package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/elk"
)

// ErrRestartForUpgrade asks the service manager to launch the installed binary.
var ErrRestartForUpgrade = errors.New("restart for installed upgrade")

const RestartExitCode = 75

func (r *Runner) isDraining() bool {
	r.upgradeMu.Lock()
	defer r.upgradeMu.Unlock()
	return r.draining
}
func (r *Runner) beginClaim() bool {
	r.upgradeMu.Lock()
	defer r.upgradeMu.Unlock()
	if r.draining {
		return false
	}
	r.inFlight++
	return true
}
func (r *Runner) endClaim() {
	r.upgradeMu.Lock()
	defer r.upgradeMu.Unlock()
	r.inFlight--
	if r.draining && r.inFlight == 0 {
		close(r.upgradeDone)
	}
}
func (r *Runner) drain(version string) {
	r.upgradeMu.Lock()
	defer r.upgradeMu.Unlock()
	if r.draining {
		return
	}
	r.draining = true
	if r.upgradeStarted != nil {
		close(r.upgradeStarted)
	}
	r.logf("upgrade: %s → %s; draining %d in-flight runs", r.opts.Version, version, r.inFlight)
	if r.inFlight == 0 {
		close(r.upgradeDone)
	}
}

func binaryVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", errors.New("empty version output")
	}
	return fields[len(fields)-1], nil
}

func sameBinary(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// checkUpgrade re-baselines only after a successful version probe. A failed
// probe is retried, and a deleted executable never strands the running daemon.
func (r *Runner) checkUpgrade(ctx context.Context, path string, baseline *os.FileInfo) {
	info, err := r.opts.UpgradeStat(path)
	if err != nil {
		r.logf("upgrade: installed binary unavailable: %v", err)
		return
	}
	if sameBinary(*baseline, info) {
		r.recheckRefused(ctx, path)
		return
	}
	version, err := r.opts.UpgradeVersion(ctx, path)
	if err != nil {
		r.logf("upgrade: cannot read installed version: %v", err)
		return
	}
	// Do not associate a version with a binary replaced during the probe.
	after, err := r.opts.UpgradeStat(path)
	if err != nil || !sameBinary(info, after) {
		return
	}
	*baseline = info
	if version == r.opts.Version {
		r.logf("upgrade: same version %s reinstalled", version)
		r.clearRefusal()
		return
	}
	if !r.opts.UnderService || r.opts.Once || r.opts.DryRun || (r.opts.Config.SelfRestart != nil && !*r.opts.Config.SelfRestart) {
		r.logf("upgrade: %s installed (running %s); automatic restart disabled", version, r.opts.Version)
		return
	}
	r.tryUpgrade(ctx, path, version)
}

// tryUpgrade drains onto an installed version unless its config check says
// the restart would strand queues this runner serves (ark:rein#67).
//
// v0.8.4 is why: it changed which repositories a multi-workspace config's
// queues could use, running daemons drained into it on schedule, and every
// run on every queue then ended stuck at preflight. The new binary is the only
// thing that knows its own rules, so it is asked — `config check --json`
// against the live config — and its reading is compared with this build's
// reading of the same file. A refused upgrade keeps this process serving; the
// binary on disk stays the new one, so the next restart of any kind takes it,
// and fixing config.toml lets this process take it on the next check.
func (r *Runner) tryUpgrade(ctx context.Context, path, version string) {
	stranded := r.upgradeGuard(ctx, path, version)
	if len(stranded) == 0 {
		if r.clearRefusal() {
			r.logf("upgrade: %s now reads the config without stranding a queue", version)
		}
		r.drain(version)
		return
	}
	r.refuse(version, stranded)
}

// upgradeGuard returns what restarting into the installed binary at path would
// strand, or nothing. It fails open: a binary with no config check — anything
// older than ark:rein#67 — or one that cannot answer is restarted into as
// before, because a guard that wedged every upgrade on a probe failure would
// be its own outage.
func (r *Runner) upgradeGuard(ctx context.Context, path, version string) []string {
	check := r.opts.UpgradeCheck
	if check == nil {
		check = ExecConfigCheck
	}
	next, err := check(ctx, path, r.opts.ConfigPath)
	switch {
	case err != nil:
		r.logf("upgrade: %s cannot check the config (%v); restarting into it unchecked", version, err)
		return nil
	case next.Schema > ConfigCheckSchema:
		r.logf("upgrade: %s answers config check schema %d, newer than this build reads; restarting into it unchecked", version, next.Schema)
		return nil
	}
	if next.Error != "" {
		r.logf("upgrade: %s cannot load the config: %s", version, next.Error)
	}
	return strandedBy(r.currentReport(), next)
}

// currentReport is this build's reading of the live config file, or of the
// config it started with when the file no longer loads.
func (r *Runner) currentReport() ConfigReport {
	if r.opts.ConfigPath != "" {
		if rep := CheckConfigFile(r.opts.ConfigPath, r.opts.Version); rep.Error == "" {
			return rep
		}
	}
	return CheckConfig(r.opts.Config, r.opts.Version)
}

// ExecConfigCheck runs `<binary> config check --json` against configPath and
// reads its report. The check exits 1 when it finds a problem, with the report
// still on stdout, so the report decides and the exit status does not. No
// report at all means the binary has no config check.
func ExecConfigCheck(ctx context.Context, binary, configPath string) (ConfigReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{"config", "check", "--json"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = &stdout
	runErr := cmd.Run()
	var rep ConfigReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil || rep.Schema < 1 {
		if runErr != nil {
			return ConfigReport{}, fmt.Errorf("no config check report: %w", runErr)
		}
		return ConfigReport{}, errors.New("no config check report")
	}
	return rep, nil
}

// upgradeRefusal is an installed version this runner would not restart into.
type upgradeRefusal struct {
	version  string
	stranded string
	since    time.Time
	// config is the config file as it was when the upgrade was refused, so an
	// edit to it is noticed and the check run again.
	config os.FileInfo
}

// refuse records and logs a refused upgrade, once per distinct reason.
func (r *Runner) refuse(version string, stranded []string) {
	detail := strings.Join(stranded, "; ")
	var cfgInfo os.FileInfo
	if r.opts.ConfigPath != "" {
		cfgInfo, _ = os.Stat(r.opts.ConfigPath)
	}
	r.upgradeMu.Lock()
	prev := r.refusal
	since := time.Now()
	if prev != nil && prev.version == version {
		since = prev.since
	}
	r.refusal = &upgradeRefusal{version: version, stranded: detail, since: since, config: cfgInfo}
	r.upgradeMu.Unlock()
	if prev != nil && prev.version == version && prev.stranded == detail {
		return
	}
	r.logf("upgrade: NOT restarting into %s, which is installed; staying on %s. Under %s: %s. "+
		"Fix config.toml (`rein config check` shows how the installed binary reads it) and this runner "+
		"restarts into %s on its next check, or restart the service to take %s as it is.",
		version, r.opts.Version, version, detail, version, version)
}

// clearRefusal forgets a refused upgrade, reporting whether there was one.
func (r *Runner) clearRefusal() bool {
	r.upgradeMu.Lock()
	defer r.upgradeMu.Unlock()
	had := r.refusal != nil
	r.refusal = nil
	return had
}

// recheckRefused runs a refused upgrade's check again once config.toml has
// changed, so fixing the file is all it takes.
func (r *Runner) recheckRefused(ctx context.Context, path string) {
	r.upgradeMu.Lock()
	ref := r.refusal
	r.upgradeMu.Unlock()
	if ref == nil || r.opts.ConfigPath == "" {
		return
	}
	info, err := os.Stat(r.opts.ConfigPath)
	if err != nil || (ref.config != nil && sameBinary(ref.config, info)) {
		return
	}
	r.tryUpgrade(ctx, path, ref.version)
}

// putRefusal adds a refused upgrade to a queue's beat as waiting_on, when the
// queue has nothing more pressing to say: the queue still works, but the
// machine is held on an old Rein until a person fixes its config. The
// sentence is Rein's own and names queues and repositories, never paths.
func (r *Runner) putRefusal(s *elk.SessionReading) {
	if r == nil || s.WaitingOn != "" || s.ExhaustedUntil != "" {
		return
	}
	r.upgradeMu.Lock()
	ref := r.refusal
	r.upgradeMu.Unlock()
	if ref == nil {
		return
	}
	s.WaitingOn = clip(fmt.Sprintf("Rein %s is installed but this machine stays on %s until config.toml is fixed: under %s, %s",
		ref.version, r.opts.Version, ref.version, ref.stranded), 400)
	s.WaitingSince = elk.RFC3339(ref.since)
}

func (r *Runner) startUpgradeWatch(ctx context.Context) {
	if r.opts.Version == "dev" || r.opts.Version == "" {
		return
	}
	path, err := os.Executable()
	if err == nil {
		path, err = filepath.EvalSymlinks(path)
	}
	if err != nil {
		r.logf("upgrade: cannot locate executable: %v", err)
		return
	}
	if r.opts.UpgradeStat == nil {
		r.opts.UpgradeStat = os.Stat
	}
	if r.opts.UpgradeVersion == nil {
		r.opts.UpgradeVersion = binaryVersion
	}
	baseline, err := r.opts.UpgradeStat(path)
	if err != nil {
		r.logf("upgrade: cannot baseline executable: %v", err)
		return
	}
	interval := r.opts.UpgradeInterval
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.upgradeDone:
				return
			case <-ticker.C:
				r.checkUpgrade(ctx, path, &baseline)
			}
		}
	}()
}
