package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/control"
	"github.com/elk-work/rein/internal/runner"
)

// The NOW section of `rein status` keeps three things apart that Elk's one
// "running" covers: an agent at work, a run parked with Elk's review, and a
// request in line for a slot. And it names the queue's own limit beside the
// machine's.
func TestStatusNowSeparatesSessionsReviewsAndSlotWaits(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var out strings.Builder
	renderNow(&out, control.Status{
		SlotsCeiling: 4, SlotsNow: 2, SlotsUsed: 2,
		Queues: []control.QueueStatus{{
			Queue: "mac-claude", Parallel: 3, Driving: 2,
			Runs: []control.RunActivity{
				{RunID: "arun-review", State: runner.ActivityReview, Since: now.Add(-14 * time.Minute)},
				{RunID: "arun-one", State: runner.ActivitySession, Since: now.Add(-9 * time.Minute)},
				{RunID: "arun-two", State: runner.ActivitySession, Since: now.Add(-2 * time.Minute)},
				{RunID: "arun-back", State: runner.ActivityRevisionWait, Since: now.Add(-40 * time.Second), Detail: "2 slots (the cap)"},
			},
			ClaimWaiting:      "2 slots (the cap); in use by mac-claude run arun-one for 9m",
			ClaimWaitingSince: now.Add(-30 * time.Second),
		}, {
			Queue: "mac-codex", Parallel: 1,
			NotClaiming: "not claiming: agent authentication required",
		}},
	}, now)
	got := out.String()
	for _, want := range []string{
		"machine   2 of its slots in use; the machine can afford 2 now, ceiling 4",
		"mac-claude   2 agent sessions, 1 run with Elk's review, 2 requests waiting for a slot; this queue may drive 3 runs at once (parallel = 3)",
		"arun-review", "with Elk's review, no slot held", "for 14m",
		"arun-one", "agent session", "for 9m",
		"waiting for a slot to work Elk's revisions: 2 slots (the cap)",
		"next claim", "waiting for a slot: 2 slots (the cap); in use by mac-claude run arun-one for 9m",
		"mac-codex   no agent session, no run with Elk's review, no request waiting for a slot; this queue may drive 1 run at once (parallel = 1)",
		"not claiming: agent authentication required",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("NOW is missing %q:\n%s", want, got)
		}
	}
}
