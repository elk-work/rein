package runner_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runner"
)

func TestResolveRepo(t *testing.T) {
	cfg := config.Config{
		DefaultRepo: "/dev/elk",
		Repos: map[string]string{
			"scout":          "/dev/elk/scout",
			"elk-work/rein":  "/dev/rein",
			"elk-work/pulse": "/dev/elk/pulse",
		},
	}
	queue := config.Queue{Name: "mac-claude", Repo: "/dev/queue-default"}

	for _, tc := range []struct {
		name       string
		order      string
		queue      config.Queue
		wantPath   string
		wantSource runner.RepoSource
	}{
		{
			name:       "an explicit repo: line wins over everything",
			order:      "**Direction:** fix it\nrepo: scout\n",
			queue:      queue,
			wantPath:   "/dev/elk/scout",
			wantSource: runner.RepoFromHint,
		},
		{
			name:       "a repo: line may be backticked and bold, as a person would write it",
			order:      "**Repo:** `elk-work/rein`\n",
			queue:      queue,
			wantPath:   "/dev/rein",
			wantSource: runner.RepoFromHint,
		},
		{
			name:       "an owner/name hint matches a bare key by its last segment",
			order:      "repo: elk-work/scout\n",
			queue:      queue,
			wantPath:   "/dev/elk/scout",
			wantSource: runner.RepoFromHint,
		},
		{
			name:       "a github URL is the second-best signal",
			order:      "See https://github.com/elk-work/pulse/pull/12 for context.\n",
			queue:      queue,
			wantPath:   "/dev/elk/pulse",
			wantSource: runner.RepoFromURL,
		},
		{
			name:       "the queue's own repo when the order says nothing",
			order:      "**Direction:** tidy the docs\n",
			queue:      queue,
			wantPath:   "/dev/queue-default",
			wantSource: runner.RepoFromQueue,
		},
		{
			name:       "default_repo last",
			order:      "**Direction:** tidy the docs\n",
			queue:      config.Queue{Name: "mac-claude"},
			wantPath:   "/dev/elk",
			wantSource: runner.RepoFromDefault,
		},
		{
			name:       "a sentence mentioning a repository is not a hint",
			order:      "The repository was renamed last week.\n",
			queue:      queue,
			wantPath:   "/dev/queue-default",
			wantSource: runner.RepoFromQueue,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runner.ResolveRepo(cfg, tc.queue, tc.order)
			if err != nil {
				t.Fatal(err)
			}
			if got.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", got.Path, tc.wantPath)
			}
			if got.Source != tc.wantSource {
				t.Errorf("source = %q, want %q", got.Source, tc.wantSource)
			}
		})
	}
}

func TestResolveRepoRefusesRatherThanGuessing(t *testing.T) {
	// A runner that picks the wrong repository does the work in the wrong
	// place and reports success, so an unresolvable name is a `stuck`, never
	// a fallback.
	cfg := config.Config{Repos: map[string]string{"scout": "/dev/elk/scout"}}
	_, err := runner.ResolveRepo(cfg, config.Queue{Name: "mac-claude"}, "repo: signal\n")

	var unresolved *runner.UnresolvedRepoError
	if !errors.As(err, &unresolved) {
		t.Fatalf("err = %v, want *UnresolvedRepoError", err)
	}
	msg := err.Error()
	for _, want := range []string{`"signal"`, "repo: <name or path>", "mac-claude", "scout"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the stuck reason does not mention %q:\n%s", want, msg)
		}
	}
}

func TestResolveRepoWithNothingConfiguredAtAll(t *testing.T) {
	_, err := runner.ResolveRepo(config.Config{}, config.Queue{Name: "mac-claude"}, "**Direction:** do it\n")
	if err == nil {
		t.Fatal("a run with no repository anywhere was accepted")
	}
	if !strings.Contains(err.Error(), "Elk's work order carries no repository field") {
		t.Errorf("the reason does not say why Rein had to ask: %v", err)
	}
}

func TestWorkspaceRepositoryBoundary(t *testing.T) {
	scout := config.Queue{Name: "mac-claude", Repos: []string{"elk-work/scout"}}
	gallery := config.Queue{Name: "mac-claude", Workspace: "Gallery", Repos: []string{"friend/gallery"}}
	cfg := config.Config{Workspace: "Scout", DefaultRepo: "/dev/scout", Repos: map[string]string{"elk-work/scout": "/dev/scout", "friend/gallery": "/dev/gallery"}, Queues: []config.Queue{scout, gallery}}
	for _, text := range []string{"repo: friend/gallery", "repo: /dev/gallery", "https://github.com/friend/gallery/pull/1", "repo: other/scout"} {
		if _, err := runner.ResolveRepo(cfg, scout, text); err == nil || !strings.Contains(err.Error(), "not declared") {
			t.Fatalf("accepted %q: %v", text, err)
		}
	}
	if got, err := runner.ResolveRepo(cfg, scout, "repo: elk-work/scout"); err != nil || got.Path != "/dev/scout" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := runner.ResolveRepo(cfg, gallery, "no repo"); err == nil {
		t.Fatal("used machine default")
	}
	// One list anywhere scopes the whole config: Gallery, with none of its
	// own, has no repositories and no default.
	cfg.Queues[1].Repos = nil
	gallery.Repos = nil
	if _, err := runner.ResolveRepo(cfg, gallery, "repo: friend/gallery"); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("a workspace without a list reached a repository: %v", err)
	}
	if _, err := runner.ResolveRepo(cfg, gallery, "no repo"); err == nil {
		t.Fatal("a scoped config used the machine default")
	}
}

// TestCompatibilityModeKeepsTheOldRules is ark:rein#67: a multi-workspace
// config with no list anywhere is read the way it was written, before v0.8.4 —
// the top-level map for every queue and default_repo for a run that names no
// repository — instead of stranding every run.
func TestCompatibilityModeKeepsTheOldRules(t *testing.T) {
	scout := config.Queue{Name: "mac-codex"}
	signal := config.Queue{Name: "mac-claude", Workspace: "Signal"}
	cfg := config.Config{Workspace: "Elk Scout", DefaultRepo: "/dev/elk",
		Repos:  map[string]string{"elk-work/scout": "/dev/scout", "signal": "/dev/signal"},
		Queues: []config.Queue{scout, signal}}
	for _, tc := range []struct {
		q          config.Queue
		text, want string
	}{
		{scout, "repo: elk-work/scout", "/dev/scout"},
		{signal, "repo: signal", "/dev/signal"},
		{scout, "**Direction:** run the Wrangler cycle", "/dev/elk"},
		{signal, "repo: elk-work/signal", "/dev/signal"},
	} {
		got, err := runner.ResolveRepo(cfg, tc.q, tc.text)
		if err != nil || got.Path != tc.want {
			t.Errorf("%s %q = %+v, %v; want %s", cfg.QueueLabel(tc.q), tc.text, got, err, tc.want)
		}
	}
}

func TestQueueRepositoryCapabilities(t *testing.T) {
	path := t.TempDir()
	scout := config.Queue{Name: "mac-claude", Repos: []string{"elk-work/scout"}}
	gallery := config.Queue{Name: "mac-claude", Workspace: "Gallery", Repos: []string{"friend/gallery"}}
	cfg := config.Config{Workspace: "Scout", Repos: map[string]string{"elk-work/scout": path, "friend/gallery": path}, Queues: []config.Queue{scout, gallery}}
	host := runner.NewHostCapabilities("test", "repo:friend/gallery", "git")
	scoped := host.ForQueue(cfg, scout)
	if !scoped.Has("repo:elk-work/scout") || scoped.Has("repo:friend/gallery") || !scoped.Has("git") {
		t.Fatalf("capabilities = %v", scoped.Names())
	}
}

func TestUnknownURLNeverFallsBack(t *testing.T) {
	_, err := runner.ResolveRepo(config.Config{DefaultRepo: "/dev/default"}, config.Queue{Repo: "/dev/queue"}, "https://github.com/other/project/pull/1")
	if err == nil {
		t.Fatal("unknown repository URL used a default")
	}
}
