package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	if q, _ := cfg.Queue("mac-codex"); cfg.RepositoriesFor(q)["elk-work/elk"] != elk {
		t.Errorf("mac-codex lost elk-work/elk: %v", cfg.RepositoriesFor(q))
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
	// Again: the checkout is there now, so it is reused, not cloned over, and
	// the queue already has it, so nothing changes.
	out, errb, code = run(t, "repos", "add", "elk-work/pulse", "--clone", "--from", src, "--into", into)
	if code == 0 || !strings.Contains(errb, "already has elk-work/pulse") || strings.Contains(out, "cloned") {
		t.Fatalf("second add: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
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

// Adding a repository the queue already has, or removing one it does not
// have, says so and leaves config.toml byte for byte as it was.
func TestReposRefusalsLeaveTheFileAsItWas(t *testing.T) {
	elk, pulse := checkout(t, "elk"), checkout(t, "pulse")
	path := saveConfig(t, config.Config{Workspace: "Elk Scout", HostID: "mac",
		Repos:  map[string]string{"elk-work/elk": elk, "elk-work/pulse": pulse},
		Queues: []config.Queue{{Name: "worker-4", AgentKind: "claude", Parallel: 2, Repos: []string{"elk-work/elk", "elk-work/pulse"}, Repo: "elk-work/elk"}}})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"repos", "add", "elk-work/pulse", "--path", pulse}, "already has elk-work/pulse"},
		{[]string{"repos", "add", "elk-work/pulse", "--path", checkout(t, "other")}, "already has elk-work/pulse"},
		{[]string{"repos", "remove", "elk-work/scout"}, "does not have elk-work/scout"},
	} {
		out, errb, code := run(t, tc.args...)
		if code == 0 || !strings.Contains(errb, tc.want) || !strings.Contains(errb, "nothing") {
			t.Errorf("%v: exit = %d\nstdout: %s\nstderr: %s", tc.args, code, out, errb)
		}
		if after, _ := os.ReadFile(path); string(after) != string(before) {
			t.Errorf("%v changed config.toml:\n%s", tc.args, after)
		}
	}
}

// Removing one repository changes that queue's list and nothing else: the
// other repositories, the [repos] table and every other setting stay.
func TestReposRemoveLeavesEverythingElseAsItWas(t *testing.T) {
	a, b, c := checkout(t, "a"), checkout(t, "b"), checkout(t, "c")
	before := config.Config{Workspace: "Elk Scout", HostID: "mac", MachineID: "m-1",
		Repos: map[string]string{"o/a": a, "o/b": b, "o/c": c},
		Queues: []config.Queue{
			{Name: "worker-4", AgentKind: "claude", Parallel: 3, Repos: []string{"o/a", "o/b"}, Repo: "o/a"},
			{Name: "mac-codex", AgentKind: "codex", Workspace: "Other", Repos: []string{"o/c"}, Repo: "o/c"}}}
	saveConfig(t, before)
	want := loadConfig(t)
	if _, errb, code := run(t, "repos", "remove", "o/b", "--queue", "worker-4"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errb)
	}
	got := loadConfig(t)
	q, _ := got.Queue("worker-4")
	if strings.Join(q.Repos, ",") != "o/a" || q.Repo != "o/a" {
		t.Errorf("worker-4 repos = %v, repo = %q", q.Repos, q.Repo)
	}
	// Put back what remove was asked to change; the rest must be identical.
	w, _ := want.Queue("worker-4")
	w.Repos = []string{"o/a"}
	want.SetQueue(w)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("remove changed more than the one repository:\n got  %+v\n want %+v", got, want)
	}
}

// A worker set up by hand with no repos list reaches all of [repos]. Adding
// one must keep every repository it already had, not narrow it to the new one.
func TestReposAddKeepsWhatAnUnlistedQueueAlreadyHad(t *testing.T) {
	elk, pulse := checkout(t, "elk"), checkout(t, "pulse")
	saveConfig(t, config.Config{Workspace: "Elk Scout",
		Repos:  map[string]string{"elk-work/elk": elk},
		Queues: []config.Queue{{Name: "worker-4", AgentKind: "claude"}}})
	if out, errb, code := run(t, "repos", "add", "elk-work/pulse", "--path", pulse); code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	cfg := loadConfig(t)
	q, _ := cfg.Queue("worker-4")
	served := cfg.RepositoriesFor(q)
	if served["elk-work/elk"] != elk || served["elk-work/pulse"] != pulse {
		t.Errorf("worker-4 serves %v, want elk and pulse", served)
	}
}

// Lists in one workspace combine, so taking a repository off one queue while
// a peer still lists it would leave it reachable. Remove says so instead.
func TestReposRemoveRefusesARepositoryAPeerStillLists(t *testing.T) {
	a, b := checkout(t, "a"), checkout(t, "b")
	saveConfig(t, config.Config{Workspace: "Elk Scout",
		Repos: map[string]string{"o/a": a, "o/b": b},
		Queues: []config.Queue{
			{Name: "worker-4", AgentKind: "claude", Repos: []string{"o/a", "o/b"}, Repo: "o/a"},
			{Name: "mac-codex", AgentKind: "codex", Repos: []string{"o/b"}, Repo: "o/b"}}})
	_, errb, code := run(t, "repos", "remove", "o/b", "--queue", "worker-4")
	if code == 0 || !strings.Contains(errb, "mac-codex") {
		t.Fatalf("exit = %d, stderr: %s", code, errb)
	}
	if q, _ := loadConfig(t).Queue("worker-4"); strings.Join(q.Repos, ",") != "o/a,o/b" {
		t.Errorf("worker-4 repos = %v", q.Repos)
	}
}
