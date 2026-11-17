package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runner"
)

// ark:rein#160: a worker's repositories change after enrol without enrolling
// again. These write config.toml only; the running service restarts on the
// file change and its next heartbeat declares the new list.

func saveConfig(t *testing.T, cfg config.Config) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	path := filepath.Join(home, config.FileName)
	if err := cfg.SaveFile(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestReposAddDeclaresACheckoutOnTheOnlyQueue(t *testing.T) {
	elk, pulse := checkout(t, "elk"), checkout(t, "pulse")
	saveConfig(t, config.Config{Workspace: "Elk Scout", HostID: "mac",
		Repos:  map[string]string{"elk-work/elk": elk},
		Queues: []config.Queue{{Name: "worker-4", AgentKind: "claude", Repos: []string{"elk-work/elk"}, Repo: "elk-work/elk"}}})

	out, errb, code := run(t, "repos", "add", "elk-work/pulse", "--path", pulse)
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	cfg := loadConfig(t)
	q, _ := cfg.Queue("worker-4")
	if cfg.Repos["elk-work/pulse"] != pulse || strings.Join(q.Repos, ",") != "elk-work/elk,elk-work/pulse" || q.Repo != "elk-work/elk" {
		t.Errorf("[repos] = %v, queue repos = %v, repo = %q", cfg.Repos, q.Repos, q.Repo)
	}
	if res, err := runner.ResolveRepo(cfg, q, "repo: elk-work/pulse"); err != nil || res.Path != pulse {
		t.Errorf("resolution = %+v, %v", res, err)
	}
	if !strings.Contains(out, "elk-work/pulse") {
		t.Errorf("add did not say what the queue serves now:\n%s", out)
	}
}

func TestReposAddRefusesAFolderThatIsNotACheckout(t *testing.T) {
	saveConfig(t, config.Config{Workspace: "Elk Scout", Queues: []config.Queue{{Name: "worker-4", AgentKind: "claude"}}})
	_, errb, code := run(t, "repos", "add", "elk-work/pulse", "--path", t.TempDir())
	if code == 0 || !strings.Contains(errb, "not a git checkout") {
		t.Fatalf("exit = %d, stderr: %s", code, errb)
	}
	if _, ok := loadConfig(t).Repos["elk-work/pulse"]; ok {
		t.Error("a refused add wrote the config")
	}
}

func TestReposAddAsksWhichQueueWhenThereAreSeveral(t *testing.T) {
	pulse := checkout(t, "pulse")
	saveConfig(t, config.Config{Workspace: "Elk Scout", Queues: []config.Queue{
		{Name: "worker-4", AgentKind: "claude"}, {Name: "mac-codex", AgentKind: "codex"}}})
	_, errb, code := run(t, "repos", "add", "elk-work/pulse", "--path", pulse)
	if code == 0 || !strings.Contains(errb, "--queue") || !strings.Contains(errb, "worker-4") || !strings.Contains(errb, "mac-codex") {
		t.Fatalf("exit = %d, stderr: %s", code, errb)
	}
	out, errb, code := run(t, "repos", "add", "elk-work/pulse", "--path", pulse, "--queue", "mac-codex")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	cfg := loadConfig(t)
	if q, _ := cfg.Queue("mac-codex"); strings.Join(q.Repos, ",") != "elk-work/pulse" {
		t.Errorf("mac-codex repos = %v", q.Repos)
	}
	if q, _ := cfg.Queue("worker-4"); len(q.Repos) != 0 {
		t.Errorf("worker-4 changed: %v", q.Repos)
	}
}

// The first list scopes the whole config, so a queue that served [repos]
// unscoped keeps exactly what it served, as an enrolment keeps it.
func TestReposAddKeepsAnotherQueueOnWhatItServes(t *testing.T) {
	elk, pulse := checkout(t, "elk"), checkout(t, "pulse")
	saveConfig(t, config.Config{Workspace: "Elk Scout", HostID: "mac",
		Repos:  map[string]string{"elk-work/elk": elk},
		Queues: []config.Queue{{Name: "worker-4", AgentKind: "claude"}, {Name: "mac-codex", AgentKind: "codex"}}})
	out, errb, code := run(t, "repos", "add", "elk-work/pulse", "--path", pulse, "--queue", "worker-4")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	cfg := loadConfig(t)
	if q, _ := cfg.Queue("mac-codex"); !strings.Contains(strings.Join(q.Repos, ","), "elk-work/elk") {
		t.Errorf("mac-codex lost elk-work/elk: %v", q.Repos)
	}
	if !strings.Contains(out, "kept    mac-codex") {
		t.Errorf("add did not say it kept mac-codex:\n%s", out)
	}
}

// Removing the default leaves another default, never none.
func TestReposRemoveOfTheDefaultKeepsADefault(t *testing.T) {
	a, b, c := checkout(t, "a"), checkout(t, "b"), checkout(t, "c")
	saveConfig(t, config.Config{Workspace: "Elk Scout",
		Repos:  map[string]string{"o/a": a, "o/b": b, "o/c": c},
		Queues: []config.Queue{{Name: "worker-4", AgentKind: "claude", Repos: []string{"o/a", "o/b", "o/c"}, Repo: "o/a"}}})
	if _, errb, code := run(t, "repos", "remove", "o/a"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errb)
	}
	if q, _ := loadConfig(t).Queue("worker-4"); q.Repo != "o/b" {
		t.Errorf("default = %q, want o/b", q.Repo)
	}
}

// --clone downloads a repository nothing on the machine has yet.
func TestReposAddClonesIntoTheFolderGiven(t *testing.T) {
	src := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "first"}} {
		c := exec.Command("git", append([]string{"-C", src}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v %s", err, out)
		}
	}
	into := t.TempDir()
	saveConfig(t, config.Config{Workspace: "Elk Scout", Queues: []config.Queue{{Name: "worker-4", AgentKind: "claude"}}})
	out, errb, code := run(t, "repos", "add", "elk-work/pulse", "--clone", "--from", src, "--into", into)
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	want := filepath.Join(into, "pulse")
	if _, err := os.Stat(filepath.Join(want, ".git")); err != nil {
		t.Fatalf("no checkout at %s: %v", want, err)
	}
	if got := loadConfig(t).Repos["elk-work/pulse"]; got != want {
		t.Errorf("[repos] elk-work/pulse = %q, want %q", got, want)
	}
	// Again: the checkout is there now, so it is reused, not cloned over.
	if _, errb, code := run(t, "repos", "add", "elk-work/pulse", "--clone", "--from", src, "--into", into); code != 0 {
		t.Fatalf("second add: exit = %d, stderr: %s", code, errb)
	}
}

func TestReposAddNeedsAPathOrClone(t *testing.T) {
	saveConfig(t, config.Config{Workspace: "Elk Scout", Queues: []config.Queue{{Name: "worker-4", AgentKind: "claude"}}})
	if _, errb, code := run(t, "repos", "add", "elk-work/pulse"); code == 0 || !strings.Contains(errb, "--path") {
		t.Fatalf("exit = %d, stderr: %s", code, errb)
	}
}

func TestReposRemoveTakesARepositoryOffTheQueue(t *testing.T) {
	elk, pulse := checkout(t, "elk"), checkout(t, "pulse")
	saveConfig(t, config.Config{Workspace: "Elk Scout",
		Repos:  map[string]string{"elk-work/elk": elk, "elk-work/pulse": pulse},
		Queues: []config.Queue{{Name: "worker-4", AgentKind: "claude", Repos: []string{"elk-work/elk", "elk-work/pulse"}, Repo: "elk-work/pulse"}}})
	out, errb, code := run(t, "repos", "remove", "elk-work/pulse")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	q, _ := loadConfig(t).Queue("worker-4")
	if strings.Join(q.Repos, ",") != "elk-work/elk" || q.Repo != "elk-work/elk" {
		t.Errorf("queue repos = %v, repo = %q", q.Repos, q.Repo)
	}
	// A running service will not restart into a config that takes a
	// repository away, so the person is told to restart it.
	if !strings.Contains(out, "rein service stop") {
		t.Errorf("remove did not say the service needs a restart:\n%s", out)
	}
	// Its last repository stays: a queue with none strands every run.
	if _, errb, code := run(t, "repos", "remove", "elk-work/elk"); code == 0 || !strings.Contains(errb, "last repository") {
		t.Fatalf("exit = %d, stderr: %s", code, errb)
	}
}
