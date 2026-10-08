package tail_test

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/fake"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/tail"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// started is the fixed clock the golden files are rendered against. A real run
// takes however long it takes; a golden file cannot.
var (
	started = time.Date(2026, 8, 30, 9, 15, 0, 0, time.UTC)
	now     = started.Add(4*time.Minute + 12*time.Second)
)

// recordedRun drives the fake adapter through a session with one of every
// shape the renderer has a rule for, writes the events to a real run log the
// way the run loop does, and hands back what a reader sees.
//
// Driving the adapter rather than hand-writing records is the point: the
// golden then covers the path an actual run takes, including the terminal
// event the adapter appends for itself.
func recordedRun(t *testing.T) (*runlog.Store, runlog.Meta, []runlog.Record) {
	t.Helper()

	a := fake.New()
	a.Kind = "fake-tail"
	a.Delay = 0
	a.Script = []adapter.Event{
		{Kind: adapter.EventProgress, Text: "starting"},
		{Kind: adapter.EventText, Text: "I'll start by reading the failing test."},
		{Kind: adapter.EventToolUse, Tool: &adapter.ToolUse{
			ID: "t1", Name: "Read", Input: json.RawMessage(`{"file_path":"ElkKit/Tests/DueDateTests.swift"}`)}},
		{Kind: adapter.EventToolUse, Tool: &adapter.ToolUse{
			ID: "t2", Name: "Bash", Input: json.RawMessage(
				`{"command":"swift test --filter DueDate\n  2>&1 | tail -40","description":"run the suite"}`)}},
		{Kind: adapter.EventText, Text: "The assertion compares a formatted date to a literal,\nso it fails whenever the machine is not in UTC."},
		{Kind: adapter.EventToolUse, Tool: &adapter.ToolUse{
			ID: "t3", Name: "TodoWrite", Input: json.RawMessage(`{"todos":[{"content":"pin the timezone"}]}`)}},
		{Kind: adapter.EventPermissionRequest, Permission: &adapter.PermissionRequest{
			ID: "p1", Tool: "Bash", Summary: "git push -u origin rein/run-run-1"}},
		{Kind: adapter.EventQuestion, Text: "Should the fix pin the formatter to UTC, or take the timezone from the fixture?"},
		{Kind: adapter.EventUsage, Usage: &adapter.Usage{
			InputTokens: 11400, OutputTokens: 1050, Model: "fake-1"}},
		{Kind: adapter.EventIdle},
	}
	a.Result = adapter.Result{
		Status:    adapter.StatusSucceeded,
		Summary:   "Pinned the formatter to UTC and added a regression test.\nOpened elk-work/scout#812.",
		SessionID: "sess-fake-1",
	}

	store, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := store.Create(runlog.Meta{
		RunID:          "a1b2c3d4-5e6f-7890-abcd-ef1234567890",
		Queue:          "mac-claude",
		Workspace:      "Elk Scout",
		AgentKind:      "fake-tail",
		Direction:      "Fix the flaky due-date test in ElkKit",
		PermissionMode: "full",
		StartedAt:      started,
	})
	w.Runner(runlog.KindClaimed, "claimed on queue mac-claude — Fix the flaky due-date test in ElkKit")
	w.Runner(runlog.KindWorktree, "worktree /Users/x/.rein/work/run-1 on branch rein/run-run-1 from origin/main (abc12345)")

	sess, err := a.Start(context.Background(), adapter.RunSpec{
		RunID: "run-1", WorktreeDir: t.TempDir(), Prompt: "fix it", PermissionMode: adapter.PermissionFull,
	})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range sess.Events() {
		w.Event(ev)
	}
	w.Runner(runlog.KindSubmit, "submitted ready — Fix the flaky due-date test in ElkKit")
	w.Runner(runlog.KindReaped, "worktree /Users/x/.rein/work/run-1 removed; branch rein/run-run-1 was not")
	w.Close(runlog.StatusSubmitted)
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}

	meta, err := store.ReadMeta("a1b2c3d4-5e6f-7890-abcd-ef1234567890")
	if err != nil {
		t.Fatal(err)
	}
	// The golden is rendered against a fixed clock, so the end time has to be
	// one too.
	meta.EndedAt = now
	recs, err := store.Records(meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return store, meta, recs
}

// TestRenderGolden is the whole renderer over a whole recorded run: every mark,
// the tool summaries, the multi-line bodies, and the footer.
func TestRenderGolden(t *testing.T) {
	_, meta, recs := recordedRun(t)

	r := tail.Renderer{Width: 88}
	var st tail.RunState
	var b strings.Builder
	for _, rec := range recs {
		st.Observe(rec)
		for _, line := range r.Lines(rec) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	b.WriteString(r.Footer(meta, st, now) + "\n")
	b.WriteString(r.Total(meta, st, now) + "\n")

	compare(t, "run.golden", b.String())
}

// The dashboard is the all-queues block: fixed columns, and the direction
// taking whatever the terminal has left.
func TestDashboardGolden(t *testing.T) {
	_, meta, recs := recordedRun(t)
	var st tail.RunState
	for _, rec := range recs {
		st.Observe(rec)
	}
	second := runlog.Meta{
		RunID:     "9f8e7d6c-1111-2222-3333-444455556666",
		Queue:     "mac-codex",
		Direction: "Write docs/tail.md and a line in the README, then land it",
		Status:    runlog.StatusRunning,
		StartedAt: now.Add(-58 * time.Second),
	}
	third := runlog.Meta{
		RunID:     "0011223344556677",
		Queue:     "win-grok",
		Direction: "Triage the Windows service failure",
		Status:    runlog.StatusRunning,
		StartedAt: now.Add(-2 * time.Hour),
		Attached:  true,
	}
	live := meta
	live.Status = runlog.StatusRunning
	live.EndedAt = time.Time{}

	var b strings.Builder
	for _, width := range []int{100, 60} {
		r := tail.Renderer{Width: width}
		b.WriteString("--- width " + itoa(width) + "\n")
		rows := []tail.DashboardRow{
			{Meta: live, State: st},
			{Meta: second, State: tail.RunState{InputTokens: 2900, OutputTokens: 210, LastEvent: "▸ Write"}},
			{Meta: third, State: tail.RunState{InputTokens: 1_240_000, LastEvent: "text"}},
		}
		for _, line := range r.Dashboard(rows, now) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	b.WriteString("--- nothing in flight\n")
	for _, line := range (tail.Renderer{Width: 100}).Dashboard(nil, now) {
		b.WriteString(line + "\n")
	}
	compare(t, "dashboard.golden", b.String())
}

// Colour is an attribute wrapped around the line the golden already covers,
// and nothing else. Asserting that separately keeps escape sequences out of
// the golden file, where they are unreadable and impossible to review.
func TestColorWrapsAndDoesNotChangeTheText(t *testing.T) {
	rec := runlog.Record{Source: runlog.SourceAgent, Kind: string(adapter.EventToolUse),
		Tool: &adapter.ToolUse{Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}}
	plain := tail.Renderer{}.Lines(rec)
	coloured := tail.Renderer{Color: true}.Lines(rec)
	if len(plain) != 1 || len(coloured) != 1 {
		t.Fatalf("lines = %d / %d", len(plain), len(coloured))
	}
	if !strings.HasPrefix(coloured[0], "\x1b[") || !strings.HasSuffix(coloured[0], "\x1b[0m") {
		t.Errorf("coloured line is not wrapped: %q", coloured[0])
	}
	if stripped := strings.TrimSuffix(strings.TrimPrefix(coloured[0], "\x1b[36m"), "\x1b[0m"); stripped != plain[0] {
		t.Errorf("colour changed the text: %q vs %q", stripped, plain[0])
	}
}

func TestToolSummary(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"Bash", `{"command":"go test ./...","timeout":120}`, "go test ./..."},
		{"Read", `{"file_path":"/a/b.go","offset":10}`, "/a/b.go"},
		{"Grep", `{"pattern":"func Main","path":"."}`, "func Main"},
		{"WebFetch", `{"url":"https://elk.work","prompt":"summarise"}`, "https://elk.work"},
		{"Bash", "{\"command\":\"line one\\n  line two\"}", "line one line two"},
		{"Odd", `{"zeta":1,"alpha":2}`, "(alpha, zeta)"},
		{"Empty", `{}`, ""},
		{"Broken", `not json`, "not json"},
	}
	for _, c := range cases {
		got := tail.ToolSummary(&adapter.ToolUse{Name: c.name, Input: json.RawMessage(c.input)})
		if got != c.want {
			t.Errorf("%s(%s) = %q, want %q", c.name, c.input, got, c.want)
		}
	}
	if got := tail.ToolSummary(nil); got != "" {
		t.Errorf("nil tool = %q", got)
	}
}

// Usage never reaches the stream — it is the footer's business — and an empty
// text record produces nothing rather than a blank line.
func TestUsageAndEmptyRecordsProduceNoLines(t *testing.T) {
	for _, rec := range []runlog.Record{
		{Source: runlog.SourceAgent, Kind: string(adapter.EventUsage),
			Usage: &adapter.Usage{InputTokens: 10}},
		{Source: runlog.SourceAgent, Kind: string(adapter.EventText), Text: "  \n\n"},
		{Source: runlog.SourceRunner, Kind: string(runlog.KindNote)},
	} {
		if lines := (tail.Renderer{}).Lines(rec); len(lines) != 0 {
			t.Errorf("%s produced %q", rec.Kind, lines)
		}
	}
}

// The LAST column tracks what the agent is doing, and usage is not something
// it is doing: letting it win would make every busy run read "usage".
func TestRunStateIgnoresUsageForTheLastColumn(t *testing.T) {
	var st tail.RunState
	st.Observe(runlog.Record{Source: runlog.SourceAgent, Kind: string(adapter.EventToolUse),
		Tool: &adapter.ToolUse{Name: "Bash"}})
	st.Observe(runlog.Record{Source: runlog.SourceAgent, Kind: string(adapter.EventUsage),
		Usage: &adapter.Usage{InputTokens: 5, OutputTokens: 2, Model: "m"}})
	if st.LastEvent != "▸ Bash" {
		t.Errorf("LastEvent = %q", st.LastEvent)
	}
	if st.InputTokens != 5 || st.OutputTokens != 2 || st.Model != "m" {
		t.Errorf("totals = %+v", st)
	}
}

func compare(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v\n\nre-run with -update to write it. What was rendered:\n%s", err, got)
	}
	// The goldens are checked in with LF; a Windows checkout may have CRLF.
	if normalise(string(want)) != normalise(got) {
		t.Errorf("%s does not match. Got:\n%s\nWant:\n%s", path, got, want)
	}
}

func normalise(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
