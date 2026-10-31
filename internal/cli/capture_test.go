package cli_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCaptureRefusesTheDisplay(t *testing.T) {
	out := filepath.Join(t.TempDir(), "shot.png")
	_, stderr, code := run(t, "capture", "screencapture", "--", out)
	if code == 0 {
		t.Fatal("a capture with no window id succeeded")
	}
	if !strings.Contains(stderr, "off limits") {
		t.Fatalf("stderr = %q", stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("a refused capture wrote %s (%v)", out, err)
	}
}

func TestCaptureRefusesAProcessTheRunDidNotLaunch(t *testing.T) {
	out := filepath.Join(t.TempDir(), "shot.png")
	// pid 1 is launchd (or the equivalent). It is never a process this run started.
	_, stderr, code := run(t, "capture", "window", "--pid", "1", "--out", out)
	if code == 0 {
		t.Fatal("photographing pid 1 succeeded")
	}
	if runtime.GOOS == "darwin" {
		if !strings.Contains(stderr, "not a process this run launched") {
			t.Fatalf("stderr = %q", stderr)
		}
	} else if !strings.Contains(stderr, "macOS") {
		t.Fatalf("stderr = %q", stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("a refused capture wrote %s (%v)", out, err)
	}
}

func TestCaptureSiteUsesHeadlessChromeAndRefusesAFile(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "chrome")
	marker := filepath.Join(dir, "args")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(marker) + "\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	out := filepath.Join(dir, "shot.png")
	_, stderr, code := run(t, "capture", "site", "--out", out, "file:///etc/passwd")
	if code == 0 {
		t.Fatal("a file URL was photographed")
	}
	if !strings.Contains(stderr, "http") {
		t.Fatalf("stderr = %q", stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("chrome was launched for a refused url")
	}
	// LookPath("chrome") on Windows only finds a PATHEXT name such as
	// chrome.exe. The arguments themselves are tested in PlanSite, on every
	// platform. This half checks the command actually launches that chrome.
	if runtime.GOOS == "windows" {
		return
	}

	_, stderr, code = run(t, "capture", "site", "--out", out, "https://example.com/page")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"--headless=new", "--use-mock-keychain", "--screenshot=" + out, "https://example.com/page", "--user-data-dir="} {
		if !strings.Contains(text, want) {
			t.Errorf("chrome args missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Library/Application Support/Google/Chrome") {
		t.Fatalf("the page shot used the person's Chrome profile:\n%s", text)
	}
}

func TestCaptureHelpNamesBothForms(t *testing.T) {
	out, _, code := run(t, "capture", "--help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"window", "site", "off limits"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
