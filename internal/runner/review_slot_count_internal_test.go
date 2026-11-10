package runner

import (
	"strings"
	"testing"
)

// The other half of the rule in review_slot_count_test.go, at the counter
// itself: a run that is working counts against the machine's slot, and the
// gate names it as the holder, while the release that parks a run for Elk's
// review takes it out of the count.
//
// This is asserted on the gate rather than end to end because the serve loop
// is inside the session for as long as a run works, and the telemetry tick
// has a ten-second floor ([MinTelemetryInterval]) — there is no heartbeat to
// read in a test that finishes in under a second. ark:scout#4728.
func TestTheSlotCountHoldsAWorkingRunAndDropsOneGivenBackForReview(t *testing.T) {
	g := oneSlot()
	reading := Headroom{}

	if n := g.inFlight(); n != 0 {
		t.Fatalf("an idle gate counts %d runs, want 0", n)
	}

	// A run that is working holds the slot and is counted.
	h := take(t, g, slotRequest{queue: "mac-claude", prio: prioBuild})
	h.setRun("run-a")
	if n := g.inFlight(); n != 1 {
		t.Errorf("a working run counts %d, want 1", n)
	}
	if total, used, _ := g.capacity(reading); total != 1 || used != 1 {
		t.Errorf("capacity reports %d of %d used while a run works, want 1 of 1", used, total)
	}

	// So the next queue is refused, and told which run has it.
	w := g.join(slotRequest{queue: "mac-codex", prio: prioBuild})
	got, why := g.tryEnter(w)
	if got != nil {
		t.Error("a second queue was given the only slot while a run was working")
	}
	if !strings.Contains(why, "in use by mac-claude run run-a") {
		t.Errorf("the refusal does not name the run holding the slot: %q", why)
	}
	g.quit(w)

	// The submit that parks run-a for Elk's review gives the slot back. The
	// run is still Rein's — its lease, worktree and continuation live on — but
	// it stops counting, because nothing is running.
	h.release()
	if n := g.inFlight(); n != 0 {
		t.Errorf("a run held for review counts %d, want 0", n)
	}
	if _, used, _ := g.capacity(reading); used != 0 {
		t.Errorf("capacity reports %d used while the only run is held for review, want 0", used)
	}

	// And the slot really is available again — to another queue, and to the
	// held run itself when Elk asks for revisions.
	next := take(t, g, slotRequest{queue: "mac-codex", prio: prioBuild})
	next.release()
	back := take(t, g, slotRequest{queue: "mac-claude", prio: prioRevision})
	if n := g.inFlight(); n != 1 {
		t.Errorf("a revision that retook the slot counts %d, want 1", n)
	}
	back.release()

	// Releasing twice is safe: drive gives the slot back at the submit and
	// claimAndDrive's deferred release runs after it.
	back.release()
	if n := g.inFlight(); n != 0 {
		t.Errorf("after everything was released the gate counts %d, want 0", n)
	}
}
