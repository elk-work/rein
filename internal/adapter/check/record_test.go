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
	body := strings.Join(lines[open+1:end], "\n") + "\n"
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("the block is not one JSON object: %v\n%s", err, body)
	}
	return body, m
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
