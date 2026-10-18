package browser

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPlatformNamesMatchChromeForTesting(t *testing.T) {
	for in, want := range map[string]string{
		"darwin/arm64": "mac-arm64", "darwin/amd64": "mac-x64",
		"linux/amd64": "linux64", "linux/arm64": "linux-arm64",
		"windows/amd64": "win64", "windows/386": "win32", "freebsd/amd64": "",
	} {
		goos, goarch, _ := strings.Cut(in, "/")
		if got := platform(goos, goarch); got != want {
			t.Errorf("platform(%s) = %q, want %q", in, got, want)
		}
	}
	if got := URL("1.2.3.4", "mac-arm64"); got != "https://storage.googleapis.com/chrome-for-testing-public/1.2.3.4/mac-arm64/chrome-mac-arm64.zip" {
		t.Errorf("URL = %s", got)
	}
}

// fakeInstall lays out a complete copy of version the way Install leaves one.
func fakeInstall(t *testing.T, home, version string) string {
	t.Helper()
	dir := filepath.Join(Dir(home), version)
	exe := filepath.Join(dir, executableIn(Platform()))
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, completeMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestResolvePrefersReinsCopyThenTheInstalledChrome(t *testing.T) {
	if Platform() == "" {
		t.Skip("no Chrome for Testing build for this platform")
	}
	system := map[string]bool{}
	exists := func(p string) bool { return system[p] }
	getenv := func(string) string { return "" }
	installed := systemPaths("darwin", getenv)[0]

	home := t.TempDir()
	if c := resolve(home, "", "darwin", getenv, exists); c.OK() {
		t.Fatalf("nothing installed, got %+v", c)
	}

	system[installed] = true
	c := resolve(home, "", "darwin", getenv, exists)
	if c.OK() || !strings.Contains(c.Why, "modify apps") || !strings.Contains(c.Why, "rein browser install") {
		t.Fatalf("macOS handed a run the installed Chrome, which asks for App Management: %+v", c)
	}
	linux := systemPaths("linux", getenv)[0]
	system[linux] = true
	c = resolve(home, "", "linux", getenv, exists)
	if c.Source != SourceInstalled || c.Path != linux || !strings.Contains(c.Why, "rein browser install") {
		t.Fatalf("installed Chrome only, on Linux: %+v", c)
	}

	// An interrupted download is not a copy.
	if err := os.MkdirAll(filepath.Join(Dir(home), Version), 0o755); err != nil {
		t.Fatal(err)
	}
	if c := resolve(home, "", "linux", getenv, exists); c.Source != SourceInstalled {
		t.Fatalf("an incomplete copy was used: %+v", c)
	}

	old := fakeInstall(t, home, "1.0.0.0")
	if c := resolve(home, "", "darwin", getenv, exists); c.Source != SourceRein || c.Path != old || !strings.Contains(c.Why, "older") {
		t.Fatalf("older copy: %+v", c)
	}
	pinned := fakeInstall(t, home, Version)
	if c := resolve(home, "", "darwin", getenv, exists); c.Source != SourceRein || c.Path != pinned || c.Version != Version {
		t.Fatalf("pinned copy: %+v", c)
	}
	if c := resolve(home, ConfigNone, "darwin", getenv, exists); c.OK() {
		t.Fatalf(`browser = "none" still gave a Chrome: %+v`, c)
	}
}

func TestWrapperAddsTheSafetyFlagsAndAFreshProfileEveryLaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the sh wrapper; chrome.cmd is covered by its text")
	}
	// A stand-in Chrome that records its arguments.
	bin := t.TempDir()
	fake := filepath.Join(bin, "Google Chrome")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nfor a in \"$@\"; do echo \"$a\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "run's dir")
	p, err := WriteWrapper(dir, Chrome{Path: fake, Source: SourceInstalled})
	if err != nil {
		t.Fatal(err)
	}
	var profiles []string
	for i := 0; i < 2; i++ {
		out, err := exec.Command(p, "--dump-dom", "about:blank").Output()
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(strings.TrimSpace(string(out)), "\n")
		for _, f := range Flags {
			if !contains(args, f) {
				t.Errorf("launch %d: missing %s in %q", i, f, args)
			}
		}
		if args[len(args)-2] != "--dump-dom" || args[len(args)-1] != "about:blank" {
			t.Errorf("the caller's arguments did not come last: %q", args)
		}
		var profile string
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "--user-data-dir="); ok {
				profile = v
			}
		}
		if !strings.HasPrefix(profile, dir+"/profile.") {
			t.Fatalf("profile %q is not a fresh directory under the run's", profile)
		}
		if st, err := os.Stat(profile); err != nil || !st.IsDir() {
			t.Fatalf("profile %q was not created: %v", profile, err)
		}
		profiles = append(profiles, profile)
	}
	if profiles[0] == profiles[1] {
		t.Errorf("two launches shared a profile: %s", profiles[0])
	}
}

func TestWindowsWrapperAddsTheSameFlags(t *testing.T) {
	w := windowsWrapper(`C:\t\run`, `C:\chrome.exe`)
	for _, want := range append(Flags, `--user-data-dir="%REIN_PROFILE%"`, `"C:\chrome.exe"`, "%*") {
		if !strings.Contains(w, want) {
			t.Errorf("chrome.cmd is missing %q:\n%s", want, w)
		}
	}
}

func TestRunEnvNamesTheBinaryAndPutsTheWrapperFirst(t *testing.T) {
	env := Chrome{Path: "/x/chrome", Source: SourceRein}.RunEnv("/run", "/usr/bin")
	for k, want := range map[string]string{
		EnvChrome: "/x/chrome", "CHROME_PATH": "/x/chrome", "PUPPETEER_EXECUTABLE_PATH": "/x/chrome",
		"PATH": "/run" + string(os.PathListSeparator) + "/usr/bin",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if (Chrome{}).RunEnv("/run", "/usr/bin") != nil {
		t.Error("no Chrome still changed the run's environment")
	}
}

// archive builds a Chrome for Testing-shaped zip for plat, with a symlink the
// way a macOS bundle's frameworks have them.
func archive(t *testing.T, plat string, extra func(*zip.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: filepath.ToSlash(executableIn(plat)), Method: zip.Deflate}
	h.SetMode(0o755)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("#!/bin/sh\n"))
	l := &zip.FileHeader{Name: "chrome-" + plat + "/Current"}
	l.SetMode(os.ModeSymlink | 0o755)
	w, _ = zw.CreateHeader(l)
	w.Write([]byte(filepath.Base(executableIn(plat))))
	if extra != nil {
		extra(zw)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInstallUnpacksOnceAndPrunesOlderCopies(t *testing.T) {
	plat := "linux64"
	if runtime.GOOS == "windows" {
		t.Skip("symlinks in the archive need privileges on Windows")
	}
	body := archive(t, plat, nil)
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		w.Write(body)
	}))
	defer srv.Close()
	urlFor := func(v, p string) string { return srv.URL + "/" + v + "/" + p }

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(Dir(home), "1.0.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	exe, err := install(context.Background(), srv.Client(), home, "2.0.0.0", plat, urlFor, &log)
	if err != nil {
		t.Fatal(err, log.String())
	}
	if want := filepath.Join(Dir(home), "2.0.0.0", executableIn(plat)); exe != want {
		t.Errorf("exe = %s, want %s", exe, want)
	}
	if st, err := os.Stat(exe); err != nil || st.Mode()&0o100 == 0 {
		t.Errorf("the binary is not executable: %v %v", st, err)
	}
	if target, err := os.Readlink(filepath.Join(Dir(home), "2.0.0.0", "chrome-"+plat, "Current")); err != nil || target == "" {
		t.Errorf("the symlink was not kept: %q %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(Dir(home), "1.0.0.0")); !os.IsNotExist(err) {
		t.Errorf("the older copy was not pruned: %v", err)
	}
	if _, err := install(context.Background(), srv.Client(), home, "2.0.0.0", plat, urlFor, nil); err != nil {
		t.Fatal(err)
	}
	if gets != 1 {
		t.Errorf("a second install downloaded again (%d downloads)", gets)
	}
}

func TestInstallRefusesAnArchiveThatEscapes(t *testing.T) {
	for name, extra := range map[string]func(*zip.Writer){
		"entry": func(zw *zip.Writer) { zw.Create("../evil") },
		"link": func(zw *zip.Writer) {
			h := &zip.FileHeader{Name: "chrome-linux64/evil"}
			h.SetMode(os.ModeSymlink | 0o777)
			w, _ := zw.CreateHeader(h)
			w.Write([]byte("../../../etc"))
		},
	} {
		body := archive(t, "linux64", extra)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
		home := t.TempDir()
		_, err := install(context.Background(), srv.Client(), home, "2.0.0.0", "linux64",
			func(v, p string) string { return srv.URL }, nil)
		srv.Close()
		if err == nil {
			t.Errorf("%s: an escaping archive installed", name)
		}
		if _, ok := Installed(home, "2.0.0.0"); ok {
			t.Errorf("%s: a refused archive left a copy behind", name)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
