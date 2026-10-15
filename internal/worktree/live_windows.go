//go:build windows

package worktree

// processesIn is not implemented on Windows, where a directory in use cannot
// be removed anyway: the reap fails and says so.
func processesIn(string) ([]string, error) { return nil, nil }
