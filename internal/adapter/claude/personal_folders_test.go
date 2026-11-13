package claude

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

func specFor(t *testing.T, personalFolders bool) adapter.RunSpec {
	return adapter.RunSpec{
		WorktreeDir:     t.TempDir(),
		Prompt:          "do the thing",
		PermissionMode:  adapter.PermissionFull,
		PersonalFolders: personalFolders,
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// Elk #1832: by default a worker stays out of the person's own folders, by a
// Read and an Edit deny rule for each, written from home so Claude Code
// applies them wherever the run's worktree is.
func TestPersonalFolderDeniesCoverEveryFolderByDefault(t *testing.T) {
	home := t.TempDir()
	worktree := filepath.Join(home, "dev", "app")
	deny := personalFolderDenies(home, worktree)
	for _, folder := range []string{"Documents", "Desktop", "Downloads", "Pictures", "Movies", "Music", "Library/Mobile Documents"} {
		for _, rule := range []string{"Read(~/" + folder + "/**)", "Edit(~/" + folder + "/**)"} {
			if !slices.Contains(deny, rule) {
				t.Errorf("missing %s in %v", rule, deny)
			}
		}
	}
	if slices.ContainsFunc(deny, func(r string) bool { return r == "Read(~/Library/**)" }) {
		t.Error("all of Library is denied; agent tools need it")
	}
	if personalFolderDenies("", worktree) != nil {
		t.Error("no home should mean no rules, not rules anchored at /")
	}
}

// A repository the person keeps in Documents stays reachable for its own
// worker, whether the run's worktree sits there or only the repository's git
// data does; the other folders are still denied.
func TestAFolderHoldingTheRunsOwnRepositoryIsLeftOpen(t *testing.T) {
	home := t.TempDir()
	inDocs := filepath.Join(home, "Documents", "GitHub", "site")
	deny := personalFolderDenies(home, inDocs)
	if slices.Contains(deny, "Read(~/Documents/**)") {
		t.Errorf("worktree in Documents was denied: %v", deny)
	}
	if !slices.Contains(deny, "Read(~/Desktop/**)") {
		t.Errorf("Desktop should still be denied: %v", deny)
	}

	// A worktree outside Documents whose repository lives in it.
	common := filepath.Join(home, "documents", "GitHub", "site", ".git")
	gitDir := filepath.Join(common, "worktrees", "run1")
	worktree := filepath.Join(home, ".rein", "work", "run1")
	for _, d := range []string{gitDir, worktree} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deny = personalFolderDenies(home, worktree)
	if slices.Contains(deny, "Read(~/Documents/**)") {
		t.Errorf("repository data in Documents (spelled lower case) was denied: %v", deny)
	}
}

// The rules reach Claude Code through the one --settings file, beside the
// watchdog's hooks; a queue that allows personal folders gets none.
func TestBuildArgsWritesTheFolderRulesUnlessTheQueueAllowsThem(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &Adapter{DisableWatchdog: true}

	s := &Session{tmpDir: t.TempDir()}
	args, err := a.buildArgs(specFor(t, false), "bypassPermissions", s)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	i := slices.Index(args, "--settings")
	if i < 0 {
		t.Fatalf("no --settings in %v", args)
	}
	body := readFile(t, args[i+1])
	for _, want := range []string{`"deny"`, `Read(~/Documents/**)`} {
		if !contains(body, want) {
			t.Errorf("settings lack %s:\n%s", want, body)
		}
	}

	s = &Session{tmpDir: t.TempDir()}
	args, err = a.buildArgs(specFor(t, true), "bypassPermissions", s)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	if slices.Contains(args, "--settings") {
		t.Errorf("allowed queue still got a settings file: %v", args)
	}
}
