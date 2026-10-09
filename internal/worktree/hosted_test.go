package worktree_test

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/hosted"
	"github.com/elk-work/rein/internal/worktree"
)

func TestHostedCloneUsesEnvironmentHelperAndLeavesCleanRemote(t *testing.T) {
	origin := newRepo(t)
	for i := 0; i < 55; i++ {
		git(t, origin, "commit", "--allow-empty", "-qm", "history")
	}
	bare := filepath.Join(t.TempDir(), "origin.git")
	git(t, origin, "clone", "--bare", origin, bare)
	executable := filepath.Join(t.TempDir(), "helper with spaces", "rein")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	if err := os.MkdirAll(filepath.Dir(executable), 0700); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", executable, "../../cmd/rein")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, out)
	}
	env := hosted.GitEnv(executable)
	const token = "synthetic-hosted-github-token"
	env["REIN_GITHUB_TOKEN"] = token
	// Only tests rewrite the HTTPS origin to a local bare repository. The
	// stored remote remains credential-free HTTPS, with a depth-50 file clone.
	localPath := filepath.ToSlash(bare)
	if runtime.GOOS == "windows" {
		localPath = "/" + localPath
	}
	localURL := (&url.URL{Scheme: "file", Path: localPath}).String()
	env["GIT_CONFIG_COUNT"] = "4"
	env["GIT_CONFIG_KEY_3"] = "url." + localURL + ".insteadOf"
	env["GIT_CONFIG_VALUE_3"] = "https://github.com/acme/repo.git"
	m := &worktree.Manager{Root: t.TempDir()}
	wt, err := m.Clone(context.Background(), "acme/repo", "hosted-run", env)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Reap(context.Background(), wt)
	if got := git(t, wt.Dir, "config", "--get", "remote.origin.url"); got != "https://github.com/acme/repo.git" {
		t.Fatalf("remote URL=%s", got)
	}
	if got := git(t, wt.Dir, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatal("clone is not shallow")
	}
	if wt.Branch != worktree.BranchPrefix+"hosted-run" || wt.BaseSHA == "" {
		t.Fatal("run branch missing")
	}
	blob, err := os.ReadFile(filepath.Join(wt.Dir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), token) || strings.Contains(string(blob), "credential.helper") {
		t.Fatal("credential persisted in git config")
	}
	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir = wt.Dir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=acme/repo.git\n\n")
	for _, arg := range cmd.Args {
		if strings.Contains(arg, token) {
			t.Fatal("token in argv")
		}
	}
	for _, v := range hosted.GitEnv(executable) {
		if strings.Contains(v, token) {
			t.Fatal("token in helper command")
		}
	}
	output, err := cmd.Output()
	if err != nil {
		t.Fatal("git credential helper failed", err)
	}
	if !strings.Contains(string(output), "password="+token) {
		t.Fatal("token did not arrive through the helper")
	}
	if _, err := m.Clone(context.Background(), "acme/repo", "hosted-run", env); err == nil {
		t.Fatal("existing clone overwritten")
	}
}
