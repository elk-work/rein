package runner

import (
	"os"
	"path/filepath"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
)

// openPersonalFolders lists each of the person's own folders once when any
// queue this process serves allows them, and reports which it opened and
// which macOS (or anything else) refused. Listing is what makes macOS ask, so
// doing it as the daemon starts puts the question there rather than in the
// middle of a run (Elk #1832). The caller runs it off the start path: macOS
// can hold the listing until the person answers.
func openPersonalFolders(queues []config.Queue, home func() (string, error)) (opened, refused []string) {
	allowed := false
	for _, q := range queues {
		allowed = allowed || q.PersonalFolders
	}
	if !allowed {
		return nil, nil
	}
	h, err := home()
	if err != nil || h == "" {
		return nil, nil
	}
	for _, folder := range adapter.PersonalFolders {
		path := filepath.Join(h, folder)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		if _, err := os.ReadDir(path); err != nil {
			refused = append(refused, folder)
		} else {
			opened = append(opened, folder)
		}
	}
	return opened, refused
}
