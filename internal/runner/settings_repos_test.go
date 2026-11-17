package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
)

// gitCheckout makes a git checkout at dir, with origin as its origin remote
// when given, and returns dir.
func gitCheckout(t *testing.T, dir, origin string) string {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	if origin != "" {
		run("remote", "add", "origin", origin)
	}
	return dir
}

func noClone(t *testing.T) func(string) (string, error) {
	return func(name string) (string, error) {
		t.Errorf("cloned %s, which this computer already has", name)
		return "", errors.New("unexpected clone")
	}
}

func reposOf(t *testing.T, cfg config.Config, queue string) []string {
	t.Helper()
	q, ok := cfg.QueueIn("Elk Scout", queue)
	if !ok {
		t.Fatalf("no queue %s", queue)
	}
	return q.Repos
}

func TestAddElkReposUsesACheckoutAlreadyInRepos(t *testing.T) {
	tmp := t.TempDir()
	app := gitCheckout(t, filepath.Join(tmp, "app"), "")
	other := gitCheckout(t, filepath.Join(tmp, "other"), "")
	cfg := settingsCfg(config.Queue{Repos: []string{"acme/other"}, Repo: "acme/other"})
	cfg.Repos = map[string]string{"acme/app": app, "acme/other": other}

	next, changed, err := AddElkRepos(cfg, "Elk Scout", "mac-claude", []string{"ACME/App"}, noClone(t))
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got := reposOf(t, next, "mac-claude"); !slices.Equal(got, []string{"acme/other", "acme/app"}) {
		t.Errorf("repos = %v", got)
	}
	if len(next.Repos) != 2 || next.Repos["acme/app"] != app {
		t.Errorf("[repos] = %v", next.Repos)
	}
	if got := reposOf(t, cfg, "mac-claude"); len(got) != 1 {
		t.Errorf("AddElkRepos changed its input: %v", got)
	}
}

func TestAddElkReposUsesACheckoutWhoseOriginIsTheRepository(t *testing.T) {
	tmp := t.TempDir()
	app := gitCheckout(t, filepath.Join(tmp, "app"), "https://github.com/acme/app.git")
	other := gitCheckout(t, filepath.Join(tmp, "other"), "")
	cfg := settingsCfg(config.Queue{Repos: []string{"acme/other"}, Repo: "acme/other"})
	cfg.Repos = map[string]string{"app": app, "acme/other": other}

	next, changed, err := AddElkRepos(cfg, "Elk Scout", "mac-claude", []string{"acme/app"}, noClone(t))
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got := reposOf(t, next, "mac-claude"); !slices.Equal(got, []string{"acme/other", "app"}) {
		t.Errorf("repos = %v", got)
	}
}

func TestAddElkReposClonesAMissingRepository(t *testing.T) {
	tmp := t.TempDir()
	src := gitCheckout(t, filepath.Join(tmp, "src"), "")
	base := filepath.Join(tmp, "Developer")
	other := gitCheckout(t, filepath.Join(tmp, "other"), "")
	cfg := settingsCfg(config.Queue{Repos: []string{"acme/other"}, Repo: "acme/other"})
	cfg.Repos = map[string]string{"acme/other": other}
	clone := func(name string) (string, error) {
		dir, _, err := CloneRepo(context.Background(), name, src, base, false)
		return dir, err
	}

	next, changed, err := AddElkRepos(cfg, "Elk Scout", "mac-claude", []string{"acme/new"}, clone)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	want := filepath.Join(base, "new")
	if _, err := os.Stat(filepath.Join(want, ".git")); err != nil {
		t.Fatalf("no checkout at %s: %v", want, err)
	}
	if next.Repos["acme/new"] != want {
		t.Errorf("[repos] = %v", next.Repos)
	}
	if got := reposOf(t, next, "mac-claude"); !slices.Equal(got, []string{"acme/other", "acme/new"}) {
		t.Errorf("repos = %v", got)
	}

	// A checkout already where the clone would go is reused.
	dir, cloned, err := CloneRepo(context.Background(), "acme/new", src, base, false)
	if err != nil || cloned || dir != want {
		t.Errorf("second clone: dir=%s cloned=%v err=%v", dir, cloned, err)
	}

	// A clone that fails leaves nothing a later one would take for a checkout.
	if _, _, err := CloneRepo(context.Background(), "acme/gone", filepath.Join(tmp, "nowhere"), base, false); err == nil {
		t.Fatal("cloning from nowhere succeeded")
	}
	if _, err := os.Stat(filepath.Join(base, "gone")); !os.IsNotExist(err) {
		t.Errorf("a failed clone left %s behind: %v", filepath.Join(base, "gone"), err)
	}
}

func TestAddElkReposLeavesARepositoryAlreadyServed(t *testing.T) {
	tmp := t.TempDir()
	app := gitCheckout(t, filepath.Join(tmp, "app"), "")
	scoped := settingsCfg(config.Queue{Repos: []string{"acme/app"}})
	scoped.Repos = map[string]string{"acme/app": app}
	unscoped := settingsCfg(config.Queue{})
	unscoped.Repos = map[string]string{"acme/app": app}

	for name, cfg := range map[string]config.Config{"scoped": scoped, "unscoped": unscoped} {
		next, changed, err := AddElkRepos(cfg, "Elk Scout", "mac-claude", []string{"acme/app"}, noClone(t))
		if err != nil || changed {
			t.Errorf("%s: changed=%v err=%v", name, changed, err)
		}
		if !slices.Equal(reposOf(t, next, "mac-claude"), reposOf(t, cfg, "mac-claude")) {
			t.Errorf("%s: repos changed to %v", name, reposOf(t, next, "mac-claude"))
		}
	}
	// Nothing wanted is nothing to add.
	if _, changed, err := AddElkRepos(scoped, "Elk Scout", "mac-claude", nil, noClone(t)); err != nil || changed {
		t.Errorf("empty list: changed=%v err=%v", changed, err)
	}
}

func TestAddElkReposSkipsOnlyAFailedClone(t *testing.T) {
	tmp := t.TempDir()
	app := gitCheckout(t, filepath.Join(tmp, "app"), "")
	other := gitCheckout(t, filepath.Join(tmp, "other"), "")
	cfg := settingsCfg(config.Queue{Repos: []string{"acme/other"}, Repo: "acme/other"})
	cfg.Repos = map[string]string{"acme/app": app, "acme/other": other}
	var tried []string
	clone := func(name string) (string, error) {
		tried = append(tried, name)
		return "", errors.New("network down")
	}

	next, changed, err := AddElkRepos(cfg, "Elk Scout", "mac-claude", []string{"acme/broken", "acme/app"}, clone)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !slices.Equal(tried, []string{"acme/broken"}) {
		t.Errorf("tried to clone %v", tried)
	}
	if got := reposOf(t, next, "mac-claude"); !slices.Equal(got, []string{"acme/other", "acme/app"}) {
		t.Errorf("repos = %v", got)
	}
	if _, ok := next.Repos["acme/broken"]; ok {
		t.Errorf("a failed clone was declared: %v", next.Repos)
	}
}

func TestAddElkReposKeepsEveryQueueOnWhatItServes(t *testing.T) {
	tmp := t.TempDir()
	app := gitCheckout(t, filepath.Join(tmp, "app"), "")
	lib := gitCheckout(t, filepath.Join(tmp, "lib"), "")
	fresh := gitCheckout(t, filepath.Join(tmp, "Developer", "new"), "")
	// No queue declares a list, so each serves all of [repos]. The first list
	// would scope the whole config.
	cfg := settingsCfg(config.Queue{})
	cfg.Queues = append(cfg.Queues, config.Queue{Name: "mac-codex", AgentKind: "codex", Workspace: "Elk Scout"})
	cfg.Repos = map[string]string{"acme/app": app, "acme/lib": lib}
	clone := func(string) (string, error) { return fresh, nil }

	next, changed, err := AddElkRepos(cfg, "Elk Scout", "mac-claude", []string{"acme/new"}, clone)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	for _, queue := range []string{"mac-claude", "mac-codex"} {
		q, _ := next.QueueIn("Elk Scout", queue)
		served := next.RepositoriesFor(q)
		for _, name := range []string{"acme/app", "acme/lib"} {
			if _, ok := served[name]; !ok {
				t.Errorf("%s lost %s: serves %v", queue, name, served)
			}
		}
	}
	q, _ := next.QueueIn("Elk Scout", "mac-claude")
	if _, ok := next.RepositoriesFor(q)["acme/new"]; !ok {
		t.Errorf("mac-claude does not serve acme/new: %v", q.Repos)
	}
	if lost := StrandedBy(CheckConfig(cfg, ""), CheckConfig(next, "")); len(lost) > 0 {
		t.Errorf("a running service would refuse this config: %v", lost)
	}
}

func TestAddElkReposTwiceIsANoOp(t *testing.T) {
	tmp := t.TempDir()
	other := gitCheckout(t, filepath.Join(tmp, "other"), "")
	fresh := gitCheckout(t, filepath.Join(tmp, "Developer", "new"), "")
	cfg := settingsCfg(config.Queue{Repos: []string{"acme/other"}, Repo: "acme/other"})
	cfg.Repos = map[string]string{"acme/other": other}
	clones := 0
	clone := func(string) (string, error) { clones++; return fresh, nil }

	next, changed, err := AddElkRepos(cfg, "Elk Scout", "mac-claude", []string{"acme/new"}, clone)
	if err != nil || !changed {
		t.Fatalf("first: changed=%v err=%v", changed, err)
	}
	again, changed, err := AddElkRepos(next, "Elk Scout", "mac-claude", []string{"acme/new"}, clone)
	if err != nil || changed || clones != 1 {
		t.Fatalf("second: changed=%v err=%v clones=%d", changed, err, clones)
	}
	if !slices.Equal(reposOf(t, again, "mac-claude"), reposOf(t, next, "mac-claude")) {
		t.Errorf("repos moved: %v", reposOf(t, again, "mac-claude"))
	}
}

func TestSettingsSignatureCoversRepos(t *testing.T) {
	a := settingsSignature(elk.WorkerSettings{Repos: []string{"acme/app"}})
	if a == settingsSignature(elk.WorkerSettings{}) || a != settingsSignature(elk.WorkerSettings{Repos: []string{"acme/app"}}) {
		t.Errorf("a new repository must re-apply, and the same one must not")
	}
}
