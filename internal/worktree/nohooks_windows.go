package worktree

// noHooks is the null device by its absolute device path. The bare name
// `NUL` would be a relative path to git, and a folder of that name can be made
// with the `\\?\` prefix; `\\.\NUL` cannot hold files.
func noHooks() string { return `\\.\NUL` }
