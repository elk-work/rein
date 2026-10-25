package check

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// recordFromDeliverable reads the record out of a deliverable the way Elk
// does: the last line that is exactly RecordHeading, then the ```json fence
// after it, to the next line that is exactly ```. It also holds the contract
// around it: nothing but blank lines between the heading and the fence,
// nothing after the fence, and the fenced block under 8 KiB.
func recordFromDeliverable(t *testing.T, deliverable string) (string, map[string]any) {
	t.Helper()
	body := recordBody(t, deliverable)
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("the block is not one JSON object: %v\n%s", err, body)
	}
	return body, m
}

// recordsFromDeliverable is [recordFromDeliverable] for a check that carried
// several suites: the same contract, and the block holds a JSON array of
// records.
func recordsFromDeliverable(t *testing.T, deliverable string) (string, []map[string]any) {
	t.Helper()
	body := recordBody(t, deliverable)
	if !strings.HasPrefix(body, "[\n") {
		t.Fatalf("the block is not an array:\n%s", body)
	}
	var ms []map[string]any
	if err := json.Unmarshal([]byte(body), &ms); err != nil {
		t.Fatalf("the block is not a JSON array of objects: %v\n%s", err, body)
	}
	return body, ms
}

// recordBody is the JSON between the record's fences, after holding the
// contract around it.
func recordBody(t *testing.T, deliverable string) string {
	t.Helper()
	lines := strings.Split(deliverable, "\n")
	at := -1
	for i, l := range lines {
		if l == RecordHeading {
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("no %q line in the deliverable:\n%s", RecordHeading, deliverable)
	}
	open := -1
	for i := at + 1; i < len(lines) && open < 0; i++ {
		switch {
		case lines[i] == "```json":
			open = i
		case strings.TrimSpace(lines[i]) != "":
			t.Fatalf("%q sits between the heading and the record's fence", lines[i])
		}
	}
	end := -1
	for i := open + 1; open >= 0 && i < len(lines); i++ {
		if lines[i] == "```" {
			end = i
			break
		}
	}
	if open < 0 || end < 0 {
		t.Fatalf("no fenced json block after %q:\n%s", RecordHeading, deliverable)
	}
	for _, l := range lines[end+1:] {
		if strings.TrimSpace(l) != "" {
			t.Fatalf("the record is not the deliverable's last section; %q follows it", l)
		}
	}
	if block := strings.Join(lines[open:end+1], "\n") + "\n"; len(block) >= maxRecordBlock {
		t.Errorf("the fenced block is %d bytes; it must stay under 8 KiB", len(block))
	}
	return strings.Join(lines[open+1:end], "\n") + "\n"
}

// withoutEmittedBy is the block's JSON with the field Rein adds taken out
// again: what the record file holds.
func withoutEmittedBy(body string) string {
	return strings.Replace(body, ",\n  \"emitted_by\": \"rein\"\n}", "\n}", 1)
}

func sampleRecord(t *testing.T, paths []string) SuiteRecord {
	t.Helper()
	c := Check{Repo: "elk-work/scout", SHA: testSHA, Workflow: ".github/workflows/quick-checks.yml", Job: "checks",
		Suite: "migration-numbers", Provider: "linux-lab", ChangedPaths: paths}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC)
	tl := tally{steps: 3, passed: 3}
	return newRecord(c, "lab-checks", "main", ResultPass, &tl, start, start.Add(2*time.Second))
}

func TestTheInlineRecordIsTheRecordFileByteForByte(t *testing.T) {
	rec := sampleRecord(t, []string{"supabase/migrations/0550_x.sql"})
	file, err := encodeRecord(inlineRecord{SuiteRecord: rec})
	if err != nil {
		t.Fatal(err)
	}
	// The wrapper adds nothing of its own when its fields are empty: the
	// file is what pulse has always read.
	bare, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(file, append(bare, '\n')) {
		t.Fatalf("the record file changed shape:\n%s\nwant\n%s", file, bare)
	}

	body, m := recordFromDeliverable(t, recordBlock(rec, false))
	if body != string(file) {
		t.Errorf("the block is not the file's bytes:\n%s\nfile:\n%s", body, file)
	}
	if _, ok := m["emitted_by"]; ok {
		t.Error("emitted_by set on a record Rein did not send")
	}

	body, m = recordFromDeliverable(t, recordBlock(rec, true))
	if m["emitted_by"] != "rein" {
		t.Errorf("emitted_by = %v, want rein", m["emitted_by"])
	}
	if withoutEmittedBy(body) != string(file) {
		t.Errorf("the block is not the file's bytes plus emitted_by:\n%s", body)
	}
	if _, ok := m["note"]; ok {
		t.Error("a note on a record that fits")
	}
}

func TestABacktickInTheRecordCannotCloseTheFence(t *testing.T) {
	rec := sampleRecord(t, []string{"docs/a```b.md"})
	rec.Suite = "odd```suite"
	block := recordBlock(rec, false)
	inner := strings.TrimPrefix(block, RecordHeading+"\n\n```json\n")
	inner = strings.TrimSuffix(inner, "```\n")
	if strings.Contains(inner, "`") {
		t.Fatalf("a backtick reached the block:\n%s", block)
	}
	_, m := recordFromDeliverable(t, block)
	if m["suite"] != "odd```suite" || m["changed_paths"].([]any)[0] != "docs/a```b.md" {
		t.Errorf("the escaped strings did not read back: %v", m)
	}
}

func TestChangedPathsAreCappedAsPulseCapsThem(t *testing.T) {
	small := []string{"a.go", "b.go"}
	if got := capChangedPaths(small); strings.Join(got, ",") != "a.go,b.go" {
		t.Errorf("a list that fits changed: %v", got)
	}

	var paths []string
	for i := 0; i < 500; i++ {
		paths = append(paths, fmt.Sprintf("supabase/migrations/%04d_a_long_migration_name.sql", i))
	}
	got := capChangedPaths(paths)
	b, _ := json.Marshal(got)
	if len(b) > changedPathsLimit {
		t.Fatalf("capped list is %d bytes, over %d", len(b), changedPathsLimit)
	}
	n := len(got) - 1
	if got[n] != changedPathsMarker || n == 0 {
		t.Fatalf("capped list does not end with the marker after a prefix: %v", got)
	}
	for i := 0; i < n; i++ {
		if got[i] != paths[i] {
			t.Fatalf("path %d is %q, want the prefix in order (%q)", i, got[i], paths[i])
		}
	}
	longer := append(append([]string{}, paths[:n+1]...), changedPathsMarker)
	if lb, _ := json.Marshal(longer); len(lb) <= changedPathsLimit {
		t.Errorf("the prefix is not the longest that fits: %d paths would too", n+1)
	}
	if again := capChangedPaths(got); strings.Join(again, "\n") != strings.Join(got, "\n") {
		t.Error("capping a capped list changed it; pulse would see something different from the file")
	}

	rec := sampleRecord(t, paths)
	if strings.Join(rec.ChangedPaths, "\n") != strings.Join(got, "\n") {
		t.Error("the record does not carry the capped list")
	}
}

func TestTheRecordBlockStaysUnder8KiB(t *testing.T) {
	// The cap keeps a real record far below the bound; the bound is the
	// backstop, so build an over-long record by hand.
	rec := sampleRecord(t, nil)
	for i := 0; i < 10000; i++ {
		rec.ChangedPaths = append(rec.ChangedPaths, fmt.Sprintf("Apps/ElkKit/Sources/Module%05d/File.swift", i))
	}
	for _, emitted := range []bool{false, true} {
		_, m := recordFromDeliverable(t, recordBlock(rec, emitted))
		if paths, ok := m["changed_paths"].([]any); !ok || len(paths) != 0 {
			t.Errorf("changed_paths = %v, want []", m["changed_paths"])
		}
		note, _ := m["note"].(string)
		if !strings.Contains(note, "10000") || !strings.Contains(note, "8 KiB") {
			t.Errorf("note = %q", note)
		}
		if _, ok := m["emitted_by"]; ok != emitted {
			t.Errorf("emitted_by present = %v, want %v", ok, emitted)
		}
	}
	if len(rec.ChangedPaths) != 10000 {
		t.Error("building the block changed the record it was given")
	}
}

// Every field but changed_paths is bounded by the packet's validation, the
// queue name's limit and the record's own shape, so once changed_paths is
// dropped even the largest record a packet can produce fits.
func TestTheLargestPossibleRecordFitsOnceItsPathsAreDropped(t *testing.T) {
	c := Check{
		Repo:     strings.Repeat("o", 39) + "/" + strings.Repeat("r", 100),
		SHA:      strings.Repeat("f", 64),
		Workflow: ".github/workflows/" + strings.Repeat("w", 60) + ".yml",
		Job:      strings.Repeat("j", 100),
		Suite:    strings.Repeat("<", 200), // six bytes each once escaped
		Provider: strings.Repeat("p", 64),
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if originRepo("https://github.com/"+strings.Repeat("o", 40)+"/x") != "" {
		t.Error("an owner longer than GitHub allows was accepted")
	}
	start := time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC)
	rec := newRecord(c, strings.Repeat("q", 12), strings.Repeat("b", 200), ResultCancelled, nil, start, start)
	rec.Counts = map[string]int{"steps": math.MaxInt, "steps_passed": math.MaxInt, "steps_failed": math.MaxInt,
		"steps_skipped": math.MaxInt}
	for i := 0; i < 5000; i++ {
		rec.ChangedPaths = append(rec.ChangedPaths, strings.Repeat("x", 200))
	}
	_, m := recordFromDeliverable(t, recordBlock(rec, true))
	if m["note"] == nil {
		t.Error("no note on a record whose paths were dropped")
	}
}

// sharedRecords is one run's records for three suites, as newRecords makes
// them.
func sharedRecords(t *testing.T, paths []string) []SuiteRecord {
	t.Helper()
	c := Check{Repo: "elk-work/scout", SHA: testSHA, Workflow: ".github/workflows/quick-checks.yml", Job: "checks",
		Suite: "migration-numbers", Suites: []string{"migration-numbers", "no-schema-fallbacks", "elk-type"},
		Provider: "linux-lab", ChangedPaths: paths}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC)
	tl := tally{steps: 3, passed: 3}
	return newRecords(c, "lab-checks", "main", ResultPass, &tl, start, start.Add(2*time.Second))
}

func TestASharedRunsRecordsDifferOnlyInSuite(t *testing.T) {
	recs := sharedRecords(t, []string{"supabase/migrations/0559_x.sql"})
	if len(recs) != 3 {
		t.Fatalf("%d records, want 3", len(recs))
	}
	for i, want := range []string{"migration-numbers", "no-schema-fallbacks", "elk-type"} {
		if recs[i].Suite != want {
			t.Errorf("record %d is for %q, want %q", i, recs[i].Suite, want)
		}
		same := recs[i]
		same.Suite = recs[0].Suite
		a, _ := json.Marshal(same)
		b, _ := json.Marshal(recs[0])
		if !bytes.Equal(a, b) {
			t.Errorf("record %d differs from the first in more than its suite:\n%s\n%s", i, a, b)
		}
	}
}

func TestASharedRunsBlockIsAnArrayOfTheRecordFiles(t *testing.T) {
	recs := sharedRecords(t, []string{"supabase/migrations/0559_x.sql", "docs/a```b.md"})
	block := recordsBlock(recs, []bool{true, false, true})
	body, ms := recordsFromDeliverable(t, block)
	if strings.Contains(body, "`") {
		t.Fatalf("a backtick reached the block:\n%s", body)
	}
	if len(ms) != len(recs) {
		t.Fatalf("%d records in the block, want %d", len(ms), len(recs))
	}
	for i, rec := range recs {
		file, err := encodeRecord(inlineRecord{SuiteRecord: rec})
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]any
		if err := json.Unmarshal(file, &want); err != nil {
			t.Fatal(err)
		}
		got := ms[i]
		emitted, has := got["emitted_by"]
		if wantEmitted := i != 1; has != wantEmitted || (has && emitted != "rein") {
			t.Errorf("record %d: emitted_by = %v (present %v), want present %v", i, emitted, has, wantEmitted)
		}
		delete(got, "emitted_by")
		if _, ok := got["note"]; ok {
			t.Errorf("record %d has a note although the block fits", i)
		}
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		if !bytes.Equal(gb, wb) {
			t.Errorf("record %d is not its file's JSON:\n%s\nfile:\n%s", i, gb, wb)
		}
	}
	if ms[1]["changed_paths"].([]any)[1] != "docs/a```b.md" {
		t.Errorf("the escaped path did not read back: %v", ms[1]["changed_paths"])
	}
}

func TestASharedRunsBlockStaysUnder8KiB(t *testing.T) {
	recs := sharedRecords(t, nil)
	for i := range recs {
		for j := 0; j < 10000; j++ {
			recs[i].ChangedPaths = append(recs[i].ChangedPaths, fmt.Sprintf("Apps/ElkKit/Sources/Module%05d/File.swift", j))
		}
	}
	_, ms := recordsFromDeliverable(t, recordsBlock(recs, []bool{false, true, false}))
	for i, m := range ms {
		if paths, ok := m["changed_paths"].([]any); !ok || len(paths) != 0 {
			t.Errorf("record %d: changed_paths = %v, want []", i, m["changed_paths"])
		}
		note, _ := m["note"].(string)
		if !strings.Contains(note, "10000") || !strings.Contains(note, "8 KiB") {
			t.Errorf("record %d: note = %q", i, note)
		}
		if _, ok := m["emitted_by"]; ok != (i == 1) {
			t.Errorf("record %d: emitted_by present = %v", i, ok)
		}
	}
	for i := range recs {
		if len(recs[i].ChangedPaths) != 10000 {
			t.Error("building the block changed the records it was given")
		}
	}
}

// Validate refuses a check whose records could not all fit, measuring the
// worst case; so the largest shared run it accepts, filled in with the
// longest values a run can produce, still fits.
func TestTheLargestSharedRunValidateAcceptsFits(t *testing.T) {
	longest := func(n int) Check {
		c := Check{
			Repo:     strings.Repeat("o", 39) + "/" + strings.Repeat("r", 100),
			SHA:      strings.Repeat("f", 64),
			Ref:      strings.Repeat("b", 200),
			Workflow: ".github/workflows/" + strings.Repeat("w", 60) + ".yml",
			Job:      strings.Repeat("j", 100),
			Provider: strings.Repeat("p", 64),
		}
		for i := 0; i < n; i++ {
			c.Suites = append(c.Suites, strings.Repeat("<", 199)+string(rune('a'+i)))
		}
		return c
	}
	n := 1
	for ; n < maxSuites; n++ {
		c := longest(n + 1)
		if c.Validate() != nil {
			break
		}
	}
	if n < 2 {
		t.Fatalf("Validate accepts only %d suites of the longest names; the test needs a shared run", n)
	}
	c := longest(n)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 24, 22, 0, 0, 123_000_000, time.UTC)
	for i := 0; i < 5000; i++ {
		c.ChangedPaths = append(c.ChangedPaths, strings.Repeat("x", 200))
	}
	recs := newRecords(c, strings.Repeat("q", 12), candidateBranch(c.Ref), ResultCancelled, nil, start, start.Add(time.Hour))
	for i := range recs {
		recs[i].DurationMS = math.MaxInt64
		recs[i].Counts = map[string]int{"steps": math.MaxInt, "steps_passed": math.MaxInt, "steps_failed": math.MaxInt,
			"steps_skipped": math.MaxInt}
		recs[i].ChangedPaths = c.ChangedPaths // past the cap, to force the drop
	}
	emitted := make([]bool, len(recs))
	for i := range emitted {
		emitted[i] = true
	}
	_, ms := recordsFromDeliverable(t, recordsBlock(recs, emitted))
	if len(ms) != n || ms[0]["note"] == nil {
		t.Errorf("%d records, note %v", len(ms), ms[0]["note"])
	}
	t.Logf("Validate accepts at most %d suites of the longest possible names", n)
}
