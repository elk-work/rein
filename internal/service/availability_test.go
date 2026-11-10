//go:build !windows

package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserManagerAvailable(t *testing.T) {
	for _, tc := range []struct {
		name, script       string
		available, wantErr bool
	}{
		{"available", "exit 0", true, false},
		{"no bus", "echo 'Failed to connect to bus: No medium found' >&2; exit 1", false, false},
		{"no systemd", "echo 'System has not been booted with systemd as init system' >&2; exit 1", false, false},
		{"permission denied", "echo 'Failed to connect to bus: Permission denied' >&2; exit 1", false, true},
		{"unknown failure", "exit 1", false, true},
		{"missing binary", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.script != "" {
				if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			got, err := userManagerAvailable()
			if got != tc.available || (err != nil) != tc.wantErr {
				t.Fatalf("available=%v err=%v", got, err)
			}
		})
	}
}
