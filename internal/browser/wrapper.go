package browser

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Run-side variables. REIN_CHROME is the binary itself, for anything that
// takes a path; CHROME_PATH and PUPPETEER_EXECUTABLE_PATH are what
// chrome-launcher (Lighthouse) and Puppeteer read, so they find it without
// being told. A library that launches Chrome itself passes its own profile and
// flags; a person or an agent launching it by hand uses `chrome`.
const (
	EnvChrome    = "REIN_CHROME"
	EnvChromeDir = "REIN_CHROME_DIR"
)

// RunEnv is what a run gets for c: the variables above, and PATH with the
// wrapper's directory in front. path is the PATH the run would otherwise get.
// Nil when runs get no Chrome.
func (c Chrome) RunEnv(wrapperDir, path string) map[string]string {
	if !c.OK() {
		return nil
	}
	p := wrapperDir
	if path != "" {
		p += string(os.PathListSeparator) + path
	}
	return map[string]string{
		EnvChrome:                   c.Path,
		EnvChromeDir:                wrapperDir,
		"CHROME_PATH":               c.Path,
		"PUPPETEER_EXECUTABLE_PATH": c.Path,
		"PATH":                      p,
	}
}

// WriteWrapper writes the `chrome` command into dir (the run's own directory,
// outside its worktree, removed when the run ends) and returns its path. Every
// launch through it gets [Flags] and a new --user-data-dir under dir, so no
// two launches share a profile and none is the person's own.
func WriteWrapper(dir string, c Chrome) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		p := filepath.Join(dir, "chrome.cmd")
		return p, os.WriteFile(p, []byte(windowsWrapper(dir, c.Path)), 0o700)
	}
	p := filepath.Join(dir, "chrome")
	return p, os.WriteFile(p, []byte(unixWrapper(dir, c.Path)), 0o700)
}

func unixWrapper(dir, exe string) string {
	return "#!/bin/sh\n" +
		"# Written by Rein for one run. A fresh profile per launch, never a window,\n" +
		"# never the person's own profile. See rein docs/service.md.\n" +
		"profile=$(mktemp -d " + shQuote(dir+"/profile.XXXXXX") + ") || exit 1\n" +
		"exec " + shQuote(exe) + " " + strings.Join(Flags, " ") + " --user-data-dir=\"$profile\" \"$@\"\n"
}

func windowsWrapper(dir, exe string) string {
	return "@echo off\r\n" +
		"rem Written by Rein for one run. A fresh profile per launch, never a window,\r\n" +
		"rem never the person's own profile. See rein docs/service.md.\r\n" +
		"setlocal\r\n" +
		"set \"REIN_PROFILE=" + dir + "\\profile-%RANDOM%%RANDOM%%RANDOM%\"\r\n" +
		"mkdir \"%REIN_PROFILE%\" || exit /b 1\r\n" +
		"\"" + exe + "\" " + strings.Join(Flags, " ") + " --user-data-dir=\"%REIN_PROFILE%\" %*\r\n"
}

// shQuote single-quotes s for sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
