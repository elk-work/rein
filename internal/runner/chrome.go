package runner

import (
	"os"
	"runtime"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/browser"
	"github.com/elk-work/rein/internal/runlog"
)

// giveChrome hands the machine's Chrome to one run (ark:rein#87): a `chrome`
// command on the run's PATH that always adds a fresh profile and the safety
// flags, and REIN_CHROME / CHROME_PATH / PUPPETEER_EXECUTABLE_PATH naming the
// binary. They go in spec.Env rather than through passEnv because Rein sets
// them; the daemon holds none of them.
//
// The wrapper and every profile it makes live in one directory outside the
// worktree, which the returned func removes when the run ends. A run on a
// machine with no Chrome gets nothing and a no-op.
func (qr *queueRunner) giveChrome(spec *adapter.RunSpec) func() {
	c := qr.r.chrome
	if !c.OK() {
		return func() {}
	}
	dir, err := os.MkdirTemp("", "rein-chrome-"+sanitizeRunID(spec.RunID)+"-")
	if err == nil {
		_, err = browser.WriteWrapper(dir, c)
	}
	if err != nil {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
		qr.log.Runner(runlog.KindNote, "no Chrome for this run: %v", err)
		qr.logf("run %s: no Chrome for this run: %v", spec.RunID, err)
		return func() {}
	}
	pathKey, path := "PATH", ""
	if v, ok := spec.Env["PATH"]; ok {
		path = v
	} else {
		pathKey, path = environPath()
	}
	// A copy: a scoped queue's Env is its resolved secrets map, which the
	// redactor was built from and which is not Rein's to add to.
	env := make(map[string]string, len(spec.Env)+5)
	for k, v := range spec.Env {
		env[k] = v
	}
	spec.Env = env
	for k, v := range c.RunEnv(dir, path) {
		if k == "PATH" {
			k = pathKey
		}
		spec.Env[k] = v
	}
	return func() { _ = os.RemoveAll(dir) }
}

// environPath is the daemon's PATH and the name it is spelled with — "Path" on
// most Windows machines, where an override spelled "PATH" would sit beside it
// rather than replace it.
func environPath() (string, string) {
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if k == "PATH" || (runtime.GOOS == "windows" && strings.EqualFold(k, "PATH")) {
			return k, v
		}
	}
	return "PATH", ""
}

// sanitizeRunID keeps a run id safe to put in a directory name.
func sanitizeRunID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
}
