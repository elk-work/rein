package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// ark:rein#172: a laptop whose checkout sat in ~/Documents claimed a recurring
// walk twice and stopped both at once — macOS keeps the Rein service out of
// that folder without Full Disk Access. A queue that cannot read its
// repository claims nothing, so the run stays queued for another agent.

// unreadableDir makes a directory this process may not list, the way macOS's
// privacy protection makes ~/Documents look to a service without Full Disk
// Access: listing it fails with a permission error.
func unreadableDir(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads any folder; the refusal cannot be staged")
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o755) })
	if _, err := os.ReadDir(path); err == nil {
		t.Skip("this filesystem lets a mode-0 folder be listed")
	}
}

// fixWords is what the refusal must say to be acted on: on a Mac, that the
// folder needs Full Disk Access for the Rein service.
func fixWords() []string {
	if runtime.GOOS == "darwin" {
		return []string{"Full Disk Access", "Rein service"}
	}
	return []string{"read access"}
}

func TestAQueueThatCannotReadItsRepositoryClaimsNothing(t *testing.T) {
	h := newHarness(t)
	path := h.cfg.Repos["scout"]
	unreadableDir(t, path)
	h.elk.Text("claim_run", order("run-1"))

	if err := h.runLoop(runner.Options{PollInterval: 10 * time.Millisecond}, 300*time.Millisecond); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.elk.CallsTo("claim_run")); n != 0 {
		t.Fatalf("claim_run called %d times; an unreadable repository must leave the run queued", n)
	}
	if n := len(h.elk.CallsTo("submit_deliverable")); n != 0 {
		t.Errorf("submitted %d times; nothing was claimed, so nothing may end stuck", n)
	}
	if n := len(h.agent.Specs()); n != 0 {
		t.Errorf("the agent started %d times", n)
	}
	beats := len(h.elk.CallsTo("heartbeat_executor"))
	if beats < 3 {
		t.Fatalf("only %d beats in the window; the test proves nothing about repetition", beats)
	}
	got := lastWaitingOn(h)
	for _, want := range append([]string{"not claiming", path, "stay queued"}, fixWords()...) {
		if !strings.Contains(got, want) {
			t.Errorf("waiting_on = %q, missing %q", got, want)
		}
	}
	if n := strings.Count(h.log.String(), "Rein cannot read the repository folder"); n != 1 {
		t.Errorf("the refusal was logged %d times over %d polls, want once:\n%s", n, beats, h.log)
	}
}

func TestAQueueThatCanReadItsRepositoryClaimsAsBefore(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(h.cfg.Repos["scout"], 0o755); err != nil {
		t.Fatal(err)
	}
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.elk.CallsTo("claim_run")); n != 1 {
		t.Fatalf("claim_run called %d times, want 1", n)
	}
	if n := len(h.agent.Specs()); n != 1 {
		t.Errorf("the agent started %d times, want 1", n)
	}
	if strings.Contains(h.log.String(), "not claiming") {
		t.Errorf("a readable repository was refused:\n%s", h.log)
	}
}

// orderIn is a work order naming repository name.
func orderIn(runID, name string) string {
	return strings.Replace(order(runID), "repo: scout\n", "repo: "+name+"\n", 1)
}

// runScoped lists the Elk calls, other than claim_run, that named runID. Any
// of them would start a run Elk only holds (scout 0790).
func runScoped(h *harness, runID string) []string {
	var out []string
	for _, c := range h.elk.Calls() {
		if c.Tool != "claim_run" && c.Arg("run_id") == runID {
			out = append(out, c.Tool)
		}
	}
	return out
}

// A queue whose folder is readable still claims. A run that names an
// unreadable repository is left unstarted: nothing names it, so Elk's hold
// lapses and it stays queued for another agent, and the queue goes on to its
// next run straight away.
func TestARunNamingAnUnreadableRepositoryIsLeftQueued(t *testing.T) {
	h := newHarness(t)
	readable := filepath.Join(filepath.Dir(h.cfg.Repos["scout"]), "pulse")
	if err := os.MkdirAll(readable, 0o755); err != nil {
		t.Fatal(err)
	}
	h.cfg.Repos["pulse"] = readable
	unreadableDir(t, h.cfg.Repos["scout"])
	var mu sync.Mutex
	claims := 0
	h.elk.Handle("claim_run", func(map[string]any) elktest.Reply {
		mu.Lock()
		defer mu.Unlock()
		claims++
		switch claims {
		case 1:
			return elktest.Reply{Text: order("run-1")}
		case 2:
			return elktest.Reply{Text: orderIn("run-2", "pulse")}
		}
		return elktest.Reply{Text: cleared(queue)}
	})

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if got := runScoped(h, "run-1"); len(got) != 0 {
		t.Errorf("the declined run was named in %v; that starts it rather than leaving it queued", got)
	}
	// The guard must skip only the unreadable run: the next readable run
	// still acknowledges receipt before starting its setup.
	reports := h.elk.CallsTo("report_progress")
	if len(reports) == 0 || reports[0].Arg("run_id") != "run-2" ||
		!strings.Contains(reports[0].Arg("body"), "Rein received the work order") {
		t.Fatal("the readable run was not acknowledged first")
	}
	for _, c := range h.elk.CallsTo("submit_deliverable") {
		if c.Arg("run_id") == "run-1" {
			t.Errorf("the declined run was submitted %q", c.Arg("status"))
		}
	}
	specs := h.agent.Specs()
	if len(specs) != 1 || specs[0].RunID != "run-2" {
		t.Fatalf("agent runs = %d, want only run-2 (the readable repository)\nlog:\n%s", len(specs), h.log)
	}
	if !strings.Contains(h.log.String(), "left run run-1 queued") {
		t.Errorf("the log does not say the run was left queued:\n%s", h.log)
	}
}

// Every run the queue is handed names the unreadable folder: none starts, the
// beat says why, and a restart's take-back of held runs does not grab the
// declined one again, which would keep restarting its hold.
func TestDeclinedRunsAreNotTakenBack(t *testing.T) {
	h := newHarness(t)
	readable := filepath.Join(filepath.Dir(h.cfg.Repos["scout"]), "pulse")
	if err := os.MkdirAll(readable, 0o755); err != nil {
		t.Fatal(err)
	}
	h.cfg.Repos["pulse"] = readable
	unreadableDir(t, h.cfg.Repos["scout"])
	var mu sync.Mutex
	handed := false
	h.elk.Handle("claim_run", func(args map[string]any) elktest.Reply {
		mu.Lock()
		defer mu.Unlock()
		if id, _ := args["run_id"].(string); id != "" {
			t.Errorf("claim_run named %s: a declined run was taken back", id)
			return elktest.Reply{Text: order(id)}
		}
		if !handed {
			handed = true
			return elktest.Reply{Text: order("run-1")}
		}
		// Elk lists the run as held by this caller while the hold is live.
		return elktest.Reply{Text: cleared(queue) + "\n\nYou hold 1 run on a live lease: `run-1`. " +
			"If a claim_run reply never reached you, call claim_run with that run_id to read its work order again."}
	})

	if err := h.runLoop(runner.Options{PollInterval: 10 * time.Millisecond}, 300*time.Millisecond); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if got := runScoped(h, "run-1"); len(got) != 0 {
		t.Errorf("the declined run was named in %v", got)
	}
	if n := len(h.agent.Specs()); n != 0 {
		t.Errorf("the agent started %d times", n)
	}
	if n := len(h.elk.CallsTo("submit_deliverable")); n != 0 {
		t.Errorf("submitted %d times; a declined run must stay queued, not stuck", n)
	}
	got := lastWaitingOn(h)
	for _, want := range append([]string{"not starting a run", h.cfg.Repos["scout"], "left queued"}, fixWords()...) {
		if !strings.Contains(got, want) {
			t.Errorf("waiting_on = %q, missing %q", got, want)
		}
	}
	if n := strings.Count(h.log.String(), "left run run-1 queued"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, h.log)
	}
}

// Elk's router reads the repo: capabilities a queue declares. A folder this
// process may not read is not declared, so the router's next choice for a run
// left queued is a machine that can read it.
func TestAnUnreadableCheckoutIsNotOfferedToElk(t *testing.T) {
	root := t.TempDir()
	readable, blocked := filepath.Join(root, "rein"), filepath.Join(root, "scout")
	if err := os.MkdirAll(readable, 0o755); err != nil {
		t.Fatal(err)
	}
	unreadableDir(t, blocked)
	got := runner.RepoCapabilities(context.Background(),
		map[string]string{"elk-work/rein": readable, "elk-work/scout": blocked})
	if len(got) != 1 || got[0] != "repo:elk-work/rein" {
		t.Errorf("declared %v, want only repo:elk-work/rein", got)
	}
}
