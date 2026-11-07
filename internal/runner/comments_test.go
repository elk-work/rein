package runner_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runlog"
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

// A comment in the reply to a report outside watch (here the opening one)
// is not lost: it reaches the session at the next receipt.
func TestCommentFromOpeningReportReachesSession(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.Script = []adapter.Event{
		{Kind: adapter.EventToolUse, Tool: &adapter.ToolUse{Name: "Read"}},
		{Kind: adapter.EventText, Text: "carrying on"},
	}
	var n int
	var mu sync.Mutex
	h.elk.Handle("report_progress", func(map[string]any) elktest.Reply {
		mu.Lock()
		defer mu.Unlock()
		if n++; n == 1 {
			return elktest.Reply{Text: "Progress recorded on `run-1`. Lease refreshed.\n" + commentBlock}
		}
		return elktest.Reply{Text: "Progress recorded on `run-1`. Lease refreshed."}
	})
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if sent := h.agent.Sessions()[0].Sent(); len(sent) != 1 || sent[0] != commentBlock {
		t.Errorf("sent = %q, want the opening reply's comment block once", sent)
	}
}

// A comment that arrives while the agent's question is open is held, not
// sent: an adapter may take input then as the answer (Codex does).
func TestCommentsWaitForAnOpenQuestion(t *testing.T) {
	h := commentHarness(t)
	h.agent.Script = []adapter.Event{
		{Kind: adapter.EventQuestion, Text: "Which target framework?"},
	}
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	if sent := h.agent.Sessions()[0].Sent(); len(sent) != 0 {
		t.Errorf("a comment went in while the question was open: %q", sent)
	}
}

func TestWatchReportsWhenSendFails(t *testing.T) {
	h := commentHarness(t)
	h.agent.Capabilities.Capabilities[adapter.CapSteer] = adapter.SupportNo
	logs := openRunLogs(t)
	if err := h.run(runner.Options{Logs: logs}); err != nil {
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
	recs, err := logs.Records("run-1")
	if err != nil {
		t.Fatal(err)
	}
	var notes []string
	for _, r := range recs {
		if r.Kind == string(runlog.KindNote) {
			notes = append(notes, r.Text)
		}
	}
	if !strings.Contains(strings.Join(notes, "\n"), "leave the Windows port out") {
		t.Errorf("the undelivered comment is not in the local run log: %q", notes)
	}
}

// A comment delivered while Rein waits on the review rides along with the
// revisions it is waiting for.
func TestAwaitReviewCarriesComments(t *testing.T) {
	h := reviewHarness(t, "No question asked yet.\n"+commentBlock, askRevisions)
	if err := h.run(reviewOptions()); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	specs := h.agent.Specs()
	if len(specs) != 2 {
		t.Fatalf("the agent ran %d times, want 2\nlog:\n%s", len(specs), h.log)
	}
	if !strings.Contains(specs[1].Prompt, "does not say which tests were run") ||
		!strings.Contains(specs[1].Prompt, "leave the Windows port out") {
		t.Errorf("the revision round lost the verdict or the comments:\n%s", specs[1].Prompt)
	}
	if strings.Contains(specs[1].Prompt, "> - Nathanael") || strings.Contains(specs[1].Prompt, "> New comment") {
		t.Errorf("the comments read as part of the review:\n%s", specs[1].Prompt)
	}
}

// The tool report is a receipt: what ran, with the agent's Why and Next.
func TestToolReportIsAReceipt(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.Script = []adapter.Event{
		{Kind: adapter.EventText, Text: "Why: the sheet has no picker\nNext: run the tests"},
		{Kind: adapter.EventToolUse, Tool: &adapter.ToolUse{Name: "Edit", Input: []byte(`{"file_path":"/w/a.ts"}`)}},
	}
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	for _, r := range h.elk.CallsTo("report_progress") {
		if r.Arg("body") == "Edited a.ts" {
			if r.Arg("why") != "the sheet has no picker" || r.Arg("next") != "run the tests" {
				t.Errorf("receipt = %#v", r.Args)
			}
			return
		}
	}
	t.Errorf("no receipt for the edit\nlog:\n%s", h.log)
}
