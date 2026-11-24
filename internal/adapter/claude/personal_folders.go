package claude

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/elk-work/rein/internal/adapter"
)

// personalFolderDenies is the deny list that keeps a session out of the
// person's own folders (adapter.PersonalFolderPaths): Read and Edit rules for
// each folder and everything in it, which Claude Code applies in every
// permission mode, bypassPermissions included, to its file tools and to the
// file commands it recognises in Bash (cat, head, sed, ls, redirections). A
// command that reads without naming a file can still reach them, so this is a
// default, not a sandbox.
//
// A folder under home is written from home (`~/`); one elsewhere, such as a
// Windows Documents that OneDrive moved to another drive, from the root of the
// filesystem (`//`), in the POSIX form Claude Code matches Windows paths in
// (C:\x becomes /c/x).
//
// A folder holding the run's own worktree, or the repository it was cut
// from, is left open: the person chose to keep that code there, and denying
// it would lock the worker out of its own repository. When neither home nor
// any folder's place is known that exception cannot be worked out, so every
// rule is written: the rules name `~/`, which Claude Code resolves itself.
func personalFolderDenies(home, worktree string) []string {
	folders := personalFolderPaths(home)
	if len(folders) == 0 {
		var deny []string
		for _, folder := range adapter.PersonalFolders {
			deny = append(deny, denyRules("~/"+folder)...)
		}
		return deny
	}
	inUse := adapter.InUsePaths(worktree)
	var deny []string
	for _, f := range folders {
		if adapter.HoldsAny(f.Path, inUse) {
			continue
		}
		deny = append(deny, denyRules(ruleRoot(home, f.Path))...)
	}
	return deny
}

// personalFolderPaths is where the person's folders are; tests replace it.
var personalFolderPaths = adapter.PersonalFolderPaths

// denyRules covers the folder itself as well as what is in it: `F/**` alone
// leaves a listing of `F` unmatched, and a listing is what makes macOS ask.
func denyRules(root string) []string {
	return []string{"Read(" + root + ")", "Read(" + root + "/**)", "Edit(" + root + ")", "Edit(" + root + "/**)"}
}

// ruleRoot is how a rule names path: from home when it is under home, else
// from the filesystem root.
func ruleRoot(home, path string) string {
	if home != "" {
		h, p := adapter.FoldCase(filepath.Clean(home)), adapter.FoldCase(filepath.Clean(path))
		if strings.HasPrefix(p, h+string(filepath.Separator)) {
			return "~/" + filepath.ToSlash(filepath.Clean(path)[len(h)+1:])
		}
	}
	return "/" + posixPath(path)
}

// posixPath is path as Claude Code matches it: forward slashes, and on
// Windows a drive letter turned into a leading folder (C:\x is /c/x).
func posixPath(path string) string {
	path = filepath.Clean(path)
	if v := filepath.VolumeName(path); len(v) == 2 && v[1] == ':' {
		return "/" + strings.ToLower(v[:1]) + filepath.ToSlash(path[2:])
	}
	return filepath.ToSlash(path)
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
