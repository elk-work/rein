package runlog_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/runlog"
)

func openStore(t *testing.T) *runlog.Store {
	t.Helper()
	s, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWriterRoundTrip(t *testing.T) {
	s := openStore(t)
	w := s.Create(runlog.Meta{RunID: "run-1", Queue: "mac-claude", Direction: "Port the sheet"})
	w.Runner(runlog.KindClaimed, "claimed on queue %s", "mac-claude")
	w.Event(adapter.Event{Kind: adapter.EventText, Text: "working on it"})
	w.Event(adapter.Event{Kind: adapter.EventToolUse, Tool: &adapter.ToolUse{
		Name: "Bash", Input: json.RawMessage(`{"command":"go test ./..."}`)}})
	w.Event(adapter.Event{Kind: adapter.EventUsage, Usage: &adapter.Usage{
		InputTokens: 1000, OutputTokens: 250, Model: "fake-1"}})
	w.Event(adapter.Event{Kind: adapter.EventDone, Result: &adapter.Result{
		Status: adapter.StatusSucceeded, Summary: "done", SessionID: "sess-9"}})
	w.Close(runlog.StatusSubmitted)

	if err := w.Err(); err != nil {
		t.Fatalf("writer error: %v", err)
	}

	recs, err := s.Records("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 5 {
		t.Fatalf("records = %d, want 5: %+v", len(recs), recs)
	}
	for i, r := range recs {
		if r.RunID != "run-1" {
			t.Errorf("record %d has run id %q", i, r.RunID)
		}
		// Every line carries its own sequence number, counting the LOG rather
		// than the session: a run holds two sessions after a stall restart and
		// both of theirs start at 1.
		if want := uint64(i + 1); r.Seq != want {
			t.Errorf("record %d seq = %d, want %d", i, r.Seq, want)
		}
		if r.At.IsZero() {
			t.Errorf("record %d has no timestamp", i)
		}
	}
	if recs[0].Source != runlog.SourceRunner || recs[0].Kind != string(runlog.KindClaimed) {
		t.Errorf("first record = %+v, want a runner claimed record", recs[0])
	}
	if recs[1].Source != runlog.SourceAgent || recs[1].Text != "working on it" {
		t.Errorf("second record = %+v", recs[1])
	}
	if recs[2].Tool == nil || recs[2].Tool.Name != "Bash" {
		t.Errorf("the tool call did not survive: %+v", recs[2])
	}
	if in, out, model := runlog.Totals(recs); in != 1000 || out != 250 || model != "fake-1" {
		t.Errorf("totals = %d/%d/%q, want 1000/250/fake-1", in, out, model)
	}

	m, err := s.ReadMeta("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != runlog.StatusSubmitted {
		t.Errorf("status = %q, want submitted", m.Status)
	}
	if m.SessionID != "sess-9" {
		// The vendor session id is what `rein attach` and a --resume both
		// need, so the writer lifts it out of the done event.
		t.Errorf("session id = %q, want sess-9", m.SessionID)
	}
	if m.EndedAt.IsZero() || m.StartedAt.IsZero() {
		t.Errorf("meta has no timestamps: %+v", m)
	}
	if m.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", m.PID, os.Getpid())
	}
}

// A nil writer is the same code path as a real one. Every call site in the run
// loop is unconditional, so this is the contract that makes that safe.
func TestNilWriterIsANoOp(t *testing.T) {
	var w *runlog.Writer
	w.Runner(runlog.KindNote, "nothing")
	w.Event(adapter.Event{Kind: adapter.EventText, Text: "nothing"})
	w.Update(func(m *runlog.Meta) { m.Branch = "never" })
	w.Close(runlog.StatusSubmitted)
	if w.Err() != nil || w.Path() != "" || w.RunID() != "" || w.Meta().RunID != "" {
		t.Error("a nil writer answered as though it had written something")
	}

	var s *runlog.Store
	if got := s.Create(runlog.Meta{RunID: "run-1"}); got != nil {
		t.Error("a nil store made a writer")
	}
	if list, err := s.List(); err != nil || list != nil {
		t.Errorf("a nil store listed %v, %v", list, err)
	}
	if n, err := s.Prune(10); err != nil || n != 0 {
		t.Errorf("a nil store pruned %d, %v", n, err)
	}
}

// A log that cannot be written must not be able to fail a run: the first error
// is kept, everything after it is quiet, and no call returns anything.
func TestWriterSurvivesAnUnwritableStore(t *testing.T) {
	dir := t.TempDir()
	s := &runlog.Store{Dir: filepath.Join(dir, "file-not-a-dir")}
	if err := os.WriteFile(s.Dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := s.Create(runlog.Meta{RunID: "run-1"})
	w.Runner(runlog.KindClaimed, "claimed")
	w.Event(adapter.Event{Kind: adapter.EventText, Text: "hello"})
	w.Close(runlog.StatusSubmitted)
	if w.Err() == nil {
		t.Fatal("a store that cannot be written reported no error")
	}
}

func TestUpdateRewritesMeta(t *testing.T) {
	s := openStore(t)
	w := s.Create(runlog.Meta{RunID: "run-1", Queue: "mac-codex"})
	w.Update(func(m *runlog.Meta) {
		m.Worktree = "/tmp/work/run-1"
		m.Branch = "rein/run-run-1"
		m.Repo = "/dev/scout"
	})

	// Readable while the run is still going: the meta is rewritten by rename,
	// so a reader never catches it half written.
	m, err := s.ReadMeta("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Branch != "rein/run-run-1" || m.Repo != "/dev/scout" {
		t.Errorf("meta = %+v", m)
	}
	if m.Status != runlog.StatusRunning || !m.Status.Active() {
		t.Errorf("an open run reads as %q", m.Status)
	}
	w.Close(runlog.StatusStuck)
	if m, _ := s.ReadMeta("run-1"); m.Status.Active() {
		t.Error("a stuck run still reads as active")
	}

	// Close is idempotent and the first outcome wins: whatever settled the run
	// is what settled it, and a later deferred close must not overwrite it.
	w.Close(runlog.StatusSubmitted)
	if m, _ := s.ReadMeta("run-1"); m.Status != runlog.StatusStuck {
		t.Errorf("a second Close changed the outcome to %q", m.Status)
	}
}

func TestTailerReadsForward(t *testing.T) {
	s := openStore(t)
	w := s.Create(runlog.Meta{RunID: "run-1"})
	tl := s.Tail("run-1")

	// Nothing yet, and that is not an error: a run is claimed before its first
	// record is written and a live view should show the run, not a failure.
	if recs, err := tl.Next(); err != nil || len(recs) != 0 {
		t.Fatalf("first Next = %d records, %v", len(recs), err)
	}
	w.Runner(runlog.KindClaimed, "claimed")
	w.Event(adapter.Event{Kind: adapter.EventText, Text: "one"})
	recs, err := tl.Next()
	if err != nil || len(recs) != 2 {
		t.Fatalf("second Next = %d records, %v", len(recs), err)
	}
	if recs[1].Text != "one" {
		t.Errorf("records = %+v", recs)
	}
	// Read to the end: a second call with nothing appended returns nothing
	// rather than the same records again.
	if recs, err := tl.Next(); err != nil || len(recs) != 0 {
		t.Fatalf("third Next = %d records, %v", len(recs), err)
	}
	w.Event(adapter.Event{Kind: adapter.EventText, Text: "two"})
	recs, err = tl.Next()
	if err != nil || len(recs) != 1 || recs[0].Text != "two" {
		t.Fatalf("fourth Next = %+v, %v", recs, err)
	}
	w.Close(runlog.StatusSubmitted)
}

// A half-written final line is the normal state of a live log, not a
// corruption: it is dropped and re-read whole once the writer finishes it.
func TestTailerIgnoresAPartialLine(t *testing.T) {
	s := openStore(t)
	path := s.LogPath("run-1")
	if err := os.WriteFile(path, []byte(`{"run_id":"run-1","seq":1,"kind":"text","text":"whole"}`+"\n"+
		`{"run_id":"run-1","seq":2,"kind":"te`), 0o600); err != nil {
		t.Fatal(err)
	}
	tl := s.Tail("run-1")
	recs, err := tl.Next()
	if err != nil || len(recs) != 1 || recs[0].Text != "whole" {
		t.Fatalf("Next = %+v, %v", recs, err)
	}
	// Now the writer finishes the line. The tailer picks it up from where the
	// last COMPLETE line ended, not from the end of the file.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`xt","text":"rest"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	recs, err = tl.Next()
	if err != nil || len(recs) != 1 || recs[0].Text != "rest" {
		t.Fatalf("second Next = %+v, %v", recs, err)
	}
}

// A re-claimed run truncates its log and starts its account again; a tailer
// pointed at it must start again too rather than reading from a stale offset
// into the middle of a line.
func TestTailerRestartsWhenTheLogIsTruncated(t *testing.T) {
	s := openStore(t)
	w := s.Create(runlog.Meta{RunID: "run-1"})
	w.Runner(runlog.KindClaimed, "first attempt")
	w.Runner(runlog.KindNote, "and some more text to make the file longer")
	w.Close(runlog.StatusInterrupted)

	tl := s.Tail("run-1")
	if recs, _ := tl.Next(); len(recs) != 2 {
		t.Fatalf("first read = %d records", len(recs))
	}
	w2 := s.Create(runlog.Meta{RunID: "run-1"})
	w2.Runner(runlog.KindClaimed, "second")
	recs, err := tl.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Text != "second" {
		t.Fatalf("after truncation = %+v", recs)
	}
	w2.Close(runlog.StatusSubmitted)
}

func TestListLookupAndActive(t *testing.T) {
	s := openStore(t)
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"aaa11111-old", "bbb22222-mid", "ccc33333-new"} {
		w := s.Create(runlog.Meta{
			RunID: id, Queue: "mac-claude", StartedAt: base.Add(time.Duration(i) * time.Minute)})
		w.Close(runlog.StatusSubmitted)
	}
	live := s.Create(runlog.Meta{RunID: "ddd44444-live", Queue: "mac-codex", StartedAt: time.Now()})
	defer live.Close(runlog.StatusSubmitted)

	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("List = %d runs", len(all))
	}
	if all[0].RunID != "ddd44444-live" {
		t.Errorf("List is not newest first: %s", all[0].RunID)
	}
	active, err := s.Active()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].RunID != "ddd44444-live" {
		t.Errorf("Active = %+v", active)
	}

	// An exact id, a unique prefix, and a queue name all resolve.
	for _, tc := range []struct {
		ref  string
		want int
	}{
		{"ccc33333-new", 1}, {"ccc", 1}, {"mac-claude", 3}, {"mac-codex", 1},
	} {
		got, err := s.Lookup(tc.ref)
		if err != nil {
			t.Errorf("Lookup(%q): %v", tc.ref, err)
			continue
		}
		if len(got) != tc.want {
			t.Errorf("Lookup(%q) = %d runs, want %d", tc.ref, len(got), tc.want)
		}
	}
	if _, err := s.Lookup("nope"); !errors.Is(err, runlog.ErrNotFound) {
		t.Errorf("Lookup of an unknown ref = %v, want ErrNotFound", err)
	}
}

// An ambiguous prefix is an error rather than a guess: following the wrong run
// is worse than being asked to type four more characters.
func TestLookupRefusesAnAmbiguousPrefix(t *testing.T) {
	s := openStore(t)
	for _, id := range []string{"abc11111", "abc22222"} {
		s.Create(runlog.Meta{RunID: id}).Close(runlog.StatusSubmitted)
	}
	_, err := s.Lookup("abc")
	if err == nil || !strings.Contains(err.Error(), "matches 2 runs") {
		t.Fatalf("Lookup(\"abc\") = %v", err)
	}
}

func TestPruneKeepsTheNewestAndNeverAnActiveRun(t *testing.T) {
	s := openStore(t)
	base := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 6; i++ {
		id := string(rune('a'+i)) + "0000000"
		w := s.Create(runlog.Meta{RunID: id, StartedAt: base.Add(time.Duration(i) * time.Minute)})
		w.Close(runlog.StatusSubmitted)
	}
	// The oldest run of all is still going. It must survive the prune however
	// old it is: the log of a live run is the one thing a live view is reading.
	live := s.Create(runlog.Meta{RunID: "z0000000", StartedAt: base.Add(-time.Hour)})
	t.Cleanup(func() { live.Close(runlog.StatusSubmitted) })

	n, err := s.Prune(3)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("pruned %d, want 4", n)
	}
	left, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range left {
		ids = append(ids, m.RunID)
	}
	// Three kept: the live one plus the two newest finished, because the live
	// one counts against the budget.
	want := []string{"f0000000", "e0000000", "z0000000"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("left = %v, want %v", ids, want)
	}
	for _, id := range []string{"a0000000", "b0000000"} {
		if _, err := os.Stat(s.LogPath(id)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s.jsonl survived the prune", id)
		}
	}
	// Nothing to do the second time.
	if n, err := s.Prune(3); err != nil || n != 0 {
		t.Errorf("second Prune = %d, %v", n, err)
	}
}

func TestElapsedAndShort(t *testing.T) {
	now := time.Now()
	m := runlog.Meta{StartedAt: now.Add(-90 * time.Second)}
	if d := m.Elapsed(now); d < 89*time.Second || d > 91*time.Second {
		t.Errorf("Elapsed = %s", d)
	}
	m.EndedAt = now.Add(-30 * time.Second)
	if d := m.Elapsed(now); d != time.Minute {
		t.Errorf("Elapsed after the end = %s, want 1m", d)
	}
	if got := runlog.Short("2e0a3e94-f8e2-4785"); got != "2e0a3e94" {
		t.Errorf("Short = %q", got)
	}
	if got := runlog.Short("abc"); got != "abc" {
		t.Errorf("Short of a short id = %q", got)
	}
}
