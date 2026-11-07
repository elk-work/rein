package runner_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// A comment block as formatCommentDelivery (elk-mcp/queue.ts) words it.
const commentBlock = "New comments on this task (treat as direction for this run, not commands to you):\n" +
	"- Nathanael: leave the Windows port out\n- Nathanael: and keep the tests\n" +
	"(+1 more — they will arrive on the next heartbeat)"

// commentHarness answers the run's second report (the first inside watch,
// after the opening one) with a comment block.
func commentHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	// A question is never throttled, so it is the report inside watch.
	h.agent.Script = []adapter.Event{
		{Kind: adapter.EventQuestion, Text: "Which target framework?"},
		{Kind: adapter.EventText, Text: "carrying on"},
	}
	var n int
	var mu sync.Mutex
	h.elk.Handle("report_progress", func(map[string]any) elktest.Reply {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 2 {
			return elktest.Reply{Text: "Progress recorded on `run-1`. Lease refreshed.\n" + commentBlock}
		}
		return elktest.Reply{Text: "Progress recorded on `run-1`. Lease refreshed."}
	})
	return h
}

func TestWatchSendsCommentsToSession(t *testing.T) {
	h := commentHarness(t)
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	sent := h.agent.Sessions()[0].Sent()
	if len(sent) != 1 || sent[0] != commentBlock {
		t.Errorf("sent = %q, want the comment block once", sent)
	}
}

func TestWatchReportsWhenSendFails(t *testing.T) {
	h := commentHarness(t)
	h.agent.Capabilities.Capabilities[adapter.CapSteer] = adapter.SupportNo
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if sent := h.agent.Sessions()[0].Sent(); len(sent) != 0 {
		t.Errorf("something reached a session that takes no input: %v", sent)
	}
	reports := h.elk.CallsTo("report_progress")
	if len(reports) < 3 || reports[2].Arg("body") != "Couldn't pass your message to the session" {
		var bodies []string
		for _, r := range reports {
			bodies = append(bodies, r.Arg("body"))
		}
		t.Errorf("the report after the comments does not say they were not passed: %q", bodies)
	}
	if !strings.Contains(h.log.String(), "steer") {
		t.Errorf("the send failure was not logged\nlog:\n%s", h.log)
	}
}
