package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
		return
	}
	if !r.opts.UnderService || r.opts.Once || r.opts.DryRun || (r.opts.Config.SelfRestart != nil && !*r.opts.Config.SelfRestart) {
		r.logf("upgrade: %s installed (running %s); automatic restart disabled", version, r.opts.Version)
		return
	}
	r.drain(version)
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
