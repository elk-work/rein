package claude

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
)

// personalFolderDenies is the deny list that keeps a session out of the
// person's own folders (adapter.PersonalFolders): a Read and an Edit rule each, written from home (`~/`), which
// Claude Code applies in every permission mode, bypassPermissions included,
// to its file tools and to the file commands it recognises in Bash (cat,
// head, sed, redirections). A command that reads without naming a file can
// still reach them, so this is a default, not a sandbox.
//
// A folder holding the run's own worktree, or the repository it was cut
// from, is left open: the person chose to keep that code there, and denying
// it would lock the worker out of its own repository.
func personalFolderDenies(home, worktree string) []string {
	if home == "" {
		return nil
	}
	inUse := []string{}
	if worktree != "" {
		inUse = append(inUse, worktree)
		if common := gitCommonDir(worktree); common != "" {
			inUse = append(inUse, common)
		}
	}
	var deny []string
	for _, folder := range adapter.PersonalFolders {
		abs := filepath.Join(home, folder)
		if holdsAny(abs, inUse) {
			continue
		}
		deny = append(deny, "Read(~/"+folder+"/**)", "Edit(~/"+folder+"/**)")
	}
	return deny
}

// holdsAny reports whether dir is, or contains, any of paths. Compared
// without regard to case, as a Mac disk is.
func holdsAny(dir string, paths []string) bool {
	d := strings.ToLower(filepath.Clean(dir))
	for _, p := range paths {
		for _, form := range []string{p, resolved(p)} {
			c := strings.ToLower(filepath.Clean(form))
			if c == d || strings.HasPrefix(c, d+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
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
