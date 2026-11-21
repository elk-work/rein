package runner

import (
	"os"
	"path/filepath"
	"runtime"
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
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("folder modes do not block reading here")
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

// Every checkout a served queue can cut a run from is opened at start,
// whether or not the queue allows personal folders, so a repository kept in
// Documents makes macOS ask then and not at the first `git worktree add`.
// Paths that do not exist are skipped; one path named twice is opened once.
func TestRepositoryFoldersAreOpenedAtStart(t *testing.T) {
	home := t.TempDir()
	inDocs := filepath.Join(home, "Documents", "code", "app")
	other := filepath.Join(home, "dev", "tool")
	for _, d := range []string{filepath.Join(inDocs, ".git"), other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{
		Repos: map[string]string{
			"app":     inDocs,
			"tool":    other,
			"missing": filepath.Join(home, "gone"),
		},
	}
	queues := []config.Queue{{Name: "a"}, {Name: "b", Repo: inDocs}}
	opened, refused := openRepositoryFolders(cfg, queues)
	if !slices.Equal(opened, []string{inDocs, other}) {
		t.Errorf("opened %v, want %v and %v once each", opened, inDocs, other)
	}
	if refused != nil {
		t.Errorf("refused %v; a missing checkout is skipped, not refused", refused)
	}
}

// A scoped config opens only the repositories its served queues may use,
// and the unscoped default repository is not one of them.
func TestRepositoryFoldersFollowTheQueueScope(t *testing.T) {
	home := t.TempDir()
	mine, theirs, def := filepath.Join(home, "mine"), filepath.Join(home, "theirs"), filepath.Join(home, "default")
	for _, d := range []string{mine, theirs, def} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{
		DefaultRepo: def,
		Repos:       map[string]string{"mine": mine, "theirs": theirs},
		Queues: []config.Queue{
			{Name: "a", Workspace: "one", Repos: []string{"mine"}},
			{Name: "b", Workspace: "two", Repos: []string{"theirs"}},
		},
	}
	opened, _ := openRepositoryFolders(cfg, cfg.Queues[:1])
	if !slices.Equal(opened, []string{mine}) {
		t.Errorf("opened %v, want only %v", opened, mine)
	}
}

// A checkout that cannot be read is reported, so the log says why runs on
// it will fail.
func TestARefusedRepositoryFolderIsReported(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("folder modes do not block reading here")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(repo, 0o755) })
	_, refused := openRepositoryFolders(config.Config{Repos: map[string]string{"r": repo}}, []config.Queue{{Name: "a"}})
	if !slices.Equal(refused, []string{repo}) {
		t.Errorf("refused %v, want %v", refused, repo)
	}
}
