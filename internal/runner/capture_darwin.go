//go:build darwin

package runner

import (
	"os"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/runlog"
)

// giveCapture installs the screencapture gate for one run. A failure is a
// note, not a stuck run: `rein capture window` is still the binary, and a run
// that never photographs anything should not fail because the shim could not
// be written.
func (qr *queueRunner) giveCapture(spec *adapter.RunSpec) func() {
	exe, err := os.Executable()
	if err != nil {
		qr.log.Runner(runlog.KindNote, "no screencapture gate for this run: %v", err)
		qr.logf("run %s: no screencapture gate: %v", spec.RunID, err)
		return func() {}
	}
	dir, err := os.MkdirTemp("", "rein-capture-"+sanitizeRunID(spec.RunID)+"-")
	if err == nil {
		err = applyCaptureGate(spec, dir, exe)
	}
	if err != nil {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
		qr.log.Runner(runlog.KindNote, "no screencapture gate for this run: %v", err)
		qr.logf("run %s: no screencapture gate: %v", spec.RunID, err)
		return func() {}
	}
	return func() { _ = os.RemoveAll(dir) }
}
