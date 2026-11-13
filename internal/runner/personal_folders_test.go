package runner

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/elk-work/rein/internal/config"
)

// Nothing is opened unless a queue allows it; when one does, each folder
// that exists is listed once at start (Elk #1832).
func TestPersonalFoldersAreOpenedOnlyWhenAQueueAllowsThem(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{"Documents", "Desktop"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	at := func() (string, error) { return home, nil }

	off := config.Config{Queues: []config.Queue{{Name: "a"}}}
	if got := openPersonalFolders(off, at); got != nil {
		t.Errorf("opened %v with no queue allowing it", got)
	}
	on := config.Config{Queues: []config.Queue{{Name: "a"}, {Name: "b", PersonalFolders: true}}}
	got := openPersonalFolders(on, at)
	if !slices.Equal(got, []string{"Documents", "Desktop"}) {
		t.Errorf("opened %v, want Documents and Desktop", got)
	}
}
