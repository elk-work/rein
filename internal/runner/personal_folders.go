package runner

import (
	"os"
	"path/filepath"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
)

// openAllowedPersonalFolders lists each of the person's own folders once when
// any queue allows them, and returns the ones it opened. Listing is what makes
// macOS ask, so doing it at start puts the question right after the press
// that allowed it rather than in the middle of a run (Elk #1832).
func openAllowedPersonalFolders(cfg config.Config) []string {
	return openPersonalFolders(cfg, os.UserHomeDir)
}

func openPersonalFolders(cfg config.Config, home func() (string, error)) []string {
	allowed := false
	for _, q := range cfg.Queues {
		allowed = allowed || q.PersonalFolders
	}
	if !allowed {
		return nil
	}
	h, err := home()
	if err != nil || h == "" {
		return nil
	}
	var opened []string
	for _, folder := range adapter.PersonalFolders {
		if _, err := os.ReadDir(filepath.Join(h, folder)); err == nil {
			opened = append(opened, folder)
		}
	}
	return opened
}
