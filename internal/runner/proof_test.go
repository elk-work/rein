package runner_test

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/fake"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// Elk refuses a finish with no screenshot and no Tests section (scout 0647).
// Rein passes Elk's words to the session once; still none, it says so.

const proofRefusal = "Not saved: Before you finish: if any screen changed, capture a screenshot of each " +
	"changed screen and attach it to the task with send_message."

// fullSummary passes the deliverable gate (summary_test.go).
var fullSummary = oct14Summary

// refuseFirst refuses `n` submits with the proof refusal, then saves.
func refuseFirst(h *harness, n int32) {
	var seen atomic.Int32
	h.elk.Handle("submit_deliverable", func(map[string]any) elktest.Reply {
		if seen.Add(1) <= n {
			return elktest.Reply{Text: proofRefusal, IsError: true}
		}
		return elktest.Reply{Text: "Deliverable saved: run `run-1` is `ready`. The user will review it in Elk."}
	})
}

func TestAProofRefusalGoesBackToTheSessionOnce(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	refuseFirst(h, 1)
	tests := "## Tests\n- go test ./...: passed"
	h.agent.Rounds = []fake.Round{
		{Script: []adapter.Event{{Kind: adapter.EventText, Text: fullSummary}}, Result: ended(fullSummary)},
		// Asked for proof, it adds only the section: too short to stand alone.
		{Script: []adapter.Event{{Kind: adapter.EventText, Text: tests}}, Result: ended(tests)},
	}

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	specs := h.agent.Specs()
	if len(specs) != 2 || specs[1].ResumeID == "" {
		t.Fatalf("want one resumed ask after the refusal, got %d sessions\nlog:\n%s", len(specs), h.log)
	}
	if !strings.Contains(specs[1].Prompt, "Before you finish") {
		t.Errorf("the ask does not carry Elk's words:\n%s", specs[1].Prompt)
	}
	subs := h.elk.CallsTo("submit_deliverable")
	if len(subs) != 2 {
		t.Fatalf("submits = %d, want 2\nlog:\n%s", len(subs), h.log)
	}
	last := subs[1]
	if d := last.Arg("deliverable"); !strings.Contains(d, fullSummary) || !strings.Contains(d, "## Tests") {
		t.Errorf("the second submit lost the summary or the tests:\n%s", d)
	}
	if last.Arg("no_proof") != "" {
		t.Errorf("no_proof sent although the agent added tests: %q", last.Arg("no_proof"))
	}
}

func TestStillNoProofSubmitsSayingSo(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	refuseFirst(h, 2)
	h.agent.Rounds = []fake.Round{
		{Script: []adapter.Event{{Kind: adapter.EventText, Text: fullSummary}}, Result: ended(fullSummary)},
		{Script: nil, Result: ended("")},
	}

	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}

	subs := h.elk.CallsTo("submit_deliverable")
	if len(subs) != 3 {
		t.Fatalf("submits = %d, want 3 (refused, refused, saved)\nlog:\n%s", len(subs), h.log)
	}
	if got := subs[2].Arg("no_proof"); got != "The agent attached no screenshot or tests" {
		t.Errorf("no_proof = %q", got)
	}
	if !strings.Contains(subs[2].Arg("deliverable"), fullSummary) {
		t.Errorf("the deliverable was lost:\n%s", subs[2].Arg("deliverable"))
	}
	if n := len(h.agent.Specs()); n != 2 {
		t.Errorf("the agent was asked %d times, want once", n-1)
	}
}
