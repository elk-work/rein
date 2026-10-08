package cli_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/service"
)

// Nothing in this file installs, starts or stops anything. A service is a
// machine-wide object living in the real home directory — REIN_HOME does not
// isolate ~/Library/LaunchAgents — so a test that called install would edit
// the developer's login items, and a test that called start would launch a
// second runner beside theirs. Everything here stops before the platform is
// touched.

func TestServiceSubcommandsExist(t *testing.T) {
	for _, sub := range []string{"install", "uninstall", "start", "stop", "status"} {
		t.Run(sub, func(t *testing.T) {
			out, _, code := run(t, "service", sub, "--help")
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if !strings.Contains(out, "rein service "+sub) {
				t.Errorf("help does not name the command:\n%s", out)
			}
		})
	}
}

func TestServiceInstallRefusesWithoutAConfig(t *testing.T) {
	// A service installed against no config is a login item that starts,
	// fails to find a queue, exits, and is restarted by KeepAlive for as long
	// as the machine is on. Refuse before writing anything.
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)

	_, errb, code := run(t, "service", "install")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\nstderr: %s", code, errb)
	}
	if !strings.Contains(errb, "rein enrol") {
		t.Errorf("the error does not say what to do about it: %q", errb)
	}
	if _, err := os.Stat(filepath.Join(home, service.LogDirName)); err == nil {
		t.Error("a refused install still created the log directory")
	}
}

func TestServiceInstallRefusesAConfigWithNoQueues(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	writeConfig(t, filepath.Join(home, config.FileName), "host_id = \"tst\"\nworkspace = \"Elk Scout\"\n\n[elk]\nmcp_url = \"https://example.test/elk-mcp\"\n")

	_, errb, code := run(t, "service", "install")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\nstderr: %s", code, errb)
	}
	if !strings.Contains(errb, "no queues") {
		t.Errorf("the error does not name the problem: %q", errb)
	}
}

func TestServiceStatusReportsNotInstalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Status asks the SCM, which needs no privilege to read but is a
		// machine-wide object; the darwin and linux paths cover the shape.
		t.Skip("service status talks to the Windows SCM")
	}
	// The real machine may well have Rein installed — this asserts the
	// command answers rather than what the answer is.
	out, errb, code := run(t, "service", "status")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, errb)
	}
	if !strings.Contains(out, service.Name) {
		t.Errorf("status does not name the service:\n%s", out)
	}
	for _, want := range []string{"platform", "command", "logs"} {
		if !strings.Contains(out, want) {
			t.Errorf("status is missing the %q row:\n%s", want, out)
		}
	}
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
