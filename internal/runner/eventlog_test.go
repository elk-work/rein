package runner_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/runner"
)

func openRunLogs(t *testing.T) *runlog.Store {
	t.Helper()
	s, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// One run, driven all the way, leaves a log a person can read afterwards: the
// runner's own lifecycle, every event the agent emitted, and a meta that says
// where the work happened.
func TestDriveWritesTheRunLog(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	logs := openRunLogs(t)

	if err := h.run(runner.Options{Logs: logs, LogKeepRuns: 50}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	meta, err := logs.ReadMeta("run-1")
	if err != nil {
		t.Fatalf("no meta for the run: %v", err)
	}
	if meta.Queue != queue || meta.AgentKind != kind || meta.Workspace != space {
		t.Errorf("meta = %+v", meta)
	}
	if meta.Direction != "Port the due-date sheet to Windows" {
		t.Errorf("direction = %q", meta.Direction)
	}
	if meta.Status != runlog.StatusSubmitted {
		t.Errorf("status = %q, want submitted", meta.Status)
	}
	if meta.Branch == "" || meta.Worktree == "" || meta.Repo == "" {
		// The branch is the durable one: the worktree is gone by the time
		// anybody reads this and the branch is not.
		t.Errorf("the worktree facts did not reach the meta: %+v", meta)
	}
	if meta.SessionID == "" {
		t.Errorf("no vendor session id in the meta: %+v", meta)
	}
	if meta.PermissionMode != "full" {
		t.Errorf("permission mode = %q", meta.PermissionMode)
	}
	if meta.EndedAt.IsZero() {
		t.Error("a finished run has no end time, so a live view would show it forever")
	}

	recs, err := logs.Records("run-1")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, r := range recs {
		kinds[string(r.Source)+":"+r.Kind]++
	}
	for _, want := range []string{
		"runner:" + string(runlog.KindClaimed),
		"runner:" + string(runlog.KindWorktree),
		"runner:" + string(runlog.KindSession),
		"runner:" + string(runlog.KindReport),
		"runner:" + string(runlog.KindSubmit),
		"runner:" + string(runlog.KindReaped),
		"agent:" + string(adapter.EventText),
		"agent:" + string(adapter.EventUsage),
		"agent:" + string(adapter.EventDone),
	} {
		if kinds[want] == 0 {
			t.Errorf("no %s record. Got: %v", want, kinds)
		}
	}

	// The whole event stream, not the throttled sample Elk sees. The fake
	// agent's script says "working on it" and the run's deliverable never
	// carries it verbatim, so finding it here is the point of the log.
	var texts []string
	for _, r := range recs {
		if r.Source == runlog.SourceAgent && r.Kind == string(adapter.EventText) {
			texts = append(texts, r.Text)
		}
	}
	if strings.Join(texts, " ") != "working on it" {
		t.Errorf("assistant text in the log = %q", texts)
	}
	if in, out, model := runlog.Totals(recs); in != 1000 || out != 250 || model != "fake-1" {
		t.Errorf("usage in the log = %d/%d/%q", in, out, model)
	}
}

// A run that cannot be served is refused before anything exists on disk — but
// it still gets a log, because "why did this go stuck" is exactly the question
// the log is for.
func TestStuckRunStillLeavesALog(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1", "xcode"))
	logs := openRunLogs(t)

	if err := h.run(runner.Options{Logs: logs}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	meta, err := logs.ReadMeta("run-1")
	if err != nil {
		t.Fatalf("no meta for a stuck run: %v", err)
	}
	if meta.Status != runlog.StatusStuck {
		t.Errorf("status = %q, want stuck", meta.Status)
	}
	if meta.Worktree != "" {
		t.Errorf("a refused run recorded a worktree: %q", meta.Worktree)
	}
	recs, err := logs.Records("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("a stuck run wrote no records at all")
	}
	if recs[0].Kind != string(runlog.KindClaimed) {
		t.Errorf("the first record is %q, want claimed", recs[0].Kind)
	}
}

// An agent that will not start is the stuck run where the local record matters
// most, because nothing else ran. Until ark:rein#38 the reason reached only
// Elk; rein.log and the run's event log said "The agent would not start" and
// nothing about why, which is how a slow Codex start-up passed for a broken
// protocol.
func TestAStartFailureSaysWhyLocally(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.StartErr = errors.New("codex: `codex app-server` did not answer initialize within the 1m30s startup timeout")
	logs := openRunLogs(t)

	if err := h.run(runner.Options{Logs: logs}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	const why = "did not answer initialize"

	sub := h.submitted()
	if sub.Arg("status") != "stuck" || !strings.Contains(sub.Arg("deliverable"), why) {
		t.Errorf("Elk got status %q, deliverable:\n%s", sub.Arg("status"), sub.Arg("deliverable"))
	}
	if !strings.Contains(h.log.String(), why) {
		t.Errorf("rein.log does not say why the agent would not start:\n%s", h.log)
	}
	recs, err := logs.Records("run-1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range recs {
		if r.Source == runlog.SourceRunner && strings.Contains(r.Text, why) {
			found = true
		}
	}
	if !found {
		t.Errorf("the run's event log does not say why the agent would not start: %+v", recs)
	}
}

// A nil store is the same code path. Nothing in the loop checks for one, so
// this is what proves it does not have to.
func TestDriveWithoutALogStore(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if sub := h.submitted(); sub.Arg("status") != "ready" {
		t.Errorf("status = %q", sub.Arg("status"))
	}
}

// The logs are pruned where the worktree reaper runs — at the end of a run —
// because that is the only moment anything is going to remember to.
func TestPruningRunsWithTheReaper(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	logs := openRunLogs(t)
	for _, id := range []string{"old-1", "old-2", "old-3"} {
		logs.Create(runlog.Meta{RunID: id}).Close(runlog.StatusSubmitted)
	}

	if err := h.run(runner.Options{Logs: logs, LogKeepRuns: 2}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	left, err := logs.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		var ids []string
		for _, m := range left {
			ids = append(ids, m.RunID)
		}
		t.Fatalf("logs left = %v, want 2", ids)
	}
	// The run that just finished is the newest, so it is one of the two.
	if left[0].RunID != "run-1" {
		t.Errorf("the newest log is %q, want run-1", left[0].RunID)
	}
}

func TestUnknownCapabilityIsAdvisoryOnTheTimeline(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1", "invented credential name"))
	logs := openRunLogs(t)
	if err := h.run(runner.Options{Logs: logs}); err != nil {
		t.Fatal(err)
	}
	if len(h.agent.Specs()) != 1 || h.submitted().Arg("status") != "ready" {
		t.Fatal("advisory prevented run")
	}
	if prompt := h.agent.Specs()[0].SystemPrompt; !strings.Contains(prompt, "unknown packet requirements as advisory: invented credential name") {
		t.Fatal("agent was not told the preflight advisory decision")
	}
	records, err := logs.Records("run-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if rec.Kind == string(runlog.KindNote) && strings.Contains(rec.Text, "invented credential name") {
			return
		}
	}
	t.Fatal("advisory absent from timeline")
}
