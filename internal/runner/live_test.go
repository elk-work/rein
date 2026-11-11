package runner_test

import (
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)


// reportBodies is every report_progress body, in order.
func (h *harness) reportBodies() []string {
	var out []string
	for _, c := range h.elk.CallsTo("report_progress") {
		out = append(out, c.Arg("body"))
	}
	return out
}

// says is every say body, in order.
func (h *harness) says() []string {
	var out []string
	for _, c := range h.elk.CallsTo("report_progress") {
		if c.Arg("kind") == "say" {
			out = append(out, c.Arg("body"))
		}
	}
	return out
}

// Outside a reply window the agent's text rides with the next receipt: sent
// as a say just before it.
func TestTextIsSentWithTheNextReceipt(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.Script = []adapter.Event{
		{Kind: adapter.EventText, Text: "The sheet has no picker yet."},
		{Kind: adapter.EventToolUse, Tool: &adapter.ToolUse{Name: "Read"}},
		{Kind: adapter.EventText, Text: "Not sent: no receipt follows and nobody asked."},
	}
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if got := h.says(); len(got) != 1 || got[0] != "The sheet has no picker yet." {
		t.Fatalf("says = %q", got)
	}
	bodies := h.reportBodies()
	for i, b := range bodies {
		if b == "The sheet has no picker yet." {
			if i+1 >= len(bodies) || bodies[i+1] != "Ran Read" {
				t.Errorf("the say is not followed by its receipt: %q", bodies)
			}
		}
	}
}

// After a person's message goes in, the reply goes out within the coalesce
// delay rather than waiting a minute for the next receipt.
func TestAReplyToAMessageIsSentAtOnce(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.Delay = 200 * time.Millisecond
	h.agent.Script = []adapter.Event{
		{Kind: adapter.EventToolUse, Tool: &adapter.ToolUse{Name: "Read"}},
		{Kind: adapter.EventText, Text: "Yes, the Windows port stays out."},
		{Kind: adapter.EventIdle},
		{Kind: adapter.EventIdle},
		{Kind: adapter.EventIdle},
		{Kind: adapter.EventQuestion, Text: "Which target framework?"},
	}
	var n int
	var mu sync.Mutex
	h.elk.Handle("report_progress", func(map[string]any) elktest.Reply {
		mu.Lock()
		defer mu.Unlock()
		if n++; n == 2 { // the Read receipt
			return elktest.Reply{Text: "Progress recorded on `run-1`. Lease refreshed.\n" + commentBlock}
		}
		return elktest.Reply{Text: "Progress recorded on `run-1`. Lease refreshed."}
	})
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if sent := h.agent.Sessions()[0].Sent(); len(sent) != 1 {
		t.Fatalf("the comment did not go in: %q", sent)
	}
	bodies := h.reportBodies()
	say, question := -1, -1
	for i, b := range bodies {
		switch b {
		case "Yes, the Windows port stays out.":
			say = i
		case "Which target framework?":
			question = i
		}
	}
	if say < 0 || question < 0 || say > question {
		t.Errorf("the reply was not sent before the agent's next report: %q", bodies)
	}
}
