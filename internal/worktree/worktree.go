// Package worktree creates and reaps the per-run git worktrees Rein drives
// agents in.
//
// One worktree per run, under the Rein home's work directory, on a branch of
// its own. The isolation is the point: a queue-driven daemon runs a repo's
// hooks and MCP servers unprompted (elk docs/rein.md §3), so it does that in a
// directory nothing else is using, on a branch nobody else is on, and takes
// the directory away afterwards.
//
// Two things a fresh worktree does NOT inherit, both of which stopped real runs
// dead in the Phase 0 dogfood (`ark:rein#18 (01M180K2307FQZM62VMGYHZS6Q)`):
//
//   - **The repository's Ark.** `.ark/` is gitignored, so `git worktree add`
//     produces a tree with no work records at all — and every repo's CLAUDE.md
//     forbids a bare `ark init`, which mints a NEW repository id and orphans
//     the project's history. A Codex run read that correctly and refused to do
//     anything. [Manager.Create] provisions it instead; see [provisionArk].
//   - **A current base.** `HEAD` is whatever the developer last checked out,
//     which on a real machine is routinely a stale release branch. Rein bases
//     a run on the remote default branch; see [Manager.resolveBase].
//
// Reaping is driven by the tracker, not by a merge: Rein removes a worktree
// when the run it belongs to has been submitted, which is Loom's rule and the
// reason its own reaper found 44 stale worktrees and 54 GB on one worker.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// BranchPrefix is what every Rein branch starts with, so a person looking at
// `git branch` in a shared clone can tell which branches a runner made.
const BranchPrefix = "rein/run-"

// InitScript is the bootstrap a repository may carry for a fresh worktree.
// The Elk superrepo needs it — a git worktree does not inherit submodule
// working trees, so a session that opens in one finds every component
// directory empty and concludes the code is missing. Rein runs it when it is
// there and ignores its absence.
const InitScript = "scripts/worktree-init.sh"

// ArkDir is the work-record directory Rein provisions into a worktree.
const ArkDir = ".ark"

// fetchTimeout bounds the `git fetch` that makes the remote default branch
// current. It is best effort: a runner on a flaky connection should start a run
// from slightly stale refs, not refuse to start one.
const fetchTimeout = 90 * time.Second

// arkVerifyTimeout bounds the `ark status` that proves the provisioning worked.
const arkVerifyTimeout = 15 * time.Second

// Manager makes worktrees under one root.
type Manager struct {
	// Root is where worktrees are created: <work_dir>/<run id>.
	Root string

	// Git is the git executable. Empty means "git" on PATH.
	Git string

	// Ark is the ark executable used only to VERIFY provisioning. Empty means
	// "ark" on PATH; when it is not installed, provisioning still happens and
	// is reported as unverified rather than failing.
	Ark string

	// Logf receives one line per external command and per decision.
	Logf func(format string, args ...any)
}

// Request is one worktree to create.
type Request struct {
	// Repo is the repository to cut from.
	Repo string

	// RunID is Elk's run id; it names the directory and the branch.
	RunID string

	// BaseRef is the ref to branch from. Empty means resolve the remote
	// default branch, which is almost always what is wanted — see
	// [Manager.resolveBase].
	BaseRef string
}

// Worktree is one checkout Rein owns.
type Worktree struct {
	// Dir is the working directory the agent runs in.
	Dir string
	// Repo is the repository it was cut from.
	Repo string
	// Branch is the branch created for the run.
	Branch string
	// RunID is Elk's run id.
	RunID string
	// BaseRef is the ref the branch was cut from, and BaseSHA what that
	// resolved to. Both go into the deliverable, because "which commit was
	// this work based on" is the first question a reviewer asks.
	BaseRef string
	BaseSHA string
	// BaseNote says how BaseRef was chosen, including when it had to fall
	// back to the source checkout's HEAD.
	BaseNote string
	// Initialised records whether [InitScript] ran.
	Initialised bool
	// Ark records how the repository's work records were provisioned.
	Ark ArkState
}

// ArkState is what happened to `.ark`.
type ArkState struct {
	// Wanted is false when the source repository has no `.ark` at all, in
	// which case none of the rest matters.
	Wanted bool
	// OK reports whether an agent can use ark in this worktree.
	OK bool
	// Linked are the paths inside the worktree that now have a `.ark`, "." for
	// the root and a subdirectory name for each submodule.
	Linked []string
	// Note is a sentence for a human: how it was provisioned, or why it was
	// not. It reaches the run's first progress report, so an agent's refusal
	// to work is never a mystery.
	Note string
}

// unsafe matches everything a run id may not contribute to a path or a branch
// name. Elk run ids are uuids, but the id arrives over the wire and is about
// to become a filesystem path and a git ref — so it is sanitised rather than
// trusted.
var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SafeID reduces a run id to what may appear in a path and a git ref.
func SafeID(runID string) string {
	s := unsafe.ReplaceAllString(runID, "-")
	s = strings.Trim(s, "-.")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func (m *Manager) log(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

func (m *Manager) git() string {
	if m.Git != "" {
		return m.Git
	}
	return "git"
}

func (m *Manager) ark() string {
	if m.Ark != "" {
		return m.Ark
	}
	return "ark"
}

// Create cuts a worktree for a run, provisions the repository's Ark into it,
// and runs [InitScript] if the repository has one.
//
// The branch is created with `-B`, which resets an existing one rather than
// failing: a run reclaimed after a lapsed lease must be able to start again,
// and the first attempt's branch is not something to trip over.
func (m *Manager) Create(ctx context.Context, req Request) (*Worktree, error) {
	if m.Root == "" {
		return nil, errors.New("worktree: no root directory")
	}
	if req.Repo == "" {
		return nil, errors.New("worktree: no repository")
	}
	id := SafeID(req.RunID)
	if id == "" {
		return nil, fmt.Errorf("worktree: run id %q has nothing usable in it", req.RunID)
	}
	repoAbs, err := filepath.Abs(req.Repo)
	if err != nil {
		return nil, fmt.Errorf("worktree: %s: %w", req.Repo, err)
	}
	if err := m.assertRepo(ctx, repoAbs); err != nil {
		return nil, err
	}

	dir := filepath.Join(m.Root, id)
	if err := os.MkdirAll(m.Root, 0o700); err != nil {
		return nil, fmt.Errorf("worktree: create %s: %w", m.Root, err)
	}
	// A leftover directory from an earlier attempt would make `worktree add`
	// fail on a path that is not git's to explain. Clear it, and its
	// registration, first — unlinking any .ark before anything recursive runs.
	if _, err := os.Stat(dir); err == nil {
		m.log("worktree: removing a leftover checkout at %s", dir)
		m.unlinkArk(dir)
		_ = m.run(ctx, repoAbs, "worktree", "remove", "--force", dir)
		if err := os.RemoveAll(dir); err != nil {
			return nil, fmt.Errorf("worktree: clear %s: %w", dir, err)
		}
	}
	_ = m.run(ctx, repoAbs, "worktree", "prune")

	base, note := m.resolveBase(ctx, repoAbs, req.BaseRef)
	m.log("worktree: basing run %s on %s (%s)", req.RunID, base, note)

	branch := BranchPrefix + id
	if err := m.run(ctx, repoAbs, "worktree", "add", "-B", branch, dir, base); err != nil {
		return nil, err
	}

	wt := &Worktree{
		Dir: dir, Repo: repoAbs, Branch: branch, RunID: req.RunID,
		BaseRef: base, BaseNote: note,
	}
	wt.BaseSHA = strings.TrimSpace(m.output(ctx, dir, "rev-parse", "HEAD"))

	// Ark before the bootstrap script: the script belongs to the repository and
	// may well want to record what it did.
	wt.Ark = m.provisionArk(ctx, repoAbs, dir)
	m.log("worktree: %s", wt.Ark.Note)

	init, err := m.runInitScript(ctx, dir)
	if err != nil {
		// The worktree exists but is unusable — an Elk superrepo worktree
		// with empty submodule directories reads to an agent as "the code is
		// missing". Take it away rather than hand that over.
		_ = m.Reap(ctx, wt)
		return nil, err
	}
	wt.Initialised = init

	// Submodules only exist once the bootstrap has populated them, and each of
	// the superrepo's components carries its own Ark repository.
	if sub := m.provisionSubmoduleArk(ctx, repoAbs, dir); len(sub) > 0 {
		wt.Ark.Linked = append(wt.Ark.Linked, sub...)
		wt.Ark.Note += fmt.Sprintf(" Also linked %s.", strings.Join(sub, ", "))
	}
	return wt, nil
}

// resolveBase decides which commit a run starts from, and says why.
//
// **Not the source checkout's HEAD**, which is the obvious choice and the wrong
// one: HEAD is whatever the developer last checked out. On the machine this was
// found on it was a stale `release/build-39`, several days behind main, so
// every deliverable would have arrived as a conflict against work that had
// already moved.
//
// The order is an explicit ref, then the remote's own default branch, then the
// conventional names, and only then HEAD — which is reported loudly, because a
// run based on it is a run whose diff may be against the wrong world.
func (m *Manager) resolveBase(ctx context.Context, repo, want string) (ref, note string) {
	if want != "" {
		if m.refExists(ctx, repo, want) {
			return want, "configured base_ref"
		}
		m.log("worktree: configured base ref %q does not resolve in %s; falling back", want, repo)
	}

	if m.hasOrigin(ctx, repo) {
		fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
		err := m.run(fetchCtx, repo, "fetch", "--quiet", "origin")
		cancel()
		if err != nil {
			// Stale refs beat no run at all.
			m.log("worktree: git fetch origin failed (%v); using the refs already here", err)
		}
		// The remote's own idea of its default branch, set by clone.
		if out := strings.TrimSpace(m.output(ctx, repo, "symbolic-ref", "refs/remotes/origin/HEAD")); out != "" {
			if candidate := strings.TrimPrefix(out, "refs/remotes/"); m.refExists(ctx, repo, candidate) {
				return candidate, "the remote's default branch"
			}
		}
		for _, candidate := range []string{"origin/main", "origin/master"} {
			if m.refExists(ctx, repo, candidate) {
				return candidate, "the conventional default branch"
			}
		}
	}

	return "HEAD", "no remote default could be resolved, so this is the source checkout's HEAD — " +
		"if that checkout is on an old branch, so is this run"
}

func (m *Manager) hasOrigin(ctx context.Context, repo string) bool {
	return strings.TrimSpace(m.output(ctx, repo, "remote", "get-url", "origin")) != ""
}

func (m *Manager) refExists(ctx context.Context, repo, ref string) bool {
	return strings.TrimSpace(m.output(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")) != ""
}

// provisionArk gives the worktree the repository's work records.
//
// A symlink, so the worktree shares the source's `ark.db`, its config and its
// object store — which is the point: a run's Ark records belong to the same
// repository history as everything else, and a separate database would be a
// second, orphaned one. It is also the only approach that cannot mint a new
// repository id, which a bare `ark init` does and which has already cost scout
// its history once.
//
// Verified in both directions before being relied on: reads and writes pass
// through to the source, and neither `git worktree remove --force` nor
// `os.RemoveAll` follows the link — see [Manager.unlinkArk], which removes it
// anyway.
func (m *Manager) provisionArk(ctx context.Context, repo, dir string) ArkState {
	src := filepath.Join(repo, ArkDir)
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return ArkState{Note: "the source repository has no .ark, so this run keeps no Ark records"}
	}
	st := ArkState{Wanted: true}
	dst := filepath.Join(dir, ArkDir)
	if _, err := os.Lstat(dst); err == nil {
		st.OK, st.Linked = true, []string{"."}
		st.Note = "the worktree already had a .ark; left alone"
		return st
	}
	if err := os.Symlink(src, dst); err != nil {
		st.Note = fmt.Sprintf("could not link .ark into the worktree (%v), so `ark` will report no .ark "+
			"directory here. Do NOT run `ark init` — it mints a new repository id and orphans this "+
			"project's history. Record the work in the deliverable instead.", err)
		return st
	}
	st.Linked = []string{"."}
	st.OK = true
	st.Note = "linked .ark to the source repository, so Ark records land in the project's own history"

	// Prove it rather than assume it, when ark is here to ask.
	if _, err := exec.LookPath(m.ark()); err != nil {
		st.Note += " (unverified: ark is not on PATH)"
		return st
	}
	verifyCtx, cancel := context.WithTimeout(ctx, arkVerifyTimeout)
	defer cancel()
	if err := exec.CommandContext(verifyCtx, m.ark(), "-C", dir, "status").Run(); err != nil {
		st.OK = false
		st.Note = fmt.Sprintf("linked .ark into the worktree, but `ark -C %s status` still fails (%v). "+
			"Do NOT run `ark init` — it mints a new repository id and orphans this project's history.", dir, err)
	}
	return st
}

// provisionSubmoduleArk mirrors the same link one level down, for a superrepo
// whose components each carry their own Ark repository — elk's scout, pulse,
// signal and the rest. It runs after the bootstrap script, because until that
// has run the submodule directories are empty.
func (m *Manager) provisionSubmoduleArk(ctx context.Context, repo, dir string) []string {
	entries, err := os.ReadDir(repo)
	if err != nil {
		return nil
	}
	var linked []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == ".git" || strings.HasPrefix(name, ".") {
			continue
		}
		src := filepath.Join(repo, name, ArkDir)
		if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
			continue
		}
		target := filepath.Join(dir, name)
		if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
			continue // the submodule is not checked out in this worktree
		}
		dst := filepath.Join(target, ArkDir)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if err := os.Symlink(src, dst); err != nil {
			m.log("worktree: could not link %s/.ark: %v", name, err)
			continue
		}
		linked = append(linked, name)
	}
	_ = ctx
	return linked
}

// runInitScript runs the repository's worktree bootstrap if it has one,
// reporting whether it ran.
func (m *Manager) runInitScript(ctx context.Context, dir string) (bool, error) {
	script := filepath.Join(dir, filepath.FromSlash(InitScript))
	if _, err := os.Stat(script); err != nil {
		return false, nil
	}
	m.log("worktree: running %s in %s", InitScript, dir)
	cmd := exec.CommandContext(ctx, "bash", filepath.FromSlash(InitScript))
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("worktree: %s failed in %s: %w\n%s", InitScript, dir, err, trimOutput(out))
	}
	return true, nil
}

// Reap removes a worktree.
//
// The BRANCH survives. `git worktree remove --force` discards uncommitted
// changes, so anything the agent committed stays reachable in the repository
// while anything it did not is gone — which is the trade a v0 that reaps
// immediately after submit is making, and the reason `rein run
// --keep-worktrees` exists.
//
// Every `.ark` link is removed FIRST. Neither git nor os.RemoveAll follows a
// symlink — that was checked before this design was adopted — but the target is
// the repository's entire work-record database, and "the tool I am about to run
// recursively does not follow links" is not a thing to be merely fairly sure of.
func (m *Manager) Reap(ctx context.Context, wt *Worktree) error {
	if wt == nil || wt.Dir == "" {
		return nil
	}
	m.unlinkArk(wt.Dir)
	for _, sub := range wt.Ark.Linked {
		if sub != "." {
			m.unlinkArk(filepath.Join(wt.Dir, sub))
		}
	}
	if err := m.run(ctx, wt.Repo, "worktree", "remove", "--force", wt.Dir); err != nil {
		// git may refuse for a reason that leaves the directory behind. The
		// directory is what fills a disk, so take it either way and prune the
		// registration.
		m.log("worktree: %v; removing %s directly", err, wt.Dir)
		if rmErr := os.RemoveAll(wt.Dir); rmErr != nil {
			return fmt.Errorf("worktree: reap %s: %w", wt.Dir, rmErr)
		}
		_ = m.run(ctx, wt.Repo, "worktree", "prune")
	}
	return nil
}

// unlinkArk removes a `.ark` entry only when it is a symlink. A real directory
// is somebody's data and is left exactly where it is.
func (m *Manager) unlinkArk(dir string) {
	p := filepath.Join(dir, ArkDir)
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return
	}
	if err := os.Remove(p); err != nil {
		m.log("worktree: could not unlink %s: %v", p, err)
	}
}

// assertRepo refuses a path that is not a git working tree, with a message
// that names the path — the alternative is `worktree add` failing later with
// something a person has to go and look up.
func (m *Manager) assertRepo(ctx context.Context, repo string) error {
	if fi, err := os.Stat(repo); err != nil || !fi.IsDir() {
		return fmt.Errorf("worktree: %s is not a directory Rein can cut a worktree from", repo)
	}
	if err := m.run(ctx, repo, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("worktree: %s is not a git repository: %w", repo, err)
	}
	return nil
}

func (m *Manager) run(ctx context.Context, dir string, args ...string) error {
	full := append([]string{"-C", dir}, args...)
	m.log("worktree: git %s", strings.Join(full, " "))
	cmd := exec.CommandContext(ctx, m.git(), full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, trimOutput(out))
	}
	return nil
}

// output runs a git command and returns stdout, or "" if it failed. Used for
// the probes where failure is an answer rather than an error.
func (m *Manager) output(ctx context.Context, dir string, args ...string) string {
	full := append([]string{"-C", dir}, args...)
	out, err := exec.CommandContext(ctx, m.git(), full...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func trimOutput(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 2000 {
		return s[:2000] + "…"
	}
	return s
}
