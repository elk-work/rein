package check

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/config"
)

// SuiteRecord is the suite run record, schema_version 1, exactly as
// pulse/docs/suite-records.md defines it (elk-work/pulse internal/emit
// SuiteRecord). It is what Elk's suite history reads: `pulse suite-record
// --file` emits one to Signal, and Rein writes one for every check.
//
// counts, failing_test_ids and changed_paths are never null — pulse refuses a
// record whose arrays or object are missing — and branch is omitted unless
// the remote confirmed it.
type SuiteRecord struct {
	SchemaVersion  int            `json:"schema_version"`
	Repo           string         `json:"repo"`
	Suite          string         `json:"suite"`
	SHA            string         `json:"sha"`
	PR             int            `json:"pr"`
	Branch         string         `json:"branch,omitempty"`
	Provider       string         `json:"provider"`
	Runner         string         `json:"runner"`
	Result         string         `json:"result"`
	DurationMS     int64          `json:"duration_ms"`
	Counts         map[string]int `json:"counts"`
	FailingTestIDs []string       `json:"failing_test_ids"`
	ChangedPaths   []string       `json:"changed_paths"`
	RetentionClass string         `json:"retention_class"`
	Started        time.Time      `json:"started"`
	Ended          time.Time      `json:"ended"`
}

// Results a record may carry (pulse: pass, fail, error, cancelled, skip).
const (
	ResultPass      = "pass"
	ResultFail      = "fail"
	ResultError     = "error"
	ResultCancelled = "cancelled"
)

// newRecords is one record per suite the check satisfies, from the one run
// of its job: identical but for suite.
func newRecords(c Check, queue, branch, result string, t *tally, started, ended time.Time) []SuiteRecord {
	suites := c.Suites
	if len(suites) == 0 {
		suites = []string{c.Suite}
	}
	recs := make([]SuiteRecord, len(suites))
	for i, suite := range suites {
		recs[i] = newRecord(c, queue, branch, result, t, started, ended)
		recs[i].Suite = suite
	}
	return recs
}

// newRecord fills a record from the check and how the job went.
func newRecord(c Check, queue, branch, result string, t *tally, started, ended time.Time) SuiteRecord {
	counts := map[string]int{}
	if t != nil {
		counts = t.counts()
	}
	paths := c.ChangedPaths
	if paths == nil {
		paths = []string{}
	}
	paths = capChangedPaths(paths)
	if ended.Before(started) {
		ended = started
	}
	return SuiteRecord{
		SchemaVersion: 1, Repo: c.Repo, Suite: c.Suite, SHA: c.SHA, PR: 0, Branch: branch,
		Provider: c.Provider, Runner: queue, Result: result,
		DurationMS: ended.Sub(started).Milliseconds(), Counts: counts,
		FailingTestIDs: []string{}, ChangedPaths: paths, RetentionClass: "ci-history",
		Started: started.UTC().Truncate(time.Millisecond), Ended: ended.UTC().Truncate(time.Millisecond),
	}
}

// write saves the record as indented JSON, 0600: the bytes [recordBlock]
// carries inline when the check has one suite.
func (r SuiteRecord) write(path string) error {
	b, err := encodeRecord(inlineRecord{SuiteRecord: r})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// recordFileName is the file the i-th record (from 0) is written to: the
// first suite's is suite-record.json, as it was before a check could carry
// several, and the rest are numbered from 2.
func recordFileName(i int) string {
	if i == 0 {
		return "suite-record.json"
	}
	return fmt.Sprintf("suite-record-%d.json", i+1)
}

// The schema's bound on changed_paths (pulse/docs/suite-records.md): Stride
// keeps an attribute of at most 1,024 bytes, so a longer list keeps the
// longest prefix that fits beside a closing marker. It is not a path.
const (
	changedPathsLimit  = 1024
	changedPathsMarker = "...[truncated]"
)

// capChangedPaths applies that bound exactly as pulse does when it emits
// (elk-work/pulse internal/emit capChangedPaths): paths unchanged when their
// JSON fits, otherwise the longest prefix that fits with the marker last. A
// path is never split. Rein applies it when it writes the record, so the
// file, the copy in the deliverable and what pulse sends all agree, and a
// capped list passes through pulse unchanged.
func capChangedPaths(paths []string) []string {
	fits := func(p []string) bool {
		b, err := json.Marshal(p)
		return err == nil && len(b) <= changedPathsLimit
	}
	if fits(paths) {
		return paths
	}
	with := func(n int) []string {
		out := make([]string, n+1)
		copy(out, paths[:n])
		out[n] = changedPathsMarker
		return out
	}
	best, lo, hi := 0, 1, len(paths)
	for lo <= hi {
		mid := (lo + hi) / 2
		if fits(with(mid)) {
			best, lo = mid, mid+1
		} else {
			hi = mid - 1
		}
	}
	return with(best)
}

// inlineRecord is the record as a deliverable carries it: the record's own
// fields, in the file's order, then the two the deliverable may add. With
// both empty it encodes to exactly the bytes of the record file.
type inlineRecord struct {
	SuiteRecord
	// EmittedBy is "rein" when Rein itself sent the record with `pulse
	// suite-record` and the command succeeded. Elk forwards the record to
	// Signal only when it is absent, so a record is never sent twice.
	EmittedBy string `json:"emitted_by,omitempty"`
	// Note says what was left out to keep the block under maxRecordBlock.
	Note string `json:"note,omitempty"`
}

// encodeRecord is the one encoding of a record, for the file and the
// deliverable alike: indented JSON and a final newline. A backtick can only
// sit inside a JSON string, and is written as \u0060 — the same string to
// any JSON reader — so no line of the record can close a markdown fence.
func encodeRecord(r inlineRecord) ([]byte, error) { return encodeJSON(r) }

// encodeJSON is [encodeRecord] for a record or an array of them.
func encodeJSON(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	b = bytes.ReplaceAll(b, []byte("`"), []byte(`\u0060`))
	return append(b, '\n'), nil
}

// RecordHeading introduces the record in a check's deliverable. Elk reads
// the last line that is exactly this, then the fenced JSON block after it.
const RecordHeading = "### Suite record"

// maxRecordBlock bounds the fenced block, fences included: it stays under
// 8 KiB.
const maxRecordBlock = 8 << 10

// recordBlock is [recordsBlock] for a check with one suite.
func recordBlock(rec SuiteRecord, emitted bool) string {
	return recordsBlock([]SuiteRecord{rec}, []bool{emitted})
}

// recordsBlock is the deliverable's last section: [RecordHeading] and one
// fenced JSON block. For one record the block holds that record as an
// object, the bytes of the record file plus `"emitted_by": "rein"` when
// emitted[0] is true. For several it holds an array of them, in suite order,
// each with its own emitted_by. When the block would reach 8 KiB,
// changed_paths goes to [] on every record and a note on each says so;
// [Check.Validate] refused any check whose records would not fit even then.
func recordsBlock(recs []SuiteRecord, emitted []bool) string {
	ins := make([]inlineRecord, len(recs))
	paths := false
	for i, rec := range recs {
		ins[i] = inlineRecord{SuiteRecord: rec}
		if i < len(emitted) && emitted[i] {
			ins[i].EmittedBy = "rein"
		}
		paths = paths || len(rec.ChangedPaths) > 0
	}
	block := fenceRecords(ins)
	if len(block) >= maxRecordBlock && paths {
		for i := range ins {
			ins[i].Note = droppedNote(len(ins[i].ChangedPaths))
			ins[i].ChangedPaths = []string{}
		}
		block = fenceRecords(ins)
	}
	return RecordHeading + "\n\n" + block
}

// fenceRecords is the fenced block: one record as an object, several as an
// array.
func fenceRecords(ins []inlineRecord) string {
	var v any = ins
	if len(ins) == 1 {
		v = ins[0]
	}
	b, err := encodeJSON(v)
	if err != nil {
		return ""
	}
	return "```json\n" + string(b) + "```\n"
}

func droppedNote(n int) string {
	return fmt.Sprintf("changed_paths (%d entries) was left out to keep this block under 8 KiB; "+
		"the record file on the runner holds it, and [] here does not mean the diff was empty", n)
}

// worstRecordBlock is the size of the largest record block the check can
// produce: its own repository, commit, provider and suites, with everything
// the run fills in later at its longest — the ref's branch, a queue name of
// [config.QueueNameMax] bytes, counts and duration at their maximum, the
// longest result, millisecond timestamps — and on every record emitted_by
// and the note that replaces changed_paths. [Check.Validate] refuses a check
// whose worst case reaches 8 KiB, so every block a run writes fits.
func (c Check) worstRecordBlock() int {
	repo := c.Repo
	if repo == "" { // origin fills it in, and repoRE bounds it there
		repo = strings.Repeat("o", 39) + "/" + strings.Repeat("r", 100)
	}
	suites := c.Suites
	if len(suites) == 0 {
		suites = []string{c.Suite}
	}
	at := time.Date(9999, 12, 31, 23, 59, 59, 999_000_000, time.UTC)
	ins := make([]inlineRecord, len(suites))
	for i, suite := range suites {
		ins[i] = inlineRecord{SuiteRecord: SuiteRecord{
			SchemaVersion: 1, Repo: repo, Suite: suite, SHA: c.SHA, PR: 0, Branch: candidateBranch(c.Ref),
			Provider: c.Provider, Runner: strings.Repeat("q", config.QueueNameMax), Result: ResultCancelled,
			DurationMS: math.MaxInt64, Counts: map[string]int{"steps": math.MaxInt, "steps_passed": math.MaxInt,
				"steps_failed": math.MaxInt, "steps_skipped": math.MaxInt},
			FailingTestIDs: []string{}, ChangedPaths: []string{}, RetentionClass: "ci-history", Started: at, Ended: at,
		}, EmittedBy: "rein", Note: droppedNote(len(c.ChangedPaths))}
	}
	return len(fenceRecords(ins))
}

// branchRE is a short branch name pulse accepts: no ref path, no pull ref.
var (
	branchRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	pullRE   = regexp.MustCompile(`^(pull/)?[0-9]+/(merge|head)$`)
)

// candidateBranch is the branch a ref names, or "" when it names none:
// `refs/heads/main` and `main` are main; a tag, a pull ref or HEAD is nothing.
func candidateBranch(ref string) string {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "", ref == "HEAD", strings.HasPrefix(ref, "refs/tags/"), strings.HasPrefix(ref, "refs/pull/"):
		return ""
	case strings.HasPrefix(ref, "refs/heads/"):
		ref = strings.TrimPrefix(ref, "refs/heads/")
	case strings.HasPrefix(ref, "refs/"):
		return ""
	}
	if !branchRE.MatchString(ref) || pullRE.MatchString(ref) || strings.Contains(ref, "..") ||
		strings.HasPrefix(ref, "-") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") {
		return ""
	}
	return ref
}

// verifiedBranch is the record's branch: the ref's branch, but only when
// origin's tip of that branch is exactly the commit tested. Signal stores a
// missing branch as unknown and last green ignores it, so unknown is always
// safe; a wrong `main` is not (pulse/docs/suite-records.md, Branch
// provenance).
func verifiedBranch(ctx context.Context, git, dir, ref, sha string) string {
	b := candidateBranch(ref)
	if b == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, git, "-C", dir, "ls-remote", "origin", "refs/heads/"+b).Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) >= 2 && fields[0] == sha && fields[1] == "refs/heads/"+b {
		return b
	}
	return ""
}

// emission is what happened to the record after it was written.
type emission struct {
	sent bool
	note string
}

// emit sends the record to Elk's suite history with `pulse suite-record`,
// when this machine has pulse and the queue's environment holds the Stride
// endpoint and token. Otherwise the record stays a file on this machine and
// the deliverable says why. A failed send is reported, never a stuck run: the
// check ran, and its result is in the deliverable either way.
func emit(ctx context.Context, pulse, record string, env []string) emission {
	has := func(name string) bool {
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && k == name && v != "" {
				return true
			}
		}
		return false
	}
	if pulse == "" {
		return emission{note: "kept on this machine: pulse is not on PATH"}
	}
	if !has("STRIDE_INGEST_URL") || !has("STRIDE_TOKEN") {
		return emission{note: "kept on this machine: the queue's secrets hold no STRIDE_INGEST_URL and STRIDE_TOKEN"}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, pulse, "suite-record", "--file", record)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return emission{note: fmt.Sprintf("NOT sent: `pulse suite-record` failed (%v): %s", err, firstLine(redactEnv(string(out), env)))}
	}
	return emission{sent: true, note: "sent to Elk's suite history with `pulse suite-record`"}
}

// redactEnv removes the value of every credential-shaped variable in env
// from s, so a tool echoing its configuration cannot put a token in a
// deliverable.
func redactEnv(s string, env []string) string {
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || len(v) < 8 {
			continue
		}
		upper := strings.ToUpper(k)
		if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "KEY") ||
			strings.Contains(upper, "PASSWORD") || strings.HasSuffix(upper, "_URL") {
			s = strings.ReplaceAll(s, v, "<redacted>")
		}
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// recordDir is where one run's record and log live: outside the worktree,
// which is reaped as soon as the deliverable is saved.
func recordDir(root, runID string) (string, error) {
	dir := filepath.Join(root, safeName(runID))
	return dir, os.MkdirAll(dir, 0o700)
}

var unsafeRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string {
	s = strings.Trim(unsafeRE.ReplaceAllString(s, "-"), "-.")
	if s == "" {
		s = "run"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
