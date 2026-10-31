//go:build !windows

package runner_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/check"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
	"github.com/elk-work/rein/internal/worktree"
)

// A check packet driven through the real loop, end to end: the fake Elk hands
// out a claim, the check adapter (with a stub act) reads the packet, the real
// worktree manager fetches the commit from origin and cuts the worktree
// detached there, act's log becomes progress, and the deliverable and the
// suite record come out the other side. Nothing about the loop is special to
// checks; that is the point of the test.

const checkQueue = "lab-checks"

const floorYML = `name: floor
on: [push, workflow_dispatch]
jobs:
  floor:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`

type checkRig struct {
	t       *testing.T
	elk     *elktest.Server
	cfg     config.Config
	store   keyring.Store
	records string
	cache   string // act's cache, never the real one
	origin  string
	clone   string
	log     *strings.Builder

	// logs and keepRuns, when set, are the run logs and log_keep_runs the
	// loop is given; without logs nothing is tidied after the run.
	logs     *runlog.Store
	keepRuns int
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newCheckRig builds an origin with the workflow, a clone of it configured
// as this machine's checkout, and a check adapter whose act is a stub that
// replays fixture and exits with code.
func newCheckRig(t *testing.T, fixture string, code int) *checkRig {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	home := t.TempDir()
	r := &checkRig{t: t, elk: elktest.New(t, token), records: filepath.Join(home, "checks"),
		cache: filepath.Join(home, "cache", "act"), log: &strings.Builder{}}

	r.origin = filepath.Join(home, "origin")
	if err := os.MkdirAll(filepath.Join(r.origin, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.origin, ".github", "workflows", "floor.yml"), []byte(floorYML), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, r.origin, "init", "-q", "-b", "main")
	gitIn(t, r.origin, "add", "-A")
	gitIn(t, r.origin, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "init")
	r.clone = filepath.Join(home, "scout")
	if out, err := exec.Command("git", "clone", "-q", r.origin, r.clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	fixtures, err := filepath.Abs(filepath.Join("..", "adapter", "check", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(home, "act")
	script := "#!/bin/sh\ncase \" $* \" in *\" --list \"*) cat '" + filepath.Join(fixtures, "act-list.txt") + "'; exit 0;; esac\n" +
		"cat '" + filepath.Join(fixtures, fixture) + "'\nexit " + map[int]string{0: "0", 1: "1"}[code] + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	adapter.Unregister(check.Kind)
	if err := adapter.Register(&check.Adapter{Act: stub, RecordRoot: r.records, CacheRoot: r.cache, Pulse: "rein-test-no-pulse"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adapter.Unregister(check.Kind)
		adapter.MustRegister(check.New())
	})

	r.cfg = config.Config{
		HostID: "lab", MachineID: "machine-1", Workspace: space,
		WorkDir: filepath.Join(home, "work"),
		Repos:   map[string]string{"scout": r.clone},
		Elk:     config.Elk{MCPURL: r.elk.URL},
		Queues: []config.Queue{{Name: checkQueue, AgentKind: check.Kind,
			Settings: map[string]string{"executor": "host"}}},
	}
	store := keyring.NewFileStore(filepath.Join(home, keyring.FileName))
	if err := store.Set(space, checkQueue, token); err != nil {
		t.Fatal(err)
	}
	r.store = store
	r.elk.Text("heartbeat_executor", `Heartbeat recorded for queue "`+checkQueue+`". You are online for the next 15 minutes. 1 run waiting.`)
	r.elk.Text("report_progress", "Progress recorded on `run-check-1`. Lease refreshed.")
	r.elk.Text("submit_deliverable", "Deliverable saved: run `run-check-1` is `ready`. The user will review it in Elk.")
	return r
}

// pushToOrigin adds a commit the clone has never fetched and returns it.
func (r *checkRig) pushToOrigin() string {
	path := filepath.Join(r.origin, "later.txt")
	if err := os.WriteFile(path, []byte("later\n"), 0o644); err != nil {
		r.t.Fatal(err)
	}
	gitIn(r.t, r.origin, "add", "-A")
	gitIn(r.t, r.origin, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "later")
	return gitIn(r.t, r.origin, "rev-parse", "HEAD")
}

func checkOrder(sha string) string {
	return "Claimed Elk run `run-check-1` (contract v2).\n\n**Direction:** Run the floor check\n" +
		"**Action id:** `act-1`\n\n## Handoff packet\n\n### Work order\nrepo: scout\nkind: check\n" +
		"requires: agent:runner\n\n### Check\n\n```json\n{\"repo\":\"elk-work/scout\",\"sha\":\"" + sha +
		"\",\"ref\":\"refs/heads/main\",\"suite\":\"floor\",\"workflow\":\".github/workflows/floor.yml\"," +
		"\"job\":\"floor\",\"runs_on\":[\"ubuntu-latest\"],\"provider\":\"linux-lab\",\"changed_paths\":[\"later.txt\"]}\n```\n\n" +
		"### Required capabilities — preflight BEFORE starting\n- act\n" +
		"Fail fast: if ANY capability is missing, do no work.\n"
}

func (r *checkRig) run(claim string) {
	r.t.Helper()
	r.elk.Text("claim_run", claim)
	run, err := runner.New(runner.Options{
		Config: r.cfg, Store: r.store, Out: r.log, Once: true,
		Worktrees:        &worktree.Manager{Root: r.cfg.WorkDir},
		HostCapabilities: runner.NewHostCapabilities("a test", "elk-connector", "git", "worktree", "act"),
		StallAfter:       30 * time.Second,
		ReviewTimeout:    time.Second, ReviewPollInterval: 100 * time.Millisecond,
		Logs: r.logs, LogKeepRuns: r.keepRuns,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := run.Run(ctx); err != nil {
		r.t.Fatalf("run: %v\n%s", err, r.log)
	}
}

func (r *checkRig) submitted() elktest.Call {
	r.t.Helper()
	calls := r.elk.CallsTo("submit_deliverable")
	if len(calls) != 1 {
		r.t.Fatalf("%d submits, want 1\n%s", len(calls), r.log)
	}
	return calls[0]
}

func (r *checkRig) record() check.SuiteRecord {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.records, "run-check-1", "suite-record.json"))
	if err != nil {
		r.t.Fatalf("no suite record: %v", err)
	}
	var rec check.SuiteRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		r.t.Fatal(err)
	}
	return rec
}

// inlineRecord is the fenced JSON under the deliverable's last "### Suite
// record" line, read the way Elk reads it, and checked to be the
// deliverable's last section.
func inlineRecord(t *testing.T, deliverable string) string {
	t.Helper()
	i := strings.LastIndex(deliverable, "\n### Suite record\n")
	if i < 0 {
		t.Fatalf("no ### Suite record section:\n%s", deliverable)
	}
	rest := strings.TrimLeft(deliverable[i+len("\n### Suite record\n"):], "\n")
	if !strings.HasPrefix(rest, "```json\n") {
		t.Fatalf("the section does not open with a json fence:\n%s", rest)
	}
	body, after, ok := strings.Cut(strings.TrimPrefix(rest, "```json\n"), "\n```")
	if !ok || strings.TrimSpace(after) != "" {
		t.Fatalf("the json block is unclosed or not last:\n%s", rest)
	}
	if !json.Valid([]byte(body)) {
		t.Fatalf("the block is not JSON:\n%s", body)
	}
	return body + "\n"
}

func (r *checkRig) recordBytes() string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.records, "run-check-1", "suite-record.json"))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func TestACheckPacketRunsFromClaimToSubmit(t *testing.T) {
	r := newCheckRig(t, "act-pass.jsonl", 0)
	sha := r.pushToOrigin()
	r.run(checkOrder(sha))

	sub := r.submitted()
	if sub.Arg("status") != "ready" {
		t.Errorf("status = %q, want ready", sub.Arg("status"))
	}
	if d := sub.Arg("deliverable"); !strings.HasPrefix(d, "The floor check passed on scout.") || !strings.Contains(d, sha) {
		t.Errorf("deliverable:\n%s", d)
	}
	rec := r.record()
	if rec.Result != "pass" || rec.SHA != sha || rec.Runner != checkQueue || rec.Provider != "linux-lab" {
		t.Errorf("record: %+v", rec)
	}
	// What Elk receives ends with the record, byte for byte the file, and —
	// with no pulse on this machine — no emitted_by, so Elk forwards it.
	if body := inlineRecord(t, sub.Arg("deliverable")); body != r.recordBytes() || strings.Contains(body, "emitted_by") {
		t.Errorf("inline record:\n%s\nfile:\n%s", body, r.recordBytes())
	}
	opening := ""
	for _, c := range r.elk.CallsTo("report_progress") {
		if strings.Contains(c.Arg("body"), "Claimed by Rein") {
			opening = c.Arg("body")
		}
	}
	if !strings.Contains(opening, "Detached at") || !strings.Contains(opening, "fetched from origin") {
		t.Errorf("opening report: %q", opening)
	}
	entries, _ := os.ReadDir(r.cfg.WorkDir)
	if len(entries) != 0 {
		t.Errorf("the worktree was not reaped: %v", entries)
	}
}

// A check leaves nothing to pile up (ark:rein#91): where the worktree is
// reaped, the oldest records go past log_keep_runs and a host-executor
// directory act left behind a day ago goes too, while this run's own record,
// act's action clones and its tool cache stay.
func TestACheckTidiesUpItsRecordsAndActsLeftovers(t *testing.T) {
	r := newCheckRig(t, "act-pass.jsonl", 0)
	sha := r.pushToOrigin()
	r.logs, r.keepRuns = openRunLogs(t), 2
	aged := func(dir string, age time.Duration, children ...string) {
		t.Helper()
		at := time.Now().Add(-age)
		for _, c := range append(children, "") {
			if err := os.MkdirAll(filepath.Join(dir, c), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(filepath.Join(dir, c), at, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i, id := range []string{"old-1", "old-2", "old-3"} {
		aged(filepath.Join(r.records, id), time.Duration(3-i)*time.Hour)
	}
	aged(filepath.Join(r.cache, "ea7e347e5de3bc16"), 48*time.Hour, "act", "hostexecutor", "tmp")
	aged(filepath.Join(r.cache, "actions-setup-node@v7"), 48*time.Hour, "dist")
	aged(filepath.Join(r.cache, "tool_cache"), 48*time.Hour, "node")

	r.run(checkOrder(sha))

	if sub := r.submitted(); sub.Arg("status") != "ready" {
		t.Errorf("status = %q, want ready", sub.Arg("status"))
	}
	left, err := os.ReadDir(r.records)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range left {
		ids = append(ids, e.Name())
	}
	if strings.Join(ids, ",") != "old-3,run-check-1" {
		t.Errorf("records left = %v, want this run's and the newest old one\n%s", ids, r.log)
	}
	cached, err := os.ReadDir(r.cache)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, e := range cached {
		kept = append(kept, e.Name())
	}
	if strings.Join(kept, ",") != "actions-setup-node@v7,tool_cache" {
		t.Errorf("act's cache left = %v\n%s", kept, r.log)
	}
}

func TestAFailedCheckIsAReadyRunThatSaysFail(t *testing.T) {
	r := newCheckRig(t, "act-fail.jsonl", 1)
	sha := gitIn(t, r.clone, "rev-parse", "HEAD")
	r.run(checkOrder(sha))
	sub := r.submitted()
	if sub.Arg("status") != "ready" || !strings.Contains(sub.Arg("deliverable"), "The floor check failed on scout") {
		t.Errorf("status %q, deliverable:\n%s", sub.Arg("status"), sub.Arg("deliverable"))
	}
	if rec := r.record(); rec.Result != "fail" {
		t.Errorf("record result %q", rec.Result)
	}
	if body := inlineRecord(t, sub.Arg("deliverable")); body != r.recordBytes() {
		t.Errorf("inline record:\n%s", body)
	}
}

// A check act could not run is a stuck run, and its deliverable still ends
// with the record: an error is history too.
func TestACheckThatCouldNotRunIsStuckAndStillCarriesItsRecord(t *testing.T) {
	r := newCheckRig(t, "act-setup-failed.jsonl", 1)
	sha := gitIn(t, r.clone, "rev-parse", "HEAD")
	r.run(checkOrder(sha))
	sub := r.submitted()
	if sub.Arg("status") != "stuck" || !strings.HasPrefix(sub.Arg("title"), "Did not finish") {
		t.Errorf("status %q, title %q", sub.Arg("status"), sub.Arg("title"))
	}
	if body := inlineRecord(t, sub.Arg("deliverable")); body != r.recordBytes() || !strings.Contains(body, `"result": "error"`) {
		t.Errorf("inline record:\n%s", body)
	}
}

func TestACheckAtAnUnfetchableCommitIsStuckWithNothingOnDisk(t *testing.T) {
	r := newCheckRig(t, "act-pass.jsonl", 0)
	r.run(checkOrder(strings.Repeat("ab", 20)))
	sub := r.submitted()
	if sub.Arg("status") != "stuck" || sub.Arg("stop_code") != "repo_access" ||
		!strings.Contains(sub.Arg("title"), "could not be fetched from origin") {
		t.Errorf("status %q, stop_code %q, title %q", sub.Arg("status"), sub.Arg("stop_code"), sub.Arg("title"))
	}
	if _, err := os.Stat(r.cfg.WorkDir); !os.IsNotExist(err) {
		t.Errorf("a refused check left %s on disk: %v", r.cfg.WorkDir, err)
	}
}

func TestATaskOnARunnerQueueIsStuckAsAWorkOrderProblem(t *testing.T) {
	r := newCheckRig(t, "act-pass.jsonl", 0)
	r.run("Claimed Elk run `run-check-1` (contract v2).\n\n**Direction:** Port the due-date sheet\nrepo: scout\n")
	sub := r.submitted()
	if sub.Arg("status") != "stuck" || sub.Arg("title") != "The work order cannot be run as written" ||
		!strings.Contains(sub.Arg("deliverable"), "without a check packet") {
		t.Errorf("status %q, title %q, deliverable:\n%s", sub.Arg("status"), sub.Arg("title"), sub.Arg("deliverable"))
	}
}

// What Rein declares to Elk for a runner queue: agent:runner from the kind,
// and tool:docker and tool:act only where the probes found them.
func TestARunnerQueueDeclaresAgentRunnerAndItsTools(t *testing.T) {
	a, ok := adapter.Lookup(check.Kind)
	if !ok {
		t.Fatal("no runner adapter registered")
	}
	declared := strings.Join(runner.DeclaredCapabilities(a.Manifest(),
		runner.NewHostCapabilities("a test", "git", "docker", "act")), " ")
	for _, want := range []string{"agent:runner", "tool:docker", "tool:act", "runtime:structured_events"} {
		if !strings.Contains(" "+declared+" ", " "+want+" ") {
			t.Errorf("declared capabilities lack %s: %s", want, declared)
		}
	}
}
