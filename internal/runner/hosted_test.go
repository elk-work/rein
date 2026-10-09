package runner_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
	"github.com/elk-work/rein/internal/worktree"
)

func hostedHarness(t *testing.T) (*harness, runner.Options) {
	h := newHarness(t)
	t.Setenv("REIN_ELK_TOKEN", token)
	t.Setenv("REIN_GITHUB_TOKEN", "hosted-github-test-token")
	t.Setenv("ANTHROPIC_API_KEY", "hosted-model-test-key")
	h.cfg.Hosted = config.Hosted{Enabled: true, AllowRepos: []string{"acme/scout"}}
	h.cfg.Repos = nil
	h.cfg.Queues[0].Repo = "acme/scout"
	opts := runner.Options{Hosted: true, Landing: &fakeLanding{}, HostedClone: func(_ context.Context, repo, id string, env map[string]string) (*worktree.Worktree, error) {
		if repo != "acme/scout" {
			t.Fatalf("clone repository: %s", repo)
		}
		if env["REIN_GITHUB_TOKEN"] != "hosted-github-test-token" || env["GIT_CONFIG_VALUE_1"] == "" {
			t.Fatal("clone did not get environment helper")
		}
		dir := filepath.Join(h.cfg.WorkDir, id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		return &worktree.Worktree{Dir: dir, Repo: dir, RunID: id, Branch: worktree.BranchPrefix + id, BaseRef: "origin/main", Hosted: true}, nil
	}}
	return h, opts
}
func TestHostedOneRunClaimsDrivesSubmitsAndExits(t *testing.T) {
	h, opts := hostedHarness(t)
	h.elk.Text("claim_run", strings.ReplaceAll(order("run-1"), "repo: scout", "repo: acme/scout"))
	r, err := runner.New(h.prepare(opts)) // Once deliberately false: hosted implies it.
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.elk.CallsTo("claim_run")) != 1 {
		t.Fatal("claimed more than one run")
	}
	if h.submitted().Arg("status") != "ready" {
		t.Fatal("not submitted")
	}
	spec := h.agent.Specs()[0]
	if !spec.Hosted || spec.Timeouts.Total <= 0 || spec.Env["GH_TOKEN"] != "hosted-github-test-token" {
		t.Fatal("hosted spec incomplete")
	}
	if !strings.Contains(spec.SystemPrompt, "Landing policy: pr") && !strings.Contains(spec.SystemPrompt, "pull request") {
		t.Fatal("PR default missing")
	}
	if _, err := os.Stat(spec.WorktreeDir); !os.IsNotExist(err) {
		t.Fatal("clone not reaped")
	}
}
func TestHostedRunCapStopsNeverEndingSession(t *testing.T) {
	h, opts := hostedHarness(t)
	h.cfg.Hosted.MaxRun = config.Duration(50 * time.Millisecond)
	h.agent.Delay = time.Hour
	h.elk.Text("claim_run", strings.ReplaceAll(order("run-1"), "repo: scout", "repo: acme/scout"))
	if err := h.run(opts); err != nil {
		t.Fatal(err)
	}
	sub := h.submitted()
	if sub.Arg("status") != "stuck" || !strings.Contains(sub.Arg("deliverable"), "hit the hosted run cap of 50ms") {
		t.Fatal("cap did not submit stuck")
	}
}
func TestHostedAllowlistRefusesBeforeClone(t *testing.T) {
	h, opts := hostedHarness(t)
	opts.HostedClone = func(context.Context, string, string, map[string]string) (*worktree.Worktree, error) {
		t.Fatal("clone called")
		return nil, nil
	}
	h.elk.Text("claim_run", strings.ReplaceAll(order("run-1"), "repo: scout", "repo: other/scout"))
	if err := h.run(opts); err == nil {
		t.Fatal("refusal exited successfully")
	}
	if h.submitted().Arg("status") != "stuck" {
		t.Fatal("refusal not submitted")
	}
}
func TestHostedSecretsNeverLoggedReportedOrDelivered(t *testing.T) {
	h, opts := hostedHarness(t)
	t.Setenv("OPENAI_API_KEY", "hosted-openai-test-key")
	t.Setenv("CODEX_API_KEY", "hosted-codex-test-key")
	values := []string{"hosted-model-test-key", token, "hosted-github-test-token", "hosted-openai-test-key", "hosted-codex-test-key"}
	h.agent.Script, h.agent.Result = leakingScript(values...)
	logDir := t.TempDir()
	logs, err := runlog.Open(logDir)
	if err != nil {
		t.Fatal(err)
	}
	opts.Logs = logs
	h.elk.Text("claim_run", strings.ReplaceAll(order("run-1"), "repo: scout", "repo: acme/scout"))
	if err := h.run(opts); err != nil {
		t.Fatal(err)
	}
	all := everythingWritten(t, h, logDir)
	for _, v := range values {
		if strings.Contains(all, v) {
			t.Fatal("a hosted secret was written")
		}
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "REIN_ELK_TOKEN", "REIN_GITHUB_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY"} {
		if !strings.Contains(h.submitted().Arg("deliverable"), "[redacted:"+name+"]") {
			t.Fatalf("missing redaction for %s", name)
		}
	}
}
func TestHostedAuthenticationRefusalExitsNonzero(t *testing.T) {
	h, opts := hostedHarness(t)
	h.agent.StartErr = &adapter.APIAuthError{Because: "test rejected source"}
	h.elk.Text("claim_run", strings.ReplaceAll(order("run-1"), "repo: scout", "repo: acme/scout"))
	if err := h.run(opts); !errors.Is(err, adapter.ErrNotAPIAuth) {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(h.submitted().Arg("title"), "not on an API key") {
		t.Fatal("wrong refusal title")
	}
}

func TestHostedCrashSubmitsButExitsNonzero(t *testing.T) {
	h, opts := hostedHarness(t)
	h.agent.WaitErr = errors.New("synthetic crash")
	h.elk.Text("claim_run", strings.ReplaceAll(order("run-1"), "repo: scout", "repo: acme/scout"))
	if err := h.run(opts); err == nil {
		t.Fatal("crash exited zero")
	}
	if h.submitted().Arg("status") != "stuck" {
		t.Fatal("crash not reported")
	}
}
func TestHostedRefusesMultipleSelectedQueues(t *testing.T) {
	h, opts := hostedHarness(t)
	q := h.cfg.Queues[0]
	q.Name = "other-queue"
	h.cfg.Queues = append(h.cfg.Queues, q)
	if _, err := runner.New(h.prepare(opts)); err == nil {
		t.Fatal("multiple hosted queues accepted")
	}
}

func TestHostedAPIKeyMapNeverReadsKeychain(t *testing.T) {
	h, opts := hostedHarness(t)
	h.cfg.Queues[0].Secrets = map[string]string{"ANTHROPIC_API_KEY": "model-key"}
	// No ItemReader is supplied. Hosted billing must use the environment.
	h.elk.Text("claim_run", strings.ReplaceAll(order("run-1"), "repo: scout", "repo: acme/scout"))
	if err := h.run(opts); err != nil {
		t.Fatal(err)
	}
	if h.agent.Specs()[0].Env["ANTHROPIC_API_KEY"] != "hosted-model-test-key" {
		t.Fatal("API key not passed from environment")
	}
}
func TestHostedEmptyQueueDoesNotPretendToSubmit(t *testing.T) {
	h, opts := hostedHarness(t)
	if err := h.run(opts); err == nil {
		t.Fatal("empty hosted claim exited zero")
	}
	if len(h.elk.CallsTo("submit_deliverable")) != 0 {
		t.Fatal("submitted nonexistent run")
	}
}

func TestHostedRepositorySelectionIgnoresURLsAndLocalMappings(t *testing.T) {
	cfg := config.Config{Hosted: config.Hosted{Enabled: true, AllowRepos: []string{"acme/repo", "acme/.github"}}, DefaultRepo: "acme/repo", Repos: map[string]string{"acme/repo": "/some/local/path"}}
	for _, tc := range []struct {
		text, queue, want string
		ok                bool
	}{
		{"repo: acme/repo", "other/repo", "acme/repo", true},
		{"https://github.com/other/repo/pull/1", "acme/repo", "acme/repo", true},
		{"repo: acme/.github", "", "acme/.github", true},
		{"repo: /some/local/path", "acme/repo", "", false},
		{"repo: acme/..", "acme/repo", "", false},
		{"repo: other/repo", "acme/repo", "", false},
		{"https://github.com/acme/repo", "", "", false},
		{"no repository hint", "", "", false},
	} {
		got, err := runner.ResolveHostedRepo(cfg, config.Queue{Repo: tc.queue}, tc.text)
		if tc.ok && (err != nil || got.Name != tc.want) || !tc.ok && err == nil {
			t.Fatalf("selection %q: got %q err=%v", tc.text, got.Name, err)
		}
	}
}
