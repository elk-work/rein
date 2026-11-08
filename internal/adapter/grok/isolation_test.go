package grok

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/elk-work/rein/internal/adapter"
)

func TestPluginNames(t *testing.T) {
	for _, data := range []string{`null`, `{}`, `[{"id":"missing-name"}]`, `broken`} {
		if _, err := pluginNames([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	got, err := pluginNames([]byte(`[{"name":"context7"},{"name":"quoted\"plugin"},{"name":"context7"}]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"context7", "quoted\"plugin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v != %v", got, want)
	}
	h := &privateHome{dir: t.TempDir()}
	if err := h.writeTOML("config.toml", map[string]any{"plugins": map[string]any{"disabled": got}}); err != nil {
		t.Fatal(err)
	}
	var decoded struct{ Plugins struct{ Disabled []string } }
	if _, err := toml.DecodeFile(filepath.Join(h.dir, "config.toml"), &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Plugins.Disabled, want) {
		t.Fatal(decoded)
	}
}

func TestGitWritePathsLinkedWorktree(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main")
	work := filepath.Join(root, "linked")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	git("init", main)
	git("-C", main, "-c", "user.name=Rein Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "initial")
	git("-C", main, "worktree", "add", "-b", "probe", work)
	paths, err := gitWritePaths(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	common, _ := filepath.EvalSymlinks(filepath.Join(main, ".git"))
	want := []string{filepath.Join(common, "objects"), filepath.Join(common, "refs"), filepath.Join(common, "logs", "refs"), filepath.Join(common, "worktrees", "linked")}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("%v != %v", paths, want)
	}

}

func TestConfigureDisablesPluginsAndBuildsProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX test executable")
	}
	h := &privateHome{dir: t.TempDir()}
	work := t.TempDir()
	sessions := t.TempDir()
	if err := os.Symlink(sessions, filepath.Join(h.dir, "sessions")); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "grok")
	script := "#!/bin/sh\n[ \"$1 $2 $3\" = 'plugin list --json' ] || exit 2\n[ \"$GROK_SANDBOX\" = read-only ] || exit 3\n[ \"$GROK_CLAUDE_MCPS_ENABLED\" = 0 ] || exit 4\nprintf '[{\"name\":\"context7\"},{\"name\":\"another\"}]'\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	spec := adapter.RunSpec{WorktreeDir: work, Env: map[string]string{sandboxEnv: sandboxWorktree, "GROK_HOME": "/wrong"}}
	if err := h.configure(context.Background(), binary, spec); err != nil {
		t.Fatal(err)
	}
	var cfg struct{ Plugins struct{ Disabled []string } }
	if _, err := toml.DecodeFile(filepath.Join(h.dir, "config.toml"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Plugins.Disabled, []string{"another", "context7"}) {
		t.Fatal(cfg)
	}
	var sandbox struct {
		Profiles map[string]struct {
			Extends   string
			ReadWrite []string `toml:"read_write"`
		}
	}
	if _, err := toml.DecodeFile(filepath.Join(h.dir, "sandbox.toml"), &sandbox); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(sessions)
	profile := sandbox.Profiles[sandboxWorktree]
	if profile.Extends != "workspace" || !reflect.DeepEqual(profile.ReadWrite, []string{resolved}) {
		t.Fatal(profile)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := h.configure(context.Background(), binary, spec); err == nil {
		t.Fatal("accepted failed plugin discovery")
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf null\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := h.configure(context.Background(), binary, spec); err == nil {
		t.Fatal("accepted malformed plugin discovery")
	}
}
