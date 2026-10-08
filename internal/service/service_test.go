package service_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/service"
)

// These tests build the service definition and read it back. None of them
// installs anything: a service is a machine-wide object in the real home
// directory, and a test that wrote one would be editing the developer's login
// items.

func TestNewBuildsTheRunCommand(t *testing.T) {
	home := t.TempDir()
	m, err := service.New(service.Config{Home: home, Executable: "/opt/rein/rein"})
	if err != nil {
		t.Fatal(err)
	}

	args := m.Arguments()
	if len(args) == 0 || args[0] != "run" {
		t.Fatalf("arguments = %v, want them to start with \"run\"", args)
	}
	// No --once, ever: the service is the daemon. A --once service would
	// claim one run, exit, and be restarted by KeepAlive in a loop.
	for _, a := range args {
		if a == "--once" {
			t.Errorf("the service runs with --once: %v", args)
		}
		if a == "--queue" {
			t.Errorf("the service pins a single queue: %v", args)
		}
	}
	if !contains(args, "--service") {
		t.Errorf("the service does not run with --service, so Windows will kill it: %v", args)
	}
	wantLog := filepath.Join(home, service.LogDirName, service.LoopLogName)
	if !contains(args, wantLog) {
		t.Errorf("arguments do not carry the log path %s: %v", wantLog, args)
	}
	if m.Executable() != "/opt/rein/rein" {
		t.Errorf("executable = %q, want the one we asked for", m.Executable())
	}
}

func TestConfigPathIsPassedThroughOnlyWhenGiven(t *testing.T) {
	home := t.TempDir()

	m, err := service.New(service.Config{Home: home, Executable: "rein"})
	if err != nil {
		t.Fatal(err)
	}
	if contains(m.Arguments(), "--config") {
		t.Errorf("an unset --config was baked into the service: %v", m.Arguments())
	}

	m, err = service.New(service.Config{Home: home, Executable: "rein", ConfigPath: "/tmp/other.toml"})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(m.Arguments(), "/tmp/other.toml") {
		t.Errorf("an explicit --config was dropped: %v", m.Arguments())
	}
}

func TestEnvironmentCarriesPath(t *testing.T) {
	// The whole reason this exists: a launchd agent inherits
	// /usr/bin:/bin:/usr/sbin:/sbin, so without this the service can see none
	// of the coding-agent CLIs and no gh.
	t.Setenv("PATH", "/opt/homebrew/bin:/usr/bin:/bin")
	m, err := service.New(service.Config{Home: t.TempDir(), Executable: "rein"})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, e := range m.Environment() {
		if strings.HasPrefix(e, "PATH=") {
			got = e
		}
	}
	if got != "PATH=/opt/homebrew/bin:/usr/bin:/bin" {
		t.Errorf("PATH = %q, want the installing shell's", got)
	}
}

func TestEnvironmentCarriesTheReinVariables(t *testing.T) {
	t.Setenv("REIN_KEYRING", "file")
	t.Setenv("REIN_HOST_CAPABILITIES", "github-cli,xcode")
	m, err := service.New(service.Config{Home: t.TempDir(), Executable: "rein"})
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(m.Environment(), "\n")
	for _, want := range []string{"REIN_KEYRING=file", "REIN_HOST_CAPABILITIES=github-cli,xcode"} {
		if !strings.Contains(env, want) {
			t.Errorf("the service environment is missing %q:\n%s", want, env)
		}
	}
}

func TestEnvironmentCarriesNothingElse(t *testing.T) {
	// A launchd plist is world-readable and a Windows service's environment is
	// a registry value, so anything copied in stops being private. The list is
	// deliberately closed.
	t.Setenv("REIN_ELK_TOKEN", "sk-not-a-real-token")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "also-not-real")
	m, err := service.New(service.Config{Home: t.TempDir(), Executable: "rein"})
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(m.Environment(), "\n")
	for _, leaked := range []string{"REIN_ELK_TOKEN", "AWS_SECRET_ACCESS_KEY"} {
		if strings.Contains(env, leaked) {
			t.Errorf("%s reached the service definition:\n%s", leaked, env)
		}
	}
}

func TestLogPathsAreUnderTheReinHome(t *testing.T) {
	home := t.TempDir()
	m, err := service.New(service.Config{Home: home, Executable: "rein"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, service.LogDirName)
	if m.LogDir() != want {
		t.Errorf("log dir = %q, want %q", m.LogDir(), want)
	}
	paths := m.LogPaths()
	if len(paths) != 3 {
		t.Fatalf("log paths = %v, want the loop log and the manager's two", paths)
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, want) {
			t.Errorf("%s is not under %s", p, want)
		}
	}
}

func TestNewRefusesWithoutAHome(t *testing.T) {
	if _, err := service.New(service.Config{}); err == nil {
		t.Fatal("a service with no Rein home was accepted")
	}
}

func TestDefinitionPathIsThePlatformsOwn(t *testing.T) {
	m, err := service.New(service.Config{Home: t.TempDir(), Executable: "rein"})
	if err != nil {
		t.Fatal(err)
	}
	p := m.DefinitionPath()
	switch runtime.GOOS {
	case "darwin":
		if !strings.HasSuffix(p, filepath.Join("Library", "LaunchAgents", service.Name+".plist")) {
			t.Errorf("definition path = %q, want a LaunchAgents plist", p)
		}
	case "linux":
		if !strings.HasSuffix(p, filepath.Join("systemd", "user", service.Name+".service")) {
			t.Errorf("definition path = %q, want a systemd --user unit", p)
		}
	default:
		if p != "" {
			t.Errorf("definition path = %q, want empty — Windows keeps no file", p)
		}
	}
}

func TestStatusString(t *testing.T) {
	for _, tc := range []struct {
		st   service.Status
		want string
	}{
		{service.Status{}, "not installed"},
		{service.Status{Installed: true}, "installed, not running"},
		{service.Status{Installed: true, Running: true}, "running"},
	} {
		if got := tc.st.String(); got != tc.want {
			t.Errorf("%+v = %q, want %q", tc.st, got, tc.want)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
