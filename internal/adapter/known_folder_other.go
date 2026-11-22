//go:build !windows

package adapter

// knownFolder finds nothing off Windows: there the folders are where
// PersonalFolders says, under home.
func knownFolder(string) string { return "" }
