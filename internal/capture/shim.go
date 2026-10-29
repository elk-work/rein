package capture

import (
	"os"
	"path/filepath"
	"strings"
)

// WriteScreencaptureShim writes a `screencapture` command into dir that
// forwards to `rein capture screencapture`. Rein puts that directory at the
// front of a run's PATH, so an agent that types screencapture hits the check
// in this package. The real tool is /usr/sbin/screencapture, which this shim
// never execs: the rein binary it names does, and only after the check.
//
// Calling /usr/sbin/screencapture by its path walks around the shim. The
// prompt forbids that. The shim is the check on the command name, not a
// sandbox, which docs/run-loop.md already says about every check of this shape.
func WriteScreencaptureShim(dir, reinExe string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "screencapture")
	body := "#!/bin/sh\n" +
		"# Written by Rein for one run. Photographs a window only when its\n" +
		"# owner is a process this run launched. The display and every other\n" +
		"# window are refused. See docs/run-loop.md.\n" +
		"exec " + shQuote(reinExe) + " capture screencapture -- \"$@\"\n"
	return path, os.WriteFile(path, []byte(body), 0o700)
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
