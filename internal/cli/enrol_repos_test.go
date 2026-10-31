package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
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

// The first repos list scopes the whole config, so declaring one must not
// silently take the repositories away from a queue that already had them.
func TestEnrolRefusesToStrandAnotherQueue(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	registerAs(t, "codex")
	s := enrolServer(t)
	old := checkout(t, "old")
	cfg := config.Config{Workspace: "Elk Scout", HostID: "mac",
		Repos:  map[string]string{"old": old},
		Queues: []config.Queue{{Name: "mac-claude", AgentKind: "claude", Repo: "old"}}}
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
