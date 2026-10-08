package worktree_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/worktree"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo makes a one-commit git repository to cut worktrees from.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "rein@example.test")
	git(t, dir, "config", "user.name", "Rein Test")
	write(t, filepath.Join(dir, ".gitignore"), ".ark/\n")
	write(t, filepath.Join(dir, "README.md"), "# test\n")
	commit(t, dir, "initial")
	return dir
}

// newRepoWithOrigin makes a repository whose origin is another local
// repository, so the remote-default resolution has something real to find.
func newRepoWithOrigin(t *testing.T) (clone, origin string) {
	t.Helper()
	origin = newRepo(t)
	// A bare-ish origin: clone it, which also sets refs/remotes/origin/HEAD.
	clone = filepath.Join(t.TempDir(), "clone")
	out, err := exec.Command("git", "clone", "-q", origin, clone).CombinedOutput()
	if err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	git(t, clone, "config", "user.email", "rein@example.test")
	git(t, clone, "config", "user.name", "Rein Test")
	return clone, origin
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, repo, message string) {
	t.Helper()
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", message)
}

func TestCreateAndReap(t *testing.T) {
	repo := newRepo(t)
	root := t.TempDir()
	m := &worktree.Manager{Root: root}
	ctx := context.Background()

	wt, err := m.Create(ctx, worktree.Request{Repo: repo, RunID: "run-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if wt.Branch != worktree.BranchPrefix+"run-abc" {
		t.Errorf("branch = %q", wt.Branch)
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, "README.md")); err != nil {
		t.Fatalf("the worktree has no checkout: %v", err)
	}
	if wt.BaseSHA == "" {
		t.Error("no base commit recorded; it is the first thing a reviewer asks about")
	}
	if wt.Ark.Wanted {
		t.Error("Ark.Wanted is true for a repository with no .ark")
	}

	if err := m.Reap(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Errorf("the worktree directory survived the reap: %v", err)
	}
	// The branch outlives the worktree: anything the agent committed has to
	// stay reachable, and the deliverable names the branch precisely because
	// the directory is gone by the time a person reads it.
	if !strings.Contains(git(t, repo, "branch", "--list", wt.Branch), wt.Branch) {
		t.Error("the branch was deleted with the worktree; commits would be unreachable")
	}
}

func TestArkIsLinkedIntoTheWorktree(t *testing.T) {
	// The second dogfood failure: `.ark/` is gitignored, so a fresh worktree
	// has no work records, and a bare `ark init` would mint a NEW repository
	// id and orphan the project's history. A Codex run read that correctly and
	// refused to do anything at all.
	repo := newRepo(t)
	write(t, filepath.Join(repo, ".ark", "config.toml"), "repository = \"01KXEGZW83HC33D53VW58CWQRZ\"\n")
	write(t, filepath.Join(repo, ".ark", "ark.db"), "sentinel\n")

	m := &worktree.Manager{Root: t.TempDir(), Ark: "definitely-not-installed"}
	wt, err := m.Create(context.Background(), worktree.Request{Repo: repo, RunID: "run-ark"})
	if err != nil {
		t.Fatal(err)
	}
	if !wt.Ark.Wanted || !wt.Ark.OK {
		t.Fatalf("Ark state = %+v, want a successful link", wt.Ark)
	}

	link := filepath.Join(wt.Dir, ".ark")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("no .ark in the worktree: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error(".ark is not a symlink, so the worktree does not share the repository's database")
	}
	// Shared, not copied: the run's records belong to the same history.
	body, err := os.ReadFile(filepath.Join(link, "config.toml"))
	if err != nil || !strings.Contains(string(body), "01KXEGZW83HC33D53VW58CWQRZ") {
		t.Errorf("the link does not resolve to the source's config: %v %q", err, body)
	}
	if !strings.Contains(wt.Ark.Note, "linked") {
		t.Errorf("Note does not say what happened: %q", wt.Ark.Note)
	}
}

func TestReapNeverDeletesTheSourceArk(t *testing.T) {
	// The link points at the repository's entire work-record database. Neither
	// git nor os.RemoveAll follows a symlink — that was checked before this
	// design was adopted — but "the recursive delete I am about to run does
	// not follow links" is not a thing to be merely fairly sure of, so the
	// link is removed first and this test says so.
	repo := newRepo(t)
	write(t, filepath.Join(repo, ".ark", "ark.db"), "must-survive\n")

	m := &worktree.Manager{Root: t.TempDir(), Ark: "definitely-not-installed"}
	ctx := context.Background()
	wt, err := m.Create(ctx, worktree.Request{Repo: repo, RunID: "run-reap"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Reap(ctx, wt); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(repo, ".ark", "ark.db"))
	if err != nil || strings.TrimSpace(string(body)) != "must-survive" {
		t.Fatalf("the reap damaged the source repository's Ark database: %v %q", err, body)
	}
}

func TestSubmoduleArkIsLinkedToo(t *testing.T) {
	// The elk superrepo's components each carry their own Ark repository, so a
	// run in the superrepo that reaches into scout/ hits the same wall one
	// level down.
	repo := newRepo(t)
	write(t, filepath.Join(repo, ".ark", "ark.db"), "root\n")
	write(t, filepath.Join(repo, "scout", ".ark", "ark.db"), "scout\n")
	write(t, filepath.Join(repo, "scout", "README.md"), "# scout\n")
	commit(t, repo, "add a component")

	m := &worktree.Manager{Root: t.TempDir(), Ark: "definitely-not-installed"}
	wt, err := m.Create(context.Background(), worktree.Request{Repo: repo, RunID: "run-sub"})
	if err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(wt.Dir, "scout", ".ark", "ark.db")); err != nil ||
		strings.TrimSpace(string(body)) != "scout" {
		t.Fatalf("the component's Ark was not linked: %v %q", err, body)
	}
	var linkedScout bool
	for _, l := range wt.Ark.Linked {
		if l == "scout" {
			linkedScout = true
		}
	}
	if !linkedScout {
		t.Errorf("Linked = %v, want it to name scout", wt.Ark.Linked)
	}
}

func TestArkIsNotWantedWhenTheSourceHasNone(t *testing.T) {
	repo := newRepo(t)
	m := &worktree.Manager{Root: t.TempDir()}
	wt, err := m.Create(context.Background(), worktree.Request{Repo: repo, RunID: "run-noark"})
	if err != nil {
		t.Fatal(err)
	}
	if wt.Ark.Wanted || wt.Ark.OK {
		t.Errorf("Ark state = %+v, want nothing wanted", wt.Ark)
	}
	if _, err := os.Lstat(filepath.Join(wt.Dir, ".ark")); !os.IsNotExist(err) {
		t.Error("a .ark was invented for a repository that has none")
	}
}

func TestBaseIsTheRemoteDefaultNotTheSourceHEAD(t *testing.T) {
	// The bug: HEAD is whatever the developer last checked out. On the machine
	// this was found on it was a stale release branch several days behind
	// main, so every deliverable would have been a conflict.
	clone, origin := newRepoWithOrigin(t)

	// main moves on in the origin...
	write(t, filepath.Join(origin, "new.txt"), "landed on main\n")
	commit(t, origin, "a commit the stale checkout has never seen")
	wantSHA := git(t, origin, "rev-parse", "HEAD")

	// ...while the local checkout sits on an old release branch.
	git(t, clone, "checkout", "-q", "-b", "release/build-39")
	write(t, filepath.Join(clone, "stale.txt"), "old\n")
	commit(t, clone, "a stale release commit")
	staleSHA := git(t, clone, "rev-parse", "HEAD")

	m := &worktree.Manager{Root: t.TempDir()}
	wt, err := m.Create(context.Background(), worktree.Request{Repo: clone, RunID: "run-base"})
	if err != nil {
		t.Fatal(err)
	}
	if wt.BaseSHA == staleSHA {
		t.Fatal("the run was based on the source checkout's stale HEAD")
	}
	if wt.BaseSHA != wantSHA {
		t.Errorf("base = %s, want the freshly fetched origin default %s", wt.BaseSHA, wantSHA)
	}
	if !strings.Contains(wt.BaseRef, "origin/") {
		t.Errorf("BaseRef = %q, want a remote ref", wt.BaseRef)
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, "new.txt")); err != nil {
		t.Errorf("the worktree does not have main's newest commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, "stale.txt")); err == nil {
		t.Error("the worktree carries the stale release branch's work")
	}
}

func TestAnExplicitBaseRefWins(t *testing.T) {
	clone, _ := newRepoWithOrigin(t)
	git(t, clone, "checkout", "-q", "-b", "release/build-39")
	write(t, filepath.Join(clone, "release.txt"), "release\n")
	commit(t, clone, "release work")
	releaseSHA := git(t, clone, "rev-parse", "HEAD")

	m := &worktree.Manager{Root: t.TempDir()}
	wt, err := m.Create(context.Background(), worktree.Request{
		Repo: clone, RunID: "run-explicit", BaseRef: "release/build-39",
	})
	if err != nil {
		t.Fatal(err)
	}
	if wt.BaseSHA != releaseSHA {
		t.Errorf("base = %s, want the configured ref %s", wt.BaseSHA, releaseSHA)
	}
	if wt.BaseRef != "release/build-39" {
		t.Errorf("BaseRef = %q", wt.BaseRef)
	}
}

func TestAnUnresolvableBaseRefFallsBackRatherThanFailing(t *testing.T) {
	repo := newRepo(t) // no origin at all
	m := &worktree.Manager{Root: t.TempDir()}
	wt, err := m.Create(context.Background(), worktree.Request{
		Repo: repo, RunID: "run-nobase", BaseRef: "origin/nope",
	})
	if err != nil {
		t.Fatalf("a run refused to start over an unresolvable base: %v", err)
	}
	if wt.BaseRef != "HEAD" {
		t.Errorf("BaseRef = %q, want the HEAD fallback", wt.BaseRef)
	}
	// The fallback has to be loud: a run based on HEAD may be diffing against
	// the wrong world.
	if !strings.Contains(wt.BaseNote, "source checkout's HEAD") {
		t.Errorf("BaseNote does not warn about the fallback: %q", wt.BaseNote)
	}
}

func TestCreateRunsWorktreeInitScript(t *testing.T) {
	repo := newRepo(t)
	write(t, filepath.Join(repo, "scripts", "worktree-init.sh"),
		"#!/usr/bin/env bash\nset -e\necho populated > populated.txt\n")
	if err := os.Chmod(filepath.Join(repo, "scripts", "worktree-init.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	commit(t, repo, "add worktree-init.sh")

	m := &worktree.Manager{Root: t.TempDir()}
	wt, err := m.Create(context.Background(), worktree.Request{Repo: repo, RunID: "run-init"})
	if err != nil {
		t.Fatal(err)
	}
	if !wt.Initialised {
		t.Error("Initialised is false although the script exists")
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, "populated.txt")); err != nil {
		// A superrepo worktree with empty submodule directories reads to an
		// agent as "the code is missing", which is the bug this script exists
		// to prevent.
		t.Fatalf("worktree-init.sh did not run: %v", err)
	}
}

func TestCreateRemovesTheWorktreeWhenInitFails(t *testing.T) {
	repo := newRepo(t)
	write(t, filepath.Join(repo, ".ark", "ark.db"), "must-survive\n")
	write(t, filepath.Join(repo, "scripts", "worktree-init.sh"), "#!/usr/bin/env bash\nexit 3\n")
	if err := os.Chmod(filepath.Join(repo, "scripts", "worktree-init.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	commit(t, repo, "add a failing worktree-init.sh")

	root := t.TempDir()
	m := &worktree.Manager{Root: root, Ark: "definitely-not-installed"}
	if _, err := m.Create(context.Background(), worktree.Request{Repo: repo, RunID: "run-bad"}); err == nil {
		t.Fatal("Create succeeded although the bootstrap failed")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("an unusable worktree was left behind: %v", entries)
	}
	// The unwind runs the same reap, so it owes the same guarantee.
	if body, err := os.ReadFile(filepath.Join(repo, ".ark", "ark.db")); err != nil ||
		strings.TrimSpace(string(body)) != "must-survive" {
		t.Fatalf("the unwind damaged the source repository's Ark: %v %q", err, body)
	}
}

func TestCreateIsRepeatableAfterALapsedLease(t *testing.T) {
	// A run reclaimed after its lease lapsed starts again, and the first
	// attempt's branch and directory must not be something to trip over.
	repo := newRepo(t)
	write(t, filepath.Join(repo, ".ark", "ark.db"), "must-survive\n")
	m := &worktree.Manager{Root: t.TempDir(), Ark: "definitely-not-installed"}
	ctx := context.Background()

	first, err := m.Create(ctx, worktree.Request{Repo: repo, RunID: "run-twice"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(first.Dir, "scratch.txt"), "half-done")

	second, err := m.Create(ctx, worktree.Request{Repo: repo, RunID: "run-twice"})
	if err != nil {
		t.Fatalf("a second Create for the same run failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second.Dir, "scratch.txt")); !os.IsNotExist(err) {
		t.Error("the second worktree inherited the first attempt's leftovers")
	}
	if body, err := os.ReadFile(filepath.Join(repo, ".ark", "ark.db")); err != nil ||
		strings.TrimSpace(string(body)) != "must-survive" {
		t.Fatalf("clearing the leftover damaged the source repository's Ark: %v %q", err, body)
	}
}

func TestCreateRejectsSomethingThatIsNotARepository(t *testing.T) {
	m := &worktree.Manager{Root: t.TempDir()}
	_, err := m.Create(context.Background(), worktree.Request{Repo: t.TempDir(), RunID: "run-1"})
	if err == nil {
		t.Fatal("a directory that is not a git repository was accepted")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error does not say what is wrong: %v", err)
	}
}

func TestSafeID(t *testing.T) {
	for in, want := range map[string]string{
		"9f1c-4a2b":        "9f1c-4a2b",
		"../../etc/passwd": "etc-passwd",
		"a b/c":            "a-b-c",
	} {
		if got := worktree.SafeID(in); got != want {
			t.Errorf("SafeID(%q) = %q, want %q", in, got, want)
		}
	}
}
