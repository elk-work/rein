package service

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// fixupInstall repairs the one thing kardianos/service gets wrong about a
// `systemd --user` unit: it writes `WantedBy=multi-user.target`, which is a
// *system* manager target. A user manager has no `multi-user.target`, so
// `systemctl --user enable` files the symlink somewhere nothing ever activates
// and the service silently does not start at login — installed, enabled, and
// dead, which is the hardest of the three to notice.
//
// The library's template is overridable, but only through its own mini
// template language, which would have to be maintained in step with upstream
// and cannot be exercised from a macOS or Windows test. Rewriting one line of
// the rendered unit is smaller, and wrong in a way that is visible: if the line
// is not there, this says so instead of pretending.
//
// Note for the runbook and not for the code: a user unit only runs while that
// user has a session, unless `loginctl enable-linger <user>` is set. docs/
// service.md says so.
func fixupInstall(unitPath string) error {
	if unitPath == "" {
		return nil
	}
	data, err := os.ReadFile(unitPath)
	if err != nil {
		return fmt.Errorf("service: read %s: %w", unitPath, err)
	}
	const wrong, right = "WantedBy=multi-user.target", "WantedBy=default.target"
	unit := string(data)
	if !strings.Contains(unit, wrong) {
		// Upstream fixed it, or a custom template is in use. Either way this
		// has nothing to do.
		return nil
	}
	unit = strings.ReplaceAll(unit, wrong, right)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("service: rewrite %s: %w", unitPath, err)
	}
	// reenable rebuilds the .wants symlink from the corrected [Install]
	// section; without it the old multi-user.target.wants link is what stays
	// on disk.
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "reenable", Name + ".service"},
	} {
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("service: systemctl %s: %w: %s",
				strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
