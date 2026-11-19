package runner_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	for _, want := range []string{"not claiming", path, "Full Disk Access", "Rein service", "stay queued"} {
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

// A queue whose other repository is readable still claims; a run that then
// names the unreadable one stops before the worktree, in the same words, with
// the stop code Elk restarts on another ready queue.
func TestARunNamingAnUnreadableRepositoryStopsBeforeTheAgent(t *testing.T) {
	h := newHarness(t)
	readable := filepath.Join(filepath.Dir(h.cfg.Repos["scout"]), "pulse")
	if err := os.MkdirAll(readable, 0o755); err != nil {
		t.Fatal(err)
	}
	h.cfg.Repos["pulse"] = readable
	unreadableDir(t, h.cfg.Repos["scout"])
	h.elk.Text("claim_run", order("run-1"))

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if n := len(h.agent.Specs()); n != 0 {
		t.Errorf("the agent started %d times", n)
	}
	if created, _ := h.wts.counts(); created != 0 {
		t.Errorf("%d worktrees created", created)
	}
	sub := h.submitted()
	if sub.Arg("status") != "stuck" || sub.Arg("stop_code") != "worktree_init" {
		t.Errorf("status %q, stop_code %q", sub.Arg("status"), sub.Arg("stop_code"))
	}
	d := sub.Arg("deliverable")
	for _, want := range []string{h.cfg.Repos["scout"], "Full Disk Access", "Nothing was started"} {
		if !strings.Contains(d, want) {
			t.Errorf("deliverable missing %q:\n%s", want, d)
		}
	}
}
