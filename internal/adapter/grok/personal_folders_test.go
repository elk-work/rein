package grok

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/elk-work/rein/internal/adapter"
)

// fakeFolders makes the person's folders paths under a temporary home, so a
// test never names, let alone touches, the real ones.
func fakeFolders(t *testing.T, names ...string) (home string, paths []string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var folders []adapter.PersonalFolder
	for _, n := range names {
		p := filepath.Join(home, n)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		folders = append(folders, adapter.PersonalFolder{Name: n, Path: p})
		paths = append(paths, p)
	}
	was := personalFolders
	personalFolders = func(string, string) []adapter.PersonalFolder { return folders }
	t.Cleanup(func() { personalFolders = was })
	return home, paths
}

// pluginListGrok is a grok that answers `plugin list --json`, on any platform.
func pluginListGrok(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		bin := filepath.Join(dir, "grok.bat")
		if err := os.WriteFile(bin, []byte("@echo off\r\necho [{\"name\":\"context7\"}]\r\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return bin
	}
	bin := filepath.Join(dir, "grok")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '[{\"name\":\"context7\"}]'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestPermissionDeniesCoverEachFolderAndWhatIsInIt(t *testing.T) {
	_, paths := fakeFolders(t, "Documents")
	p := filepath.ToSlash(paths[0])
	want := []string{"Read(" + p + ")", "Read(" + p + "/**)", "Edit(" + p + ")", "Edit(" + p + "/**)"}
	if got := permissionDenies(paths); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Grok matches its rules against forward-slashed paths, and expands no `~`.
func TestWindowsFoldersAreWrittenForwardSlashed(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows paths")
	}
	got := permissionDenies([]string{`D:\OneDrive\Documents`})
	if got[1] != "Read(D:/OneDrive/Documents/**)" {
		t.Fatalf("got %v", got)
	}
}

func TestAQueueThatAllowsTheFoldersDeniesNone(t *testing.T) {
	fakeFolders(t, "Documents")
	h := &privateHome{dir: t.TempDir()}
	if got := h.deniedFolders(adapter.RunSpec{PersonalFolders: true}); got != nil {
		t.Fatalf("denied %v for a queue that allows the folders", got)
	}
	if got := h.deniedFolders(adapter.RunSpec{}); len(got) != 1 {
		t.Fatalf("denied %v, want Documents", got)
	}
}

// A folder holding the run's own private home stays open, or Grok could not
// read its configuration.
func TestAFolderHoldingThePrivateHomeIsLeftOpen(t *testing.T) {
	_, paths := fakeFolders(t, "Documents", "Music")
	h := &privateHome{dir: filepath.Join(paths[0], "rein", "grok-home")}
	if got := h.deniedFolders(adapter.RunSpec{}); !reflect.DeepEqual(got, paths[1:]) {
		t.Fatalf("denied %v, want only Music", got)
	}
}

func TestConfigureWritesTheFolderRules(t *testing.T) {
	_, paths := fakeFolders(t, "Documents", "Desktop")
	for _, allowed := range []bool{false, true} {
		h := &privateHome{dir: t.TempDir()}
		spec := adapter.RunSpec{WorktreeDir: t.TempDir(), PersonalFolders: allowed, Env: map[string]string{sandboxEnv: fullSandbox(runtime.GOOS)}}
		if err := h.configure(context.Background(), pluginListGrok(t), spec); err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Permission struct{ Deny []string }
		}
		if _, err := toml.DecodeFile(filepath.Join(h.dir, "config.toml"), &cfg); err != nil {
			t.Fatal(err)
		}
		want := permissionDenies(paths)
		if allowed {
			want = nil
		}
		if !reflect.DeepEqual(cfg.Permission.Deny, want) {
			t.Errorf("allowed=%v: permission.deny = %v, want %v", allowed, cfg.Permission.Deny, want)
		}
		if runtime.GOOS == "darwin" {
			continue // Rein's Seatbelt profile, not sandbox.toml (sandbox_darwin_test.go)
		}
		var sandbox struct {
			Profiles map[string]struct{ Deny []string }
		}
		if _, err := toml.DecodeFile(filepath.Join(h.dir, "sandbox.toml"), &sandbox); err != nil {
			t.Fatal(err)
		}
		deny := sandbox.Profiles[sandboxWorktree].Deny
		switch {
		case runtime.GOOS == "linux" && !allowed && !reflect.DeepEqual(deny, paths):
			t.Errorf("Linux profile deny = %v, want %v", deny, paths)
		case (runtime.GOOS != "linux" || allowed) && deny != nil:
			// Grok's sandbox does nothing on Windows: claiming a deny there
			// would pretend it is covered.
			t.Errorf("allowed=%v on %s: profile deny = %v, want none", allowed, runtime.GOOS, deny)
		}
	}
}

func TestGrokSandboxNamesOnlyFoldersItCanDenyLiterally(t *testing.T) {
	odd := filepath.Join(t.TempDir(), "OneDrive - [Elk]")
	if err := os.Mkdir(odd, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := profileDenies([]string{odd}); err == nil {
		t.Fatal("a folder Grok would read as a glob was accepted")
	}
	// A folder that is not there is not named: Grok would mount over it.
	got, err := profileDenies([]string{filepath.Join(t.TempDir(), "Movies")})
	if err != nil || got != nil {
		t.Fatalf("got %v, %v; want nothing for a missing folder", got, err)
	}
}
