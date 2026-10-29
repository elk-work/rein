package capture

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Site screenshots one http(s) page with the chrome on PATH. The profile is
// a new directory, removed when chrome returns, so the shot cannot carry the
// person's cookies. chrome is launched with [PlanSite]'s flags, which keep it
// headless even when PATH does not point at Rein's wrapper.
func Site(rawURL, out string) error {
	out, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	// Reject a bad url before looking for chrome, so the error is about the
	// url and nothing is launched.
	if _, err := PlanSite("chrome", rawURL, out, string(os.PathSeparator)+"profile"); err != nil {
		return err
	}
	chrome, err := exec.LookPath("chrome")
	if err != nil {
		return fmt.Errorf("no chrome: rein browser install gives a run one")
	}
	profile, err := os.MkdirTemp("", "rein-capture-site-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profile)
	args, err := PlanSite(chrome, rawURL, out, profile)
	if err != nil {
		return err
	}
	cmd := exec.Command(chrome, args...)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("could not photograph the page: %v: %s", err, msg)
	}
	return nil
}
