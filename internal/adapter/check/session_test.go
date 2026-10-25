//go:build !windows

package check

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// The stub act below replays a recorded `act --json` log, so the whole path
// from act's output to a suite record and a deliverable runs on every Unix
// machine with no act, no Docker and no network. What it cannot prove is that
// real act still prints this dialect; the fixtures were recorded from act
// 0.2.89, and TestRealActRunsATinyWorkflow runs the real thing when asked.

const floorWorkflow = `name: floor
on: [push, workflow_dispatch]
jobs:
  floor:
    name: Floor check
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`

// testRepo makes a git repository holding a workflow file and returns its
// directory and HEAD.
func testRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".github", "workflows", "floor.yml"), []byte(floorWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "init")
	return dir, strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// stubAct writes an act that answers --list with one job, `floor`, and
// otherwise records its arguments, prints fixture and exits with code after
// sleeping for sleep seconds.
func stubAct(t *testing.T, fixture string, code int, sleep string) (path, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	abs, err := filepath.Abs(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	list, err := filepath.Abs(filepath.Join("testdata", "act-list.txt"))
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" --list \"*) cat '" + list + "'; exit 0;; esac\n" +
		"for a in \"$@\"; do echo \"$a\"; done > '" + argsFile + "'\n" +
		"cat '" + abs + "'\n"
	if sleep != "" {
		script += "sleep " + sleep + "\n"
	}
	script += "exit " + itoa(code) + "\n"
	path = filepath.Join(dir, "act")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, argsFile
}

func itoa(n int) string { return strconv.Itoa(n) }

func stubScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func packetFor(sha string, extra string) string {
	return order("```json\n{\"repo\":\"elk-work/tiny\",\"sha\":\"" + sha + "\",\"ref\":\"refs/heads/main\"," +
		"\"suite\":\"floor\",\"workflow\":\".github/workflows/floor.yml\",\"job\":\"floor\"," +
		"\"runs_on\":[\"ubuntu-latest\"],\"provider\":\"rein\",\"changed_paths\":[\"README.md\"]" + extra + "}\n```")
}

type ran struct {
	res    adapter.Result
	err    error
	events []adapter.Event
	rec    SuiteRecord
	args   []string
}

func runCheck(t *testing.T, a *Adapter, spec adapter.RunSpec, argsFile string) ran {
	t.Helper()
	sess, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var r ran
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range sess.Events() {
			r.events = append(r.events, ev)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the session never closed its events")
	}
	r.res, r.err = sess.Wait()
	b, err := os.ReadFile(filepath.Join(a.RecordRoot, spec.RunID, "suite-record.json"))
	if err != nil {
		t.Fatalf("no suite record: %v", err)
	}
	if err := json.Unmarshal(b, &r.rec); err != nil {
		t.Fatal(err)
	}
	if argsFile != "" {
		if b, err := os.ReadFile(argsFile); err == nil {
			r.args = strings.Split(strings.TrimSpace(string(b)), "\n")
		}
	}
	return r
}

// recordFile is the bytes Rein wrote to the run's record file.
func recordFile(t *testing.T, a *Adapter, runID string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(a.RecordRoot, runID, "suite-record.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func baseSpec(dir, sha string) adapter.RunSpec {
	return adapter.RunSpec{
		RunID: "run-1", AgentKind: Kind, WorktreeDir: dir, Prompt: packetFor(sha, ""),
		PermissionMode: adapter.PermissionFull, Queue: "lab-checks",
		Settings: map[string]string{"executor": "host"},
	}
}

func hasArgs(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		match := true
		for j := range seq {
			if args[i+j] != seq[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestAPassingJobIsAPassRecordAndASucceededSession(t *testing.T) {
	dir, sha := testRepo(t)
	act, argsFile := stubAct(t, "act-pass.jsonl", 0, "")
	a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: "no-such-pulse-binary"}
	r := runCheck(t, a, baseSpec(dir, sha), argsFile)

	if r.res.Status != adapter.StatusSucceeded || r.err != nil {
		t.Fatalf("status %s, err %v", r.res.Status, r.err)
	}
	if r.rec.Result != ResultPass || r.rec.Runner != "lab-checks" || r.rec.SHA != sha || r.rec.Repo != "elk-work/tiny" ||
		r.rec.Suite != "floor" || r.rec.Counts["steps"] != 3 || r.rec.Branch != "" {
		t.Errorf("record: %+v", r.rec)
	}
	for _, want := range [][]string{
		{"workflow_dispatch", "-W", ".github/workflows/floor.yml", "-j", "floor", "--json"},
		{"-P", "ubuntu-latest=-self-hosted"},
	} {
		if !hasArgs(r.args, want...) {
			t.Errorf("act was not run with %v: %v", want, r.args)
		}
	}
	if hasArgs(r.args, "--container-options") || hasArgs(r.args, "-s") {
		t.Errorf("host run got container or secret flags: %v", r.args)
	}
	var progress []string
	for _, ev := range r.events {
		if ev.Kind == adapter.EventProgress {
			progress = append(progress, ev.Text)
		}
	}
	if !strings.Contains(strings.Join(progress, "\n"), "  | line 2") {
		t.Errorf("act's output did not arrive as progress events: %v", progress)
	}
	if last := r.events[len(r.events)-1]; last.Kind != adapter.EventDone {
		t.Errorf("last event is %s, want done", last.Kind)
	}
	for _, want := range []string{"The floor check passed on tiny.", "Result: **pass**", "suite-record.json",
		"pulse is not on PATH", "lines of the log", "Job succeeded"} {
		if !strings.Contains(r.res.Summary, want) {
			t.Errorf("deliverable lacks %q:\n%s", want, r.res.Summary)
		}
	}
	// The deliverable gate (ark:rein#83) must have nothing to object to.
	if n := len([]rune(r.res.Summary)); n < 600 {
		t.Errorf("deliverable is %d characters; the run loop's gate reads under 600 closely", n)
	}
	body, m := recordFromDeliverable(t, r.res.Summary)
	if body != recordFile(t, a, "run-1") {
		t.Errorf("the inline record is not the record file's bytes:\n%s", body)
	}
	if _, ok := m["emitted_by"]; ok || m["result"] != "pass" {
		t.Errorf("inline record: %v; want result pass and no emitted_by, since pulse is not on PATH", m)
	}
}

func TestAFailingJobIsAFailRecordAndStillASucceededSession(t *testing.T) {
	dir, sha := testRepo(t)
	act, argsFile := stubAct(t, "act-fail.jsonl", 1, "")
	a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: "no-such-pulse-binary"}
	r := runCheck(t, a, baseSpec(dir, sha), argsFile)
	if r.res.Status != adapter.StatusSucceeded || r.err != nil {
		t.Fatalf("a failed job is a finished check: status %s, err %v", r.res.Status, r.err)
	}
	if r.rec.Result != ResultFail || r.rec.Counts["steps_failed"] != 1 {
		t.Errorf("record: %+v", r.rec)
	}
	if !strings.Contains(r.res.Summary, `The floor check failed on tiny: the step "the failing step" failed.`) {
		t.Errorf("deliverable:\n%s", r.res.Summary)
	}
	body, m := recordFromDeliverable(t, r.res.Summary)
	if body != recordFile(t, a, "run-1") || m["result"] != "fail" {
		t.Errorf("inline record: %v\n%s", m, body)
	}
	if _, ok := m["emitted_by"]; ok {
		t.Error("emitted_by set on a record Rein did not send")
	}
}

func TestAJobActCouldNotSetUpIsAnErrorAndAFailedSession(t *testing.T) {
	dir, sha := testRepo(t)
	act, argsFile := stubAct(t, "act-setup-failed.jsonl", 1, "")
	a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: "no-such-pulse-binary"}
	r := runCheck(t, a, baseSpec(dir, sha), argsFile)
	if r.res.Status != adapter.StatusFailed || r.err == nil {
		t.Fatalf("a harness fault must fail the session: status %s, err %v", r.res.Status, r.err)
	}
	if r.rec.Result != ResultError {
		t.Errorf("record result %s, want error", r.rec.Result)
	}
	if !strings.Contains(r.res.Summary, "could not be run on this machine") {
		t.Errorf("deliverable:\n%s", r.res.Summary)
	}
	if _, m := recordFromDeliverable(t, r.res.Summary); m["result"] != "error" {
		t.Errorf("inline record result %v, want error", m["result"])
	}
}

func TestACheckPastItsTimeoutIsStopped(t *testing.T) {
	dir, sha := testRepo(t)
	act, argsFile := stubAct(t, "act-pass.jsonl", 0, "30")
	a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: "no-such-pulse-binary",
		InterruptGrace: 200 * time.Millisecond, Heartbeat: 100 * time.Millisecond}
	spec := baseSpec(dir, sha)
	spec.Settings["timeout"] = "1s"
	start := time.Now()
	r := runCheck(t, a, spec, argsFile)
	if time.Since(start) > 15*time.Second {
		t.Fatalf("the timeout did not stop act: %s", time.Since(start))
	}
	if r.res.Status != adapter.StatusTimedOut || r.rec.Result != ResultError {
		t.Errorf("status %s, record %s", r.res.Status, r.rec.Result)
	}
	if _, m := recordFromDeliverable(t, r.res.Summary); m["result"] != "error" {
		t.Errorf("inline record result %v, want error", m["result"])
	}
	beat := false
	for _, ev := range r.events {
		beat = beat || strings.Contains(ev.Text, "act is still running")
	}
	if !beat {
		t.Error("a silent act produced no heartbeat; the run loop would call it stalled")
	}
}

func TestInterruptStopsTheCheck(t *testing.T) {
	dir, sha := testRepo(t)
	act, _ := stubAct(t, "act-pass.jsonl", 0, "30")
	a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: "no-such-pulse-binary", InterruptGrace: 200 * time.Millisecond}
	sess, err := a.Start(context.Background(), baseSpec(dir, sha))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range sess.Events() {
		}
	}()
	time.Sleep(300 * time.Millisecond)
	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := sess.Wait()
	if res.Status != adapter.StatusInterrupted || err == nil {
		t.Fatalf("status %s, err %v", res.Status, err)
	}
	if err := sess.Interrupt(context.Background()); !errors.Is(err, adapter.ErrSessionClosed) {
		t.Errorf("interrupting a finished session: %v", err)
	}
}

func TestTheRecordGoesToPulseWhenTheQueueHoldsStride(t *testing.T) {
	dir, sha := testRepo(t)
	act, argsFile := stubAct(t, "act-pass.jsonl", 0, "")
	pulseArgs := filepath.Join(t.TempDir(), "pulse-args")
	pulse := stubScript(t, "pulse", "echo \"$@\" > '"+pulseArgs+"'\nenv | grep -c STRIDE_ >> '"+pulseArgs+"'")
	a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: pulse}
	spec := baseSpec(dir, sha)
	spec.Env = map[string]string{"STRIDE_INGEST_URL": "https://ingest.example/x", "STRIDE_TOKEN": "stride-secret-token",
		"GITHUB_TOKEN": "ghs_not_on_the_command_line"}
	r := runCheck(t, a, spec, argsFile)
	if !strings.Contains(r.res.Summary, "sent to Elk's suite history") {
		t.Errorf("deliverable:\n%s", r.res.Summary)
	}
	b, err := os.ReadFile(pulseArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "suite-record --file "+filepath.Join(a.RecordRoot, "run-1", "suite-record.json")) ||
		!strings.Contains(string(b), "\n2") {
		t.Errorf("pulse was called as: %q", b)
	}
	if !hasArgs(r.args, "-s", "GITHUB_TOKEN") {
		t.Errorf("act was not told about the queue's GITHUB_TOKEN: %v", r.args)
	}
	for _, arg := range r.args {
		if strings.Contains(arg, "ghs_not_on_the_command_line") || strings.Contains(arg, "stride-secret-token") {
			t.Fatalf("a secret reached act's command line: %v", r.args)
		}
	}
	if strings.Contains(r.res.Summary, "stride-secret-token") || strings.Contains(r.res.Summary, "ghs_") {
		t.Fatal("a secret reached the deliverable")
	}
	body, m := recordFromDeliverable(t, r.res.Summary)
	if m["emitted_by"] != "rein" {
		t.Errorf("emitted_by = %v after a successful send, want rein", m["emitted_by"])
	}
	if withoutEmittedBy(body) != recordFile(t, a, "run-1") {
		t.Errorf("the inline record is not the file's bytes plus emitted_by:\n%s", body)
	}
}

// A send that fails leaves the record for Elk to forward: no emitted_by.
func TestAFailedSendLeavesEmittedByAbsent(t *testing.T) {
	dir, sha := testRepo(t)
	act, argsFile := stubAct(t, "act-pass.jsonl", 0, "")
	pulse := stubScript(t, "pulse", "echo 'ingest refused: 401' >&2\nexit 1")
	a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: pulse}
	spec := baseSpec(dir, sha)
	spec.Env = map[string]string{"STRIDE_INGEST_URL": "https://ingest.example/x", "STRIDE_TOKEN": "stride-secret-token"}
	r := runCheck(t, a, spec, argsFile)
	if r.res.Status != adapter.StatusSucceeded || !strings.Contains(r.res.Summary, "NOT sent") ||
		!strings.Contains(r.res.Summary, "ingest refused") {
		t.Errorf("status %s, deliverable:\n%s", r.res.Status, r.res.Summary)
	}
	body, m := recordFromDeliverable(t, r.res.Summary)
	if _, ok := m["emitted_by"]; ok {
		t.Errorf("emitted_by set although the send failed: %v", m)
	}
	if body != recordFile(t, a, "run-1") {
		t.Errorf("the inline record is not the record file's bytes:\n%s", body)
	}
}

// Pulse on PATH but no Stride variables in the queue's secrets: pulse never
// runs, and the record is Elk's to forward.
func TestPulseWithoutStrideIsNeverRunAndLeavesEmittedByAbsent(t *testing.T) {
	dir, sha := testRepo(t)
	act, argsFile := stubAct(t, "act-pass.jsonl", 0, "")
	ran := filepath.Join(t.TempDir(), "pulse-ran")
	pulse := stubScript(t, "pulse", "touch '"+ran+"'")
	a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: pulse}
	r := runCheck(t, a, baseSpec(dir, sha), argsFile)
	if _, err := os.Stat(ran); err == nil {
		t.Error("pulse ran with no Stride variables")
	}
	if !strings.Contains(r.res.Summary, "hold no STRIDE_INGEST_URL") {
		t.Errorf("deliverable:\n%s", r.res.Summary)
	}
	if _, m := recordFromDeliverable(t, r.res.Summary); m["emitted_by"] != nil {
		t.Errorf("emitted_by set although nothing was sent: %v", m)
	}
}

func TestTheContainerExecutorMapsTheImageAndMountsGit(t *testing.T) {
	dir, sha := testRepo(t)
	act, argsFile := stubAct(t, "act-pass.jsonl", 0, "")
	docker := stubScript(t, "docker", "exit 0")
	a := &Adapter{Act: act, Docker: docker, RecordRoot: t.TempDir(), Pulse: "no-such-pulse-binary"}
	spec := baseSpec(dir, sha)
	spec.Settings = map[string]string{"executor": "container"}
	r := runCheck(t, a, spec, argsFile)
	for _, want := range [][]string{{"-P", "ubuntu-latest=" + DefaultImage}, {"-P", "ubuntu-slim=" + DefaultImage}, {"--pull=false"}} {
		if !hasArgs(r.args, want...) {
			t.Errorf("missing %v in %v", want, r.args)
		}
	}
	mounted := false
	for i, arg := range r.args {
		if arg == "--container-options" && i+1 < len(r.args) && strings.HasSuffix(r.args[i+1], ".git:ro") {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("the repository's git directory was not mounted: %v", r.args)
	}
}

func TestPrepare(t *testing.T) {
	dir, sha := testRepo(t)
	act, _ := stubAct(t, "act-pass.jsonl", 0, "")
	a := &Adapter{Act: act, RecordRoot: t.TempDir()}

	p, err := a.Prepare(context.Background(), baseSpec(dir, sha))
	if err != nil || p.Commit != sha {
		t.Fatalf("Prepare = %+v, %v", p, err)
	}

	spec := baseSpec(dir, sha)
	spec.Prompt = "Claimed Elk run `r` (contract v2).\n\n**Direction:** Port the due-date sheet\nrepo: scout\n"
	if _, err := a.Prepare(context.Background(), spec); !errors.Is(err, adapter.ErrWorkOrder) {
		t.Errorf("a run with no packet: %v, want ErrWorkOrder", err)
	}

	spec = baseSpec(dir, sha)
	spec.Settings = map[string]string{"executor": "container"}
	a.Docker = stubScript(t, "docker", "echo 'Cannot connect to the Docker daemon' >&2; exit 1")
	_, err = a.Prepare(context.Background(), spec)
	if !errors.Is(err, adapter.ErrPreflight) || !strings.Contains(err.Error(), "Docker daemon is not answering") {
		t.Errorf("Docker down: %v", err)
	}

	spec = baseSpec(dir, sha)
	spec.Settings = map[string]string{"executor": "vm"}
	if _, err := a.Prepare(context.Background(), spec); !errors.Is(err, adapter.ErrPreflight) {
		t.Errorf("a bad setting: %v", err)
	}

	a.Act = "rein-test-act-that-is-not-installed"
	if _, err := a.Prepare(context.Background(), baseSpec(dir, sha)); !errors.Is(err, adapter.ErrPreflight) ||
		!errors.Is(err, exec.ErrNotFound) {
		t.Errorf("act missing: %v", err)
	}
}

func TestStartRefusesAWorktreeAtAnotherCommit(t *testing.T) {
	dir, _ := testRepo(t)
	act, _ := stubAct(t, "act-pass.jsonl", 0, "")
	a := &Adapter{Act: act, RecordRoot: t.TempDir()}
	other := strings.Repeat("a", 40)
	if _, err := a.Start(context.Background(), baseSpec(dir, other)); err == nil || !strings.Contains(err.Error(), "not the check's commit") {
		t.Fatalf("Start at the wrong commit: %v", err)
	}
}

func TestManifestIsValidAndDeclaresNoAgentPowers(t *testing.T) {
	m := New().Manifest()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if m.Kind != "runner" || !m.Has(adapter.CapStructuredEvents) || m.Has(adapter.CapFileEdit) || m.Has(adapter.CapResume) {
		t.Errorf("manifest: %+v", m)
	}
	if _, ok := adapter.Lookup("runner"); !ok {
		t.Error("the check adapter did not register under runner")
	}
}

// A workflow step can print its environment, and on the host executor that
// environment is act's own. So Stride's variables never reach act at all, a
// GitHub token reaches it only from the queue's secrets, and every secret
// value is taken out of the log, the progress events and the deliverable.
func TestSecretsStayOutOfTheJobAndOutOfEverythingItPrints(t *testing.T) {
	dir, sha := testRepo(t)
	envFile := filepath.Join(t.TempDir(), "env")
	list, _ := filepath.Abs(filepath.Join("testdata", "act-list.txt"))
	pass, _ := filepath.Abs(filepath.Join("testdata", "act-pass.jsonl"))
	act := stubScript(t, "act", "case \" $* \" in *\" --list \"*) cat '"+list+"'; exit 0;; esac\n"+
		"env | grep -c '^STRIDE_' > '"+envFile+"'\n"+
		"env | grep -c '^GITHUB_TOKEN=' >> '"+envFile+"'\n"+
		`echo '{"msg":"token is ghs_not_on_the_command_line and stride-secret-token","raw_output":true,"stage":"Main","step":"leak"}'`+"\n"+
		"cat '"+pass+"'\n")
	a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: "no-such-pulse-binary"}
	spec := baseSpec(dir, sha)
	spec.Env = map[string]string{"STRIDE_INGEST_URL": "https://ingest.example/x", "STRIDE_TOKEN": "stride-secret-token",
		"GITHUB_TOKEN": "ghs_not_on_the_command_line"}
	r := runCheck(t, a, spec, "")

	b, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(b)); len(got) != 2 || got[0] != "0" || got[1] != "1" {
		t.Errorf("act's environment held %v (STRIDE_ variables, GITHUB_TOKEN); want 0 and 1, the token from the queue's secrets", got)
	}
	log, err := os.ReadFile(filepath.Join(a.RecordRoot, "run-1", "act.log"))
	if err != nil {
		t.Fatal(err)
	}
	everything := string(log) + r.res.Summary
	for _, ev := range r.events {
		everything += ev.Text
	}
	for _, secret := range []string{"ghs_not_on_the_command_line", "stride-secret-token"} {
		if strings.Contains(everything, secret) {
			t.Fatalf("%s reached the log, an event or the deliverable", secret)
		}
	}
	if !strings.Contains(string(log), "token is ") {
		t.Errorf("the redacted line itself is missing from the log:\n%s", log)
	}
}

// packetWithSuites is packetFor with check.suites; check.suite stays floor.
func packetWithSuites(sha string, suites ...string) string {
	b, _ := json.Marshal(suites)
	return packetFor(sha, ",\"suites\":"+string(b))
}

// countingAct wraps act so each run of a job, not a --list, adds a line to
// the returned file.
func countingAct(t *testing.T, act string) (path, count string) {
	t.Helper()
	count = filepath.Join(t.TempDir(), "count")
	path = stubScript(t, "act", "case \" $* \" in *\" --list \"*) ;; *) echo ran >> '"+count+"';; esac\nexec '"+act+"' \"$@\"")
	return path, count
}

func recordFileAt(t *testing.T, a *Adapter, i int) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(a.RecordRoot, "run-1", recordFileName(i)))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestASharedRunRunsTheJobOnceAndRecordsEverySuite(t *testing.T) {
	dir, sha := testRepo(t)
	inner, argsFile := stubAct(t, "act-pass.jsonl", 0, "")
	act, count := countingAct(t, inner)
	a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: "no-such-pulse-binary"}
	spec := baseSpec(dir, sha)
	spec.Prompt = packetWithSuites(sha, "floor", "lint", "docs")
	r := runCheck(t, a, spec, argsFile)
	if r.res.Status != adapter.StatusSucceeded || r.err != nil {
		t.Fatalf("status %s, err %v", r.res.Status, r.err)
	}
	if b, _ := os.ReadFile(count); strings.Count(string(b), "ran") != 1 {
		t.Errorf("act ran the job %d times, want once", strings.Count(string(b), "ran"))
	}

	suites := []string{"floor", "lint", "docs"}
	body, ms := recordsFromDeliverable(t, r.res.Summary)
	if len(ms) != len(suites) {
		t.Fatalf("%d records in the block, want %d:\n%s", len(ms), len(suites), body)
	}
	first := recordFileAt(t, a, 0)
	for i, suite := range suites {
		file := recordFileAt(t, a, i)
		if file["suite"] != suite || ms[i]["suite"] != suite {
			t.Errorf("record %d: file suite %v, block suite %v, want %s", i, file["suite"], ms[i]["suite"], suite)
		}
		if !reflect.DeepEqual(ms[i], file) {
			t.Errorf("record %d in the block is not its file:\n%v\n%v", i, ms[i], file)
		}
		file["suite"] = first["suite"]
		if !reflect.DeepEqual(file, first) {
			t.Errorf("record %d differs from the first in more than its suite:\n%v\n%v", i, file, first)
		}
		if _, ok := ms[i]["emitted_by"]; ok {
			t.Errorf("record %d: emitted_by set although pulse is not on PATH", i)
		}
	}
	if first["result"] != "pass" || first["sha"] != sha || first["runner"] != "lab-checks" {
		t.Errorf("record: %v", first)
	}
	if _, err := os.Stat(filepath.Join(a.RecordRoot, "run-1", recordFileName(3))); err == nil {
		t.Error("a fourth record file was written")
	}
	for _, want := range []string{"The floor, lint and docs checks passed on tiny.", "- Suites: floor, lint, docs",
		"  - lint: `" + filepath.Join(a.RecordRoot, "run-1", "suite-record-2.json") + "` — kept on this machine: pulse is not on PATH"} {
		if !strings.Contains(r.res.Summary, want) {
			t.Errorf("deliverable lacks %q:\n%s", want, r.res.Summary)
		}
	}
}

func TestOneSuiteInSuitesKeepsTheObjectBlock(t *testing.T) {
	for _, suites := range [][]string{{"floor"}, {"floor", "floor"}, {}} {
		dir, sha := testRepo(t)
		act, argsFile := stubAct(t, "act-pass.jsonl", 0, "")
		a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: "no-such-pulse-binary"}
		spec := baseSpec(dir, sha)
		spec.Prompt = packetWithSuites(sha, suites...)
		r := runCheck(t, a, spec, argsFile)
		body, m := recordFromDeliverable(t, r.res.Summary)
		if body != recordFile(t, a, "run-1") || m["suite"] != "floor" {
			t.Errorf("suites %v: the block is not the one record file:\n%s", suites, body)
		}
		if _, err := os.Stat(filepath.Join(a.RecordRoot, "run-1", recordFileName(1))); err == nil {
			t.Errorf("suites %v: a second record file was written", suites)
		}
		if !strings.Contains(r.res.Summary, "The floor check passed on tiny.") || !strings.Contains(r.res.Summary, "- Suite: floor ·") {
			t.Errorf("suites %v: deliverable:\n%s", suites, r.res.Summary)
		}
	}
}

// Each record is sent on its own, and emitted_by marks exactly the ones
// whose send succeeded: Elk forwards the rest.
func TestEmittedByIsSetPerRecord(t *testing.T) {
	dir, sha := testRepo(t)
	act, argsFile := stubAct(t, "act-pass.jsonl", 0, "")
	pulseArgs := filepath.Join(t.TempDir(), "pulse-args")
	pulse := stubScript(t, "pulse", "echo \"$@\" >> '"+pulseArgs+"'\n"+
		"case \"$*\" in *suite-record-2.json*) echo 'ingest refused: 503' >&2; exit 1;; esac")
	a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: pulse}
	spec := baseSpec(dir, sha)
	spec.Prompt = packetWithSuites(sha, "floor", "lint", "docs")
	spec.Env = map[string]string{"STRIDE_INGEST_URL": "https://ingest.example/x", "STRIDE_TOKEN": "stride-secret-token"}
	r := runCheck(t, a, spec, argsFile)
	if r.res.Status != adapter.StatusSucceeded {
		t.Fatalf("a failed send must not fail the check: status %s", r.res.Status)
	}
	b, err := os.ReadFile(pulseArgs)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(calls) != 3 {
		t.Fatalf("pulse ran %d times, want once per record: %q", len(calls), calls)
	}
	for i, call := range calls {
		if want := "suite-record --file " + filepath.Join(a.RecordRoot, "run-1", recordFileName(i)); call != want {
			t.Errorf("pulse call %d = %q, want %q", i, call, want)
		}
	}
	_, ms := recordsFromDeliverable(t, r.res.Summary)
	for i, want := range []bool{true, false, true} {
		v, ok := ms[i]["emitted_by"]
		if ok != want || (ok && v != "rein") {
			t.Errorf("record %d (%v): emitted_by = %v, present %v; want present %v", i, ms[i]["suite"], v, ok, want)
		}
	}
	if !strings.Contains(r.res.Summary, "  - lint: `"+filepath.Join(a.RecordRoot, "run-1", "suite-record-2.json")+"` — NOT sent") ||
		!strings.Contains(r.res.Summary, "  - docs: `"+filepath.Join(a.RecordRoot, "run-1", "suite-record-3.json")+"` — sent to Elk's suite history") {
		t.Errorf("deliverable does not say what happened to each record:\n%s", r.res.Summary)
	}
	if strings.Contains(r.res.Summary, "stride-secret-token") {
		t.Fatal("a secret reached the deliverable")
	}
}
