package runner

import (
	"os"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/capture"
)

// applyCaptureGate puts a screencapture shim in dir on the run's PATH and
// records the directory in [capture.EnvBin]. dir is outside the worktree; the
// caller removes it when the run ends. Chrome, when the run has one, is
// prepended after this so `chrome` stays ahead of the gate and the gate stays
// ahead of /usr/sbin.
func applyCaptureGate(spec *adapter.RunSpec, dir, reinExe string) error {
	if _, err := capture.WriteScreencaptureShim(dir, reinExe); err != nil {
		return err
	}
	pathKey, path := "PATH", ""
	if v, ok := spec.Env["PATH"]; ok {
		path = v
	} else {
		pathKey, path = environPath()
	}
	// A copy: a scoped queue's Env is its resolved secrets map, which the
	// redactor was built from and which is not Rein's to add to.
	env := make(map[string]string, len(spec.Env)+2)
	for k, v := range spec.Env {
		env[k] = v
	}
	spec.Env = env
	next := dir
	if path != "" {
		next += string(os.PathListSeparator) + path
	}
	spec.Env[pathKey] = next
	spec.Env[capture.EnvBin] = dir
	return nil
}
