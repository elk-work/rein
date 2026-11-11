package runner_test

import (
	"path/filepath"
	"strings"
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

// A tool receipt names the files edited since the last one, relative to the
// repository; a path outside the worktree is left out.
func TestAToolReceiptCarriesTheEditedFiles(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	dir := filepath.Join(h.wts.root, "run-1")
	edit := func(path string) adapter.Event {
		return adapter.Event{Kind: adapter.EventToolUse,
			Tool: &adapter.ToolUse{Name: "Edit", Input: []byte(`{"file_path":"` + path + `"}`)}}
	}
	h.agent.Script = []adapter.Event{edit(filepath.Join(dir, "Sources", "Sheet.swift"))}
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	for _, c := range h.elk.CallsTo("report_progress") {
		if !strings.HasPrefix(c.Arg("body"), "Edited") {
			continue
		}
		files, _ := c.Args["files"].([]any)
		if len(files) != 1 || files[0] != "Sources/Sheet.swift" {
			t.Errorf("files = %#v", c.Args["files"])
		}
		return
	}
	t.Errorf("no tool receipt\nlog:\n%s", h.log)
}

// Claude's TodoWrite input, in the shape Claude Code sends it (hand-written;
// no recorded fixture carries one).
func todoWrite(statuses ...string) adapter.Event {
	in := `{"todos":[`
	for i, s := range statuses {
		if i > 0 {
			in += ","
		}
		in += `{"content":"Step ` + string(rune('A'+i)) + `","status":"` + s + `","activeForm":"Doing it"}`
	}
	in += `]}`
	return adapter.Event{Kind: adapter.EventToolUse, Tool: &adapter.ToolUse{Name: "TodoWrite", Input: []byte(in)}}
}

// TodoWrite becomes the run's plan: sent at once, then at most every five
// seconds, and the last one held is sent before the session's end is handled.
func TestTodoWriteSendsThePlan(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1"))
	h.agent.Script = []adapter.Event{
		todoWrite("in_progress", "pending"),
		todoWrite("completed", "in_progress"),
		todoWrite("completed", "completed", ""),
	}
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	var plans [][]any
	for _, c := range h.elk.CallsTo("report_progress") {
		if c.Arg("body") == "Ran TodoWrite" {
			t.Errorf("TodoWrite was also reported as a tool")
		}
		if c.Arg("body") != "Updated the plan" {
			continue
		}
		if c.Arg("kind") != "step" {
			t.Errorf("plan kind = %q", c.Arg("kind"))
		}
		plan, _ := c.Args["plan"].([]any)
		plans = append(plans, plan)
	}
	if len(plans) != 2 {
		t.Fatalf("plan reports = %d, want the first and the last: %#v", len(plans), plans)
	}
	last := plans[1]
	if len(last) != 3 {
		t.Fatalf("last plan = %#v", last)
	}
	want := []string{"completed", "completed", "pending"} // a missing status reads as pending
	for i, item := range last {
		m, _ := item.(map[string]any)
		if m["status"] != want[i] || m["content"] != "Step "+string(rune('A'+i)) {
			t.Errorf("item %d = %#v", i, m)
		}
		if _, extra := m["activeForm"]; extra {
			t.Errorf("item %d carries more than content and status: %#v", i, m)
		}
	}
}
