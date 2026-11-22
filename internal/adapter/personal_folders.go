package adapter

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
