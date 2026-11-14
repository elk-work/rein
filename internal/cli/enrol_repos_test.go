package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/runner"
)

// checkout makes a directory that passes for a git checkout.
func checkout(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ark:rein#82: an enrolment can say which repositories the queue serves, so a
// fresh worker cuts a worktree for its first run with no hand edit.
func TestEnrolDeclaresRepositories(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	registerAs(t, "claude")
	s := enrolServer(t)
	elk, scout := checkout(t, "elk"), checkout(t, "scout")

	out, errb, code := run(t, "enrol",
		"--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac",
		"--token", enrolToken, "--mcp-url", s.URL,
		"--repo", "elk-work/elk="+elk, "--repo", "elk-work/scout="+scout, "--default-repo", "elk-work/elk")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	q, _ := cfg.Queue("mac-claude")
	if cfg.Repos["elk-work/elk"] != elk || cfg.Repos["elk-work/scout"] != scout {
		t.Errorf("[repos] = %v", cfg.Repos)
	}
	if strings.Join(q.Repos, ",") != "elk-work/elk,elk-work/scout" || q.Repo != "elk-work/elk" {
		t.Errorf("queue repos = %v, repo = %q", q.Repos, q.Repo)
	}
	// What a claimed run that names nothing is cut from.
	if res, err := runner.ResolveRepo(cfg, q, "**Direction:** tidy up"); err != nil || res.Path != elk {
		t.Errorf("default resolution = %+v, %v", res, err)
	}
	if res, err := runner.ResolveRepo(cfg, q, "repo: elk-work/scout"); err != nil || res.Path != scout {
		t.Errorf("named resolution = %+v, %v", res, err)
	}
	if !strings.Contains(out, "repos   elk-work/elk, elk-work/scout") {
		t.Errorf("enrol did not say what it declared:\n%s", out)
	}
	// Pace routes on repo: capabilities, so the enrolment declares them.
	caps, _ := s.CallsTo("connect_executor")[0].Args["declared_capabilities"].([]any)
	var got []string
	for _, c := range caps {
		got = append(got, c.(string))
	}
	if !strings.Contains(strings.Join(got, " "), "repo:elk-work/scout") {
		t.Errorf("declared = %v", got)
	}

	// A re-enrolment keeps them, and --repo only adds.
	extra := checkout(t, "pulse")
	out, errb, code = run(t, "enrol",
		"--workspace", "Elk Scout", "--agent-kind", "claude", "--force",
		"--token", enrolToken, "--mcp-url", s.URL, "--repo", "elk-work/pulse="+extra)
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	cfg, _ = config.Load()
	q, _ = cfg.Queue("mac-claude")
	if len(q.Repos) != 3 || q.Repo != "elk-work/elk" {
		t.Errorf("re-enrol lost repositories: %v, repo = %q", q.Repos, q.Repo)
	}
}

func TestEnrolWithOneRepositoryMakesItTheDefault(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	registerAs(t, "claude")
	s := enrolServer(t)
	dir := checkout(t, "scout")
	out, errb, code := run(t, "enrol", "--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac",
		"--token", enrolToken, "--mcp-url", s.URL, "--repo", "elk-work/scout="+dir)
	if code != 0 {
		t.Fatalf("%s %s", out, errb)
	}
	cfg, _ := config.Load()
	q, _ := cfg.Queue("mac-claude")
	if q.Repo != "elk-work/scout" {
		t.Errorf("repo = %q", q.Repo)
	}
}

func TestEnrolRefusesABadRepositoryBeforeCallingElk(t *testing.T) {
	notGit := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no equals", []string{"--repo", "scout"}, "want name=path"},
		{"missing path", []string{"--repo", "scout=/no/such/dir"}, "not a directory"},
		{"not a checkout", []string{"--repo", "scout=" + notGit}, "not a git checkout"},
		{"unknown default", []string{"--default-repo", "nope"}, "neither a repository name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvHome, t.TempDir())
			registerAs(t, "claude")
			s := enrolServer(t)
			args := append([]string{"enrol", "--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac",
				"--token", enrolToken, "--mcp-url", s.URL}, tc.args...)
			out, errb, code := run(t, args...)
			if code == 0 || !strings.Contains(errb, tc.want) {
				t.Fatalf("exit = %d, want an error containing %q\nstdout: %s\nstderr: %s", code, tc.want, out, errb)
			}
			if n := len(s.CallsTo("connect_executor")); n != 0 {
				t.Errorf("Elk was called %d times for an enrolment that could not be written", n)
			}
		})
	}
}

// The first repos list scopes the whole config. A worker set up by hand with
// no list must keep every repository it served when a second agent joins with
// one — without the person editing config.toml (Casey's Mac, Nov 13: adding
// Gemini beside an unscoped Claude worker was refused).
func TestEnrolKeepsAHandSetUpQueueOnItsRepositories(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	s := enrolServer(t)
	elk, scout, website := checkout(t, "elk"), checkout(t, "scout"), checkout(t, "website")
	path := filepath.Join(home, config.FileName)
	cfg := config.Config{Workspace: "Elk Scout", HostID: "mac",
		Repos:  map[string]string{"elk": elk, "scout": scout, "website": website},
		Queues: []config.Queue{{Name: "worker-5", AgentKind: "claude"}}}
	if err := cfg.SaveFile(path); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(path)

	out, errb, code := run(t, "enrol", "--workspace", "Elk Scout", "--agent-kind", "gemini", "--queue", "mac-claude",
		"--token", enrolToken, "--mcp-url", s.URL, "--repo", "elk-work/pulse="+checkout(t, "pulse"))
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	old, _ := cfg.Queue("worker-5")
	if got := strings.Join(old.Repos, ","); got != "elk,scout,website" {
		t.Errorf("worker-5 repos = %q, want what it served before", got)
	}
	// The test Elk files every enrolment as mac-claude.
	for _, name := range []string{"worker-5", "mac-claude"} {
		q, _ := cfg.Queue(name)
		served := cfg.RepositoriesFor(q)
		for _, want := range []string{"elk", "scout", "website", "elk-work/pulse"} {
			if _, ok := served[want]; !ok {
				t.Errorf("%s does not serve %s: %v", name, want, served)
			}
		}
	}
	if rep := runner.CheckConfig(cfg, ""); !rep.OK {
		t.Errorf("config check after enrol = %+v", rep)
	}
	if !strings.Contains(out, "kept    worker-5") {
		t.Errorf("enrol did not say it kept worker-5:\n%s", out)
	}
	if backup, err := os.ReadFile(path + ".bak"); err != nil || string(backup) != string(original) {
		t.Errorf("no backup of the original config (%v)", err)
	}
}

// A queue that relied on the machine-wide default_repo keeps a default once a
// list scopes the config, because default_repo stops applying.
func TestEnrolKeepsAnotherQueuesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	registerAs(t, "codex")
	s := enrolServer(t)
	old := checkout(t, "old")
	cfg := config.Config{Workspace: "Elk Scout", HostID: "mac", DefaultRepo: old,
		Repos:  map[string]string{"old": old},
		Queues: []config.Queue{{Name: "mac-old", AgentKind: "claude"}}}
	if err := cfg.SaveFile(filepath.Join(home, config.FileName)); err != nil {
		t.Fatal(err)
	}
	out, errb, code := run(t, "enrol", "--workspace", "Elk Scout", "--agent-kind", "codex", "--queue", "mac-claude",
		"--token", enrolToken, "--mcp-url", s.URL, "--repo", "new="+checkout(t, "new"))
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	cfg, _ = config.Load()
	q, _ := cfg.Queue("mac-old")
	if res, err := runner.ResolveRepo(cfg, q, "**Direction:** tidy up"); err != nil || res.Path != old {
		t.Errorf("mac-old default = %+v, %v; want %s", res, err, old)
	}
}

// What pinning cannot save is still refused, before Elk is called: here the
// machine-wide default is a checkout [repos] does not map.
func TestEnrolRefusesToStrandAnotherQueue(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	registerAs(t, "codex")
	s := enrolServer(t)
	cfg := config.Config{Workspace: "Elk Scout", HostID: "mac", DefaultRepo: checkout(t, "unmapped"),
		Queues: []config.Queue{{Name: "mac-claude", AgentKind: "claude"}}}
	if err := cfg.SaveFile(filepath.Join(home, config.FileName)); err != nil {
		t.Fatal(err)
	}
	out, errb, code := run(t, "enrol", "--workspace", "Elk Scout", "--agent-kind", "codex",
		"--token", enrolToken, "--mcp-url", s.URL, "--repo", "new="+checkout(t, "new"))
	if code == 0 || !strings.Contains(errb, "mac-claude") {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if n := len(s.CallsTo("connect_executor")); n != 0 {
		t.Errorf("Elk was called for a refused enrolment")
	}
}

// Removing one queue never shrinks another (Casey's laptop, Nov 13): worker-5
// listed short names and reached the full ones only through worker-11's list,
// so revoking worker-11 left it with the short names and the running service
// refused to reload.
func TestRevokeKeepsTheOtherQueuesRepositories(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	elk, bridge, website := checkout(t, "elk"), checkout(t, "bridge"), checkout(t, "website")
	path := filepath.Join(home, config.FileName)
	cfg := config.Config{Workspace: "Elk Scout", HostID: "mac",
		Repos: map[string]string{"elk": elk, "elk-work/bridge": bridge, "elk-work/website": website},
		Queues: []config.Queue{
			{Name: "worker-5", AgentKind: "claude", Repos: []string{"elk"}, Repo: "elk"},
			{Name: "worker-11", AgentKind: "codex", Repos: []string{"elk-work/bridge", "elk-work/website"}},
		}}
	if err := cfg.SaveFile(path); err != nil {
		t.Fatal(err)
	}
	store := keyring.NewFileStore(filepath.Join(home, keyring.FileName))
	if err := store.Set("Elk Scout", "worker-11", "tok"); err != nil {
		t.Fatal(err)
	}

	out, errb, code := run(t, "enrol", "--revoke", "--queue", "worker-11")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	after, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Queue("worker-11"); ok {
		t.Error("worker-11 is still in the config")
	}
	if lost := runner.StrandedBy(runner.CheckConfig(cfg, ""), runner.CheckConfig(after, "")); len(lost) != 1 || !strings.Contains(lost[0], "worker-11 would not be served") {
		t.Errorf("revoke stranded %v, want only worker-11 gone", lost)
	}
	q, _ := after.Queue("worker-5")
	if got := strings.Join(q.Repos, ","); got != "elk,elk-work/bridge,elk-work/website" || q.Repo != "elk" {
		t.Errorf("worker-5 repos = %q, repo = %q", got, q.Repo)
	}
	if !strings.Contains(out, "kept    worker-5") {
		t.Errorf("revoke did not say it kept worker-5:\n%s", out)
	}
}

func TestEnrolAndStatusWarnWhenAQueueHasNoRepository(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	registerAs(t, "claude")
	s := enrolServer(t)
	out, errb, code := run(t, "enrol", "--workspace", "Elk Scout", "--agent-kind", "claude", "--host-id", "mac",
		"--token", enrolToken, "--mcp-url", s.URL)
	if code != 0 {
		t.Fatalf("%s %s", out, errb)
	}
	if !strings.Contains(out, "repos   NONE") {
		t.Errorf("enrol did not warn:\n%s", out)
	}
	out, _, _ = run(t, "status")
	if !strings.Contains(out, "WARNING: queue mac-claude has no repository") {
		t.Errorf("status did not warn:\n%s", out)
	}
}
