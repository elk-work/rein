package check

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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

// write saves the record as indented JSON, 0600.
func (r SuiteRecord) write(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
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
