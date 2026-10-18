// Package browser is the Chrome an agent run is given (ark:rein#87).
//
// Runs render, screenshot and test web pages, and before this package each one
// improvised a browser: the run that produced the macOS "rein was prevented
// from modifying apps" notice launched the person's own /Applications Chrome
// with no profile directory, and Chrome's updater tried to write into its own
// bundle under the LaunchAgent's name (docs/service.md). Two rulings
// (2026-10-17) settle what a run gets instead:
//
//   - A Chrome Rein manages, with a fresh profile every run, never the
//     person's own signed-in profile.
//   - Rein downloads its own copy — Chrome for Testing, Google's build for
//     automation, which has no updater — only when the person opts in:
//     `rein browser install`, or `browser = "rein"` in config.toml. Until
//     then it uses the Chrome already installed on the machine, launched with
//     the safety flags in [Flags].
//
// On macOS the second half did not survive measurement: the installed Chrome,
// with a fresh profile and every flag, still asked for App Management under
// Rein's name (see [Resolve]). There, Rein's copy is the only Chrome a run
// gets, and until the person opts in a run gets none.
//
// [Resolve] picks one Chrome per machine; the run loop hands it to every run
// as REIN_CHROME (plus CHROME_PATH and PUPPETEER_EXECUTABLE_PATH) and as a
// `chrome` command on the run's PATH that always adds [Flags] and a fresh
// --user-data-dir ([WriteWrapper]).
//
// Chrome is never made its own responsible process (`open -a`, a
// responsibility disclaim) to get around the notice: a self-responsible Chrome
// brings the person's own Chrome grants — camera, microphone, screen — into an
// agent run.
package browser

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Version is the Chrome for Testing build `rein browser install` downloads.
// It moves with Rein releases; bump it from
// https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json
// (channels.Stable.version).
const Version = "155.0.8059.84"

// DownloadHost is the one host the install reaches, for an egress allow-list.
const DownloadHost = "storage.googleapis.com"

// Config values for `browser` in config.toml.
const (
	// ConfigAuto (unset) uses Rein's copy when it is installed, otherwise the
	// installed Chrome, and downloads nothing.
	ConfigAuto = ""
	// ConfigRein opts in: Rein installs its own copy at start-up when the
	// pinned version is missing.
	ConfigRein = "rein"
	// ConfigNone gives runs no Chrome at all.
	ConfigNone = "none"
)

// Sources of a resolved Chrome.
const (
	SourceRein      = "rein"
	SourceInstalled = "installed"
)

// Flags are added to every launch through the `chrome` wrapper, before the
// caller's own arguments.
//
//   - --headless=new: the service lives in the person's GUI session, so a
//     visible window would open on their screen.
//   - --use-mock-keychain: a fresh profile can otherwise raise a Keychain
//     dialog in the person's session on macOS. Harmless elsewhere.
//   - --no-first-run, --no-default-browser-check: no first-run UI, and no
//     attempt to become the person's default browser.
//
// The wrapper adds --user-data-dir itself, a new directory per launch.
var Flags = []string{"--headless=new", "--use-mock-keychain", "--no-first-run", "--no-default-browser-check"}

// Chrome is the browser this machine gives its runs.
type Chrome struct {
	// Path is the executable, or "" when runs get no Chrome.
	Path string
	// Source is SourceRein or SourceInstalled, "" when Path is.
	Source string
	// Version is Rein's copy's version; "" for the installed Chrome.
	Version string
	// Why says in words which Chrome this is and why — the line `rein
	// status` prints and the provenance of the `browser` host capability.
	Why string
}

// OK reports whether runs get a Chrome.
func (c Chrome) OK() bool { return c.Path != "" }

// Dir is where Rein keeps its copies: <rein home>/browsers.
func Dir(reinHome string) string { return filepath.Join(reinHome, "browsers") }

// Platform is Chrome for Testing's name for this machine, "" when it ships no
// build for it.
func Platform() string { return platform(runtime.GOOS, runtime.GOARCH) }

func platform(goos, goarch string) string {
	switch goos + "/" + goarch {
	case "darwin/arm64":
		return "mac-arm64"
	case "darwin/amd64":
		return "mac-x64"
	case "linux/amd64":
		return "linux64"
	case "linux/arm64":
		return "linux-arm64"
	case "windows/amd64":
		return "win64"
	case "windows/386":
		return "win32"
	}
	return ""
}

// URL is the archive for one version and platform.
func URL(version, plat string) string {
	return "https://" + DownloadHost + "/chrome-for-testing-public/" + version + "/" + plat + "/chrome-" + plat + ".zip"
}

// executableIn is the Chrome binary's path inside an extracted archive.
func executableIn(plat string) string {
	dir := "chrome-" + plat
	switch {
	case strings.HasPrefix(plat, "mac-"):
		return filepath.Join(dir, "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing")
	case strings.HasPrefix(plat, "win"):
		return filepath.Join(dir, "chrome.exe")
	}
	return filepath.Join(dir, "chrome")
}

// completeMarker is written last by [Install]; a version directory without it
// is a download that did not finish.
const completeMarker = ".rein-complete"

// Installed reports Rein's copy of version, if a complete one is on disk.
func Installed(reinHome, version string) (string, bool) {
	plat := Platform()
	if plat == "" {
		return "", false
	}
	dir := filepath.Join(Dir(reinHome), version)
	if _, err := os.Stat(filepath.Join(dir, completeMarker)); err != nil {
		return "", false
	}
	exe := filepath.Join(dir, executableIn(plat))
	if !isFile(exe) {
		return "", false
	}
	return exe, true
}

// installedVersions lists complete versions on disk, newest first.
func installedVersions(reinHome string) []string {
	entries, err := os.ReadDir(Dir(reinHome))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, ok := Installed(reinHome, e.Name()); ok {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[j], out[i]) })
	return out
}

// versionLess compares dotted numeric versions.
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x = atoi(as[i])
		}
		if i < len(bs) {
			y = atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// systemPaths is where an installed Chrome lives on each platform, in the
// order they are tried.
func systemPaths(goos string, getenv func(string) string) []string {
	switch goos {
	case "darwin":
		return []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
	case "linux":
		return []string{
			"/opt/google/chrome/chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/google-chrome",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
		}
	case "windows":
		var out []string
		for _, root := range []string{getenv("PROGRAMFILES"), getenv("PROGRAMFILES(X86)"), getenv("LOCALAPPDATA")} {
			if root != "" {
				out = append(out, filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
		return out
	}
	return nil
}

// Resolve picks the one Chrome this machine gives its runs: Rein's own copy if
// one is installed (the pinned version, else the newest on disk), otherwise
// the installed Chrome at the platform's standard path — on Linux and Windows
// only — otherwise none.
// setting is config.toml's `browser`.
func Resolve(reinHome, setting string) Chrome {
	return resolve(reinHome, setting, runtime.GOOS, os.Getenv, isFile)
}

func resolve(reinHome, setting, goos string, getenv func(string) string, exists func(string) bool) Chrome {
	if setting == ConfigNone {
		return Chrome{Why: `none: config.toml sets browser = "none"`}
	}
	if exe, ok := Installed(reinHome, Version); ok {
		return Chrome{Path: exe, Source: SourceRein, Version: Version,
			Why: "Rein's Chrome for Testing " + Version + " (" + exe + ")"}
	}
	if vs := installedVersions(reinHome); len(vs) > 0 {
		exe, _ := Installed(reinHome, vs[0])
		return Chrome{Path: exe, Source: SourceRein, Version: vs[0],
			Why: "Rein's Chrome for Testing " + vs[0] + ", older than this build's " + Version +
				" — `rein browser install` updates it (" + exe + ")"}
	}
	optIn := "`rein browser install` to use Rein's own copy instead"
	if setting == ConfigRein {
		optIn = "Rein's own copy is not installed yet; it installs at start-up, or run `rein browser install`"
	}
	for _, p := range systemPaths(goos, getenv) {
		if !exists(p) {
			continue
		}
		if goos == "darwin" {
			// Measured under the LaunchAgent on 2026-10-17: the installed
			// Chrome, launched with a fresh profile and every flag in
			// [Flags], still asked macOS to write into an app bundle
			// (kTCCServiceSystemPolicyAppBundles, preflight=no) under Rein's
			// name, and started its notification helper. That is the "rein
			// was prevented from modifying apps" notice, so on macOS it is
			// never handed to a run; Chrome for Testing asked nothing.
			return Chrome{Why: "none: the installed Chrome (" + p + ") is not given to runs on macOS, " +
				"because launching it asks macOS to let Rein modify apps; " +
				"`rein browser install` gives runs Rein's own copy"}
		}
		return Chrome{Path: p, Source: SourceInstalled,
			Why: "the installed Chrome, fresh profile and safety flags (" + p + "); " + optIn}
	}
	return Chrome{Why: "none: no Chrome installed and Rein's own copy is not; `rein browser install` gives runs one"}
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
