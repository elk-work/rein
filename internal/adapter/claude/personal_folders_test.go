package claude

import (
	"os"
	"path/filepath"
	"runtime"
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

// useHomeFolders puts the person's folders at adapter.PersonalFolders under
// home, as on a Mac, so a test means the same on every platform: on Windows
// the real lookup would find the CI machine's own known folders.
func useHomeFolders(t *testing.T) {
	useFolders(t, func(home string) []adapter.PersonalFolder {
		if home == "" {
			return nil
		}
		var out []adapter.PersonalFolder
		for _, f := range adapter.PersonalFolders {
			out = append(out, adapter.PersonalFolder{Name: f, Path: filepath.Join(home, filepath.FromSlash(f))})
		}
		return out
	})
}

func useFolders(t *testing.T, at func(string) []adapter.PersonalFolder) {
	was := personalFolderPaths
	personalFolderPaths = at
	t.Cleanup(func() { personalFolderPaths = was })
}

// Elk #1832: by default a worker stays out of the person's own folders, by a
// Read and an Edit deny rule for each, written from home so Claude Code
// applies them wherever the run's worktree is.
func TestPersonalFolderDeniesCoverEveryFolderByDefault(t *testing.T) {
	useHomeFolders(t)
	home := t.TempDir()
	worktree := filepath.Join(home, "dev", "app")
	deny := personalFolderDenies(home, worktree)
	for _, folder := range []string{"Documents", "Desktop", "Downloads", "Pictures", "Movies", "Music", "Library/Mobile Documents"} {
		// The folder itself too: a listing of ~/Desktop is what makes macOS ask.
		for _, rule := range []string{"Read(~/" + folder + ")", "Read(~/" + folder + "/**)", "Edit(~/" + folder + ")", "Edit(~/" + folder + "/**)"} {
			if !slices.Contains(deny, rule) {
				t.Errorf("missing %s in %v", rule, deny)
			}
		}
	}
	if slices.ContainsFunc(deny, func(r string) bool { return r == "Read(~/Library/**)" || r == "Read(~/Library)" }) {
		t.Error("all of Library is denied; agent tools need it")
	}
	// Unknown home: the exception cannot be worked out, so every rule holds.
	if got := personalFolderDenies("", filepath.Join("x", "Documents", "app")); !slices.Equal(got, deny) {
		t.Errorf("no home should still write every rule; got %v", got)
	}
}

// A run restarted after a stall keeps the list it started with, even if the
// agent rewrote its worktree's .git file to point into Documents meanwhile.
func TestARunKeepsTheDenyListItStartedWith(t *testing.T) {
	useHomeFolders(t)
	home := t.TempDir()
	worktree := filepath.Join(home, ".rein", "work", "run1")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	first := personalFolderDeniesForRun(home, worktree)
	if !slices.Contains(first, "Read(~/Documents/**)") {
		t.Fatalf("Documents not denied at start: %v", first)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+filepath.Join(home, "Documents", "x")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if again := personalFolderDeniesForRun(home, worktree); !slices.Equal(again, first) {
		t.Errorf("restart worked the list out again from the rewritten .git: %v", again)
	}
}

// Case is ignored only where the disk usually ignores it.
func TestCaseIsIgnoredOnlyOnMacAndWindows(t *testing.T) {
	useHomeFolders(t)
	home := t.TempDir()
	deny := personalFolderDenies(home, filepath.Join(home, "documents", "app"))
	opened := !slices.Contains(deny, "Read(~/Documents/**)")
	if want := runtime.GOOS == "darwin" || runtime.GOOS == "windows"; opened != want {
		t.Errorf("on %s a worktree in ~/documents opened Documents = %v, want %v", runtime.GOOS, opened, want)
	}
}

// A repository the person keeps in Documents stays reachable for its own
// worker, whether the run's worktree sits there or only the repository's git
// data does; the other folders are still denied.
func TestAFolderHoldingTheRunsOwnRepositoryIsLeftOpen(t *testing.T) {
	useHomeFolders(t)
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
	common := filepath.Join(home, "Documents", "GitHub", "site", ".git")
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
		t.Errorf("repository data in Documents was denied: %v", deny)
	}
}

// The rules reach Claude Code through the one --settings file, beside the
// watchdog's hooks; a queue that allows personal folders gets none.
func TestBuildArgsWritesTheFolderRulesUnlessTheQueueAllowsThem(t *testing.T) {
	useHomeFolders(t)
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

// A folder outside home, such as a Windows Documents that OneDrive moved to
// another drive, is denied by its absolute path from the filesystem root; a
// folder moved within home is still written from home.
func TestAFolderOutsideHomeIsDeniedByItsRealPath(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	moved := filepath.Join(elsewhere, "OneDrive", "Documents")
	useFolders(t, func(string) []adapter.PersonalFolder {
		return []adapter.PersonalFolder{
			{Name: "Documents", Path: moved},
			{Name: "Pictures", Path: filepath.Join(home, "OneDrive", "Pictures")},
		}
	})
	deny := personalFolderDenies(home, filepath.Join(home, "dev", "app"))
	root := "/" + posixPath(moved)
	for _, want := range denyRules(root) {
		if !slices.Contains(deny, want) {
			t.Errorf("missing %s in %v", want, deny)
		}
	}
	if !strings.HasPrefix(root, "//") || strings.Contains(root, `\`) || strings.Contains(root, ":") {
		t.Errorf("rule root %q is not a POSIX path from the filesystem root", root)
	}
	if !slices.Contains(deny, "Read(~/OneDrive/Pictures/**)") {
		t.Errorf("Pictures moved within home should be written from home: %v", deny)
	}

	// A repository kept in the moved Documents leaves it open.
	deny = personalFolderDenies(home, filepath.Join(moved, "GitHub", "site"))
	if slices.Contains(deny, "Read("+root+"/**)") {
		t.Errorf("worktree in the moved Documents was denied: %v", deny)
	}
}
