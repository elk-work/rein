package adapter

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// PersonalFolders are the person's own folders, relative to home, that a
// worker stays out of unless its queue allows them (config
// Queue.PersonalFolders; Elk #1832, Casey's ruling 2026-11-12). macOS guards
// each one and asks, with no warning and no reason Elk can show, the moment a
// background agent touches it. Library as a whole is left alone: agent tools
// read caches and settings there. Only iCloud Drive inside it is a person's
// files.
var PersonalFolders = []string{
	"Documents", "Desktop", "Downloads", "Pictures", "Movies", "Music",
	"Library/Mobile Documents",
}

// WindowsPersonalFolders are the same folders as Windows names them. Each is
// a known folder that the person, or OneDrive's folder backup, can move out
// of the profile (Documents to C:\Users\me\OneDrive\Documents, say), so
// their real place is looked up, not assumed (follow-up of the review on
// rein#86).
var WindowsPersonalFolders = []string{
	"Documents", "Desktop", "Downloads", "Pictures", "Videos", "Music",
}

// oneDriveVariables name OneDrive's own folders, the Windows counterpart of
// iCloud Drive: OneDrive sets them for the signed-in person, one per account.
var oneDriveVariables = []string{"OneDrive", "OneDriveConsumer", "OneDriveCommercial"}

// PersonalFolder is one of the person's own folders, where it really is.
type PersonalFolder struct {
	Name string // as the person knows it: "Documents", "OneDrive"
	Path string // absolute
}

// PersonalFolderPaths is where the person's own folders are on this machine.
// On macOS and Linux that is PersonalFolders under home, or nothing when home
// is unknown. On Windows it is each known folder where Windows says it is now,
// its usual place in the profile as well when that differs (a folder moved
// can leave files behind), and OneDrive's own folders.
func PersonalFolderPaths(home string) []PersonalFolder {
	return personalFolderPaths(runtime.GOOS, home, knownFolder, os.Getenv)
}

// personalFolderPaths is PersonalFolderPaths with the platform, the known
// folder lookup and the environment passed in, so every platform's answer is
// testable on any. known returns "" for a folder it cannot find.
func personalFolderPaths(goos, home string, known, getenv func(string) string) []PersonalFolder {
	var out []PersonalFolder
	seen := map[string]bool{}
	add := func(name, path string) {
		if path == "" || !filepath.IsAbs(path) {
			return
		}
		path = filepath.Clean(path)
		key := path
		if goos == "windows" || goos == "darwin" {
			key = strings.ToLower(path)
		}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, PersonalFolder{Name: name, Path: path})
	}
	if goos != "windows" {
		if home == "" {
			return nil
		}
		for _, f := range PersonalFolders {
			add(f, filepath.Join(home, filepath.FromSlash(f)))
		}
		return out
	}
	for _, f := range WindowsPersonalFolders {
		add(f, known(f))
		if home != "" {
			add(f, filepath.Join(home, f))
		}
	}
	for _, v := range oneDriveVariables {
		add("OneDrive", getenv(v))
	}
	return out
}

// DeniedPersonalFolders is the person's own folders a run on worktree stays
// out of: PersonalFolderPaths, less any folder holding the worktree or the
// repository it was cut from. The person chose to keep that code there, and
// denying it would lock the worker out of its own repository.
func DeniedPersonalFolders(home, worktree string) []PersonalFolder {
	inUse := InUsePaths(worktree)
	var out []PersonalFolder
	for _, f := range PersonalFolderPaths(home) {
		if !HoldsAny(f.Path, inUse) {
			out = append(out, f)
		}
	}
	return out
}

// runDenied remembers each worktree's folders for the life of the process,
// so a run restarted after a stall (or a review resumed) keeps the list it
// started with. The exception reads the worktree's `.git` file, which the
// agent can rewrite once it is running; working it out again then would let
// the agent open a folder by editing that file.
var runDenied sync.Map // home + "\x00" + worktree -> []PersonalFolder

// DeniedPersonalFoldersForRun is DeniedPersonalFolders, worked out once per
// worktree.
func DeniedPersonalFoldersForRun(home, worktree string) []PersonalFolder {
	key := home + "\x00" + worktree
	if v, ok := runDenied.Load(key); ok {
		return v.([]PersonalFolder)
	}
	v, _ := runDenied.LoadOrStore(key, DeniedPersonalFolders(home, worktree))
	return v.([]PersonalFolder)
}

// InUsePaths is the worktree and the repository it was cut from (its git
// common directory), the paths a personal folder holding them is left open
// for. Empty for an empty worktree.
func InUsePaths(worktree string) []string {
	if worktree == "" {
		return nil
	}
	inUse := []string{worktree}
	if common := GitCommonDir(worktree); common != "" {
		inUse = append(inUse, common)
	}
	return inUse
}

// HoldsAny reports whether dir is, or contains, any of paths, comparing both
// as written and with symlinks resolved. Case is ignored only where the disk
// usually ignores it (macOS, Windows).
func HoldsAny(dir string, paths []string) bool {
	for _, d := range []string{dir, Resolved(dir)} {
		d = FoldCase(filepath.Clean(d))
		for _, p := range paths {
			for _, form := range []string{p, Resolved(p)} {
				c := FoldCase(filepath.Clean(form))
				if c == d || strings.HasPrefix(c, d+string(filepath.Separator)) {
					return true
				}
			}
		}
	}
	return false
}

// FoldCase is p as this disk compares it: lower case where the disk usually
// ignores case (macOS, Windows), as written elsewhere.
func FoldCase(p string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// Resolved is p with symlinks resolved, or p when it cannot be.
func Resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// GitCommonDir is where a worktree's repository keeps its data: the `.git`
// file's `gitdir:` target, then that directory's `commondir`. Empty when the
// worktree is an ordinary checkout or cannot be read.
func GitCommonDir(worktree string) string {
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
