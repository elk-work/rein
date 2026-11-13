//go:build !windows

package worktree

// noHooks is the null device, for core.hooksPath: git looks for hooks under
// it and finds none, and unlike a directory nothing can ever be put there —
// not by an agent whose sandbox may write to the temporary directory, and not
// by anyone else (ark:rein#152's review).
func noHooks() string { return "/dev/null" }
