package runner_test

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/browser"
	"github.com/elk-work/rein/internal/capture"
	"github.com/elk-work/rein/internal/runner"
)

// ark:rein#87: every run gets the machine's Chrome as a `chrome` wrapper on
// its PATH and REIN_CHROME / CHROME_PATH / PUPPETEER_EXECUTABLE_PATH, in a
// directory of its own that is gone when the run ends.

func TestARunIsGivenTheMachinesChrome(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	c := &browser.Chrome{Path: "/opt/chrome/chrome", Source: browser.SourceRein}
	if err := h.run(runner.Options{HostCapabilities: machineHolds("browser"), Chrome: c}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spec := h.agent.Specs()[0]
	for _, k := range []string{browser.EnvChrome, "CHROME_PATH", "PUPPETEER_EXECUTABLE_PATH"} {
		if spec.Env[k] != c.Path {
			t.Errorf("%s = %q, want %q", k, spec.Env[k], c.Path)
		}
	}
	dir := spec.Env[browser.EnvChromeDir]
	if dir == "" || strings.HasPrefix(dir, spec.WorktreeDir) {
		t.Fatalf("the wrapper directory %q is missing or inside the worktree", dir)
	}
	if !strings.HasPrefix(spec.Env["PATH"], dir+string(os.PathListSeparator)) {
		t.Errorf("PATH %q does not start with the wrapper directory %q", spec.Env["PATH"], dir)
	}
	if runtime.GOOS == "darwin" {
		gate := spec.Env[capture.EnvBin]
		if gate == "" || !strings.Contains(spec.Env["PATH"], gate) {
			t.Errorf("PATH %q lost the screencapture gate %q", spec.Env["PATH"], gate)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the run's Chrome directory outlived the run: %v", err)
	}
}

func TestARunOnAMachineWithoutChromeGetsNone(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{HostCapabilities: machineHolds()}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	spec := h.agent.Specs()[0]
	if _, ok := spec.Env[browser.EnvChrome]; ok {
		t.Errorf("a machine with no Chrome handed a run REIN_CHROME=%q", spec.Env[browser.EnvChrome])
	}
	if runtime.GOOS == "darwin" {
		// No Chrome, but the screencapture gate is still installed: it is what
		// refuses a photograph of the person's screen, and it does not depend
		// on a browser.
		dir := spec.Env[capture.EnvBin]
		if dir == "" || !strings.HasPrefix(spec.Env["PATH"], dir+string(os.PathListSeparator)) {
			t.Fatalf("PATH %q bin %q, want the capture gate in front", spec.Env["PATH"], dir)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("the capture gate directory outlived the run: %v", err)
		}
		return
	}
	if _, ok := spec.Env["PATH"]; ok {
		t.Errorf("a machine with no Chrome rewrote the run's PATH")
	}
}
