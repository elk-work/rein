package runner

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/elk-work/rein/internal/config"
)

// Nothing is opened unless a queue this process serves allows it; when one
// does, each folder that exists is listed once at start (Elk #1832).
func TestPersonalFoldersAreOpenedOnlyWhenAQueueAllowsThem(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{"Documents", "Desktop"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	at := func() (string, error) { return home, nil }

	if got, _ := openPersonalFolders([]config.Queue{{Name: "a"}}, at); got != nil {
		t.Errorf("opened %v with no queue allowing it", got)
	}
	got, refused := openPersonalFolders([]config.Queue{{Name: "a"}, {Name: "b", PersonalFolders: true}}, at)
	if !slices.Equal(got, []string{"Documents", "Desktop"}) {
		t.Errorf("opened %v, want Documents and Desktop", got)
	}
	if refused != nil {
		t.Errorf("refused %v; missing folders are skipped, not refused", refused)
	}
}

// A folder that exists but cannot be read is reported, so the log says why
// the queue's agents later cannot reach it.
func TestARefusedPersonalFolderIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any folder")
	}
	home := t.TempDir()
	docs := filepath.Join(home, "Documents")
	if err := os.MkdirAll(docs, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(docs, 0o755) })
	_, refused := openPersonalFolders([]config.Queue{{Name: "b", PersonalFolders: true}}, func() (string, error) { return home, nil })
	if !slices.Equal(refused, []string{"Documents"}) {
		t.Errorf("refused %v, want Documents", refused)
	}
}
