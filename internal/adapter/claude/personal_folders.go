package claude

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/elk-work/rein/internal/adapter"
)

// personalFolderDenies is the deny list that keeps a session out of the
// person's own folders (adapter.PersonalFolders): Read and Edit rules for
// each folder and everything in it, written from home (`~/`), which Claude
// Code applies in every permission mode, bypassPermissions included, to its
// file tools and to the file commands it recognises in Bash (cat, head, sed,
// ls, redirections). A command that reads without naming a file can still
// reach them, so this is a default, not a sandbox.
//
// A folder holding the run's own worktree, or the repository it was cut
// from, is left open: the person chose to keep that code there, and denying
// it would lock the worker out of its own repository. When home is unknown
// that exception cannot be worked out, so every rule is written: the rules
// name `~/`, which Claude Code resolves itself.
func personalFolderDenies(home, worktree string) []string {
	inUse := []string{}
	if home != "" && worktree != "" {
		inUse = append(inUse, worktree)
		if common := gitCommonDir(worktree); common != "" {
			inUse = append(inUse, common)
		}
	}
	var deny []string
	for _, folder := range adapter.PersonalFolders {
		if home != "" && holdsAny(filepath.Join(home, folder), inUse) {
			continue
		}
		// The folder itself as well as what is in it: `~/F/**` alone leaves
		// a listing of `~/F` unmatched, and a listing is what makes macOS ask.
		deny = append(deny,
			"Read(~/"+folder+")", "Read(~/"+folder+"/**)",
			"Edit(~/"+folder+")", "Edit(~/"+folder+"/**)")
	}
	return deny
}

// runDenies remembers each worktree's deny list for the life of the process,
// so a run restarted after a stall (or a review resumed) keeps the list it
// started with. The exception reads the worktree's `.git` file, which the
// agent can rewrite once it is running; working it out again then would let
// the agent open a folder by editing that file.
var runDenies sync.Map // home + "\x00" + worktree -> []string

func personalFolderDeniesForRun(home, worktree string) []string {
	key := home + "\x00" + worktree
	if v, ok := runDenies.Load(key); ok {
		return v.([]string)
	}
	v, _ := runDenies.LoadOrStore(key, personalFolderDenies(home, worktree))
	return v.([]string)
}

// holdsAny reports whether dir is, or contains, any of paths, comparing both
// as written and with symlinks resolved. Case is ignored only where the disk
// usually ignores it (macOS, Windows).
func holdsAny(dir string, paths []string) bool {
	for _, d := range []string{dir, resolved(dir)} {
		d = fold(filepath.Clean(d))
		for _, p := range paths {
			for _, form := range []string{p, resolved(p)} {
				c := fold(filepath.Clean(form))
				if c == d || strings.HasPrefix(c, d+string(filepath.Separator)) {
					return true
				}
			}
		}
	}
	return false
}

func fold(p string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// gitCommonDir is where a worktree's repository keeps its data: the `.git`
// file's `gitdir:` target, then that directory's `commondir`. Empty when the
// worktree is an ordinary checkout or cannot be read.
func gitCommonDir(worktree string) string {
	b, err := os.ReadFile(filepath.Join(worktree, ".git"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, "gitdir:") {
		return ""
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktree, gitDir)
	}
	if c, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common := strings.TrimSpace(string(c))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		return filepath.Clean(common)
	}
	return filepath.Clean(gitDir)
}
