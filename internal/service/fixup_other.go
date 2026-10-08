//go:build !linux

package service

// fixupInstall is a no-op everywhere but Linux, where the library's
// `systemd --user` unit needs one line corrected. See fixup_linux.go.
func fixupInstall(string) error { return nil }
