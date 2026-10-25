package check

import (
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// The two executors. Gitea's act_runner has the same pair, for the same
// reason: a Linux job wants a clean container with service containers on
// fixed ports, and a lab desktop's job wants the desktop.
const (
	// ExecutorContainer runs the job in a container on the Docker daemon the
	// environment names: `act -P <label>=<image>`.
	ExecutorContainer = "container"
	// ExecutorHost runs the job's steps on this machine itself:
	// `act -P <label>=-self-hosted`. What mac-01 and the Windows boxes need.
	ExecutorHost = "host"
)

// DefaultImage is the image a container check runs in when the queue's
// settings name none: catthehacker's medium Ubuntu image, the one act's own
// documentation recommends for GitHub-hosted parity.
const DefaultImage = "catthehacker/ubuntu:act-latest"

// DefaultTimeout bounds a check when neither the run nor the queue's settings
// say. A floor suite takes minutes; an hour is a hung job, not a slow one.
const DefaultTimeout = time.Hour

// Settings is a runner queue's `[queues.settings]` table, parsed.
type Settings struct {
	// Executor is container, host, or "" to choose by the job's labels.
	Executor string
	// Image is the container image for every mapped label.
	Image string
	// Labels are extra runs-on labels to map, beyond the packet's and the
	// defaults — for a workflow whose runs-on names a label nothing else
	// knows.
	Labels []string
	// Timeout bounds the act process; zero means the run's own, then
	// [DefaultTimeout].
	Timeout time.Duration
	// Event overrides the event every check on the queue runs as.
	Event string
}

var imageRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,254}$`)

// ParseSettings reads the queue's settings and refuses anything it does not
// understand: a typo in `executor` that quietly became the default would run
// a desktop's job in a container, or a container's job on a desktop.
func ParseSettings(raw map[string]string) (Settings, error) {
	var s Settings
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := strings.TrimSpace(raw[k])
		switch k {
		case "executor":
			switch v {
			case "", "auto":
			case ExecutorContainer, ExecutorHost:
				s.Executor = v
			default:
				return s, fmt.Errorf("settings.executor %q: want container, host or auto", v)
			}
		case "image":
			if v != "" && !imageRE.MatchString(v) {
				return s, fmt.Errorf("settings.image %q is not an image reference", v)
			}
			s.Image = v
		case "labels":
			for _, l := range splitList(v) {
				if !labelRE.MatchString(l) {
					return s, fmt.Errorf("settings.labels: %q is not a runner label", l)
				}
				s.Labels = append(s.Labels, l)
			}
		case "timeout":
			if v == "" {
				continue
			}
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return s, fmt.Errorf("settings.timeout %q: want a positive duration such as 30m", v)
			}
			s.Timeout = d
		case "event":
			if v != "" && !events[v] {
				return s, fmt.Errorf("settings.event %q: want workflow_dispatch, push, pull_request or schedule", v)
			}
			s.Event = v
		default:
			return s, fmt.Errorf("settings.%s is not a setting the check adapter has; it reads executor, image, labels, timeout and event", k)
		}
	}
	if s.Image == "" {
		s.Image = DefaultImage
	}
	return s, nil
}

// ChooseExecutor picks the executor for a job on labels: the queue's setting
// when it names one, else a container for a Linux job and the host for a
// macOS or Windows one. A job whose labels say nothing runs in a container on
// a Linux machine and on the host anywhere else.
func ChooseExecutor(s Settings, labels []string) string {
	if s.Executor != "" {
		return s.Executor
	}
	switch Family(labels) {
	case "linux":
		return ExecutorContainer
	case "macos", "windows":
		return ExecutorHost
	}
	if runtime.GOOS == "linux" {
		return ExecutorContainer
	}
	return ExecutorHost
}

// Labels mapped on every check, beyond the packet's own. A workflow's runs-on
// is often an expression — scout's quick-checks resolves `vars.LINUX_RUNNER`
// to `ubuntu-slim` outside GitHub — so mapping only the label Elk read would
// leave act falling back to its own default image, which fails the job at
// "Set up job" for a reason that has nothing to do with the code.
var (
	linuxLabels = []string{"ubuntu-latest", "ubuntu-slim", "ubuntu-24.04", "ubuntu-22.04", "linux", "linux-lab", "self-hosted"}
	hostLabels  = []string{
		"self-hosted", "ubuntu-latest", "ubuntu-slim", "ubuntu-24.04", "ubuntu-22.04", "linux", "linux-lab",
		"macos-latest", "macos-26", "macos-15", "macos", "mac-01", "mac-lab",
		"windows-latest", "windows", "win-01", "win-02", "win11", "win-lab", "ui-lab", "x64", "arm64",
	}
)

// platformArgs is act's `-P` list: every label the job could resolve to,
// mapped to the image (container) or to `-self-hosted` (host). The packet's
// labels come first, then the queue's, then the defaults; each once.
func platformArgs(executor, image string, packet, extra []string) []string {
	target := image
	defaults := linuxLabels
	if executor == ExecutorHost {
		target = "-self-hosted"
		defaults = hostLabels
	}
	seen := map[string]bool{}
	var out []string
	for _, group := range [][]string{packet, extra, defaults} {
		for _, l := range group {
			key := strings.ToLower(l)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, "-P", l+"="+target)
		}
	}
	return out
}

// listedJob is one row of `act --list`.
type listedJob struct {
	ID, Name string
}

// parseList reads `act -W <file> --list`: a fixed-width table whose header is
// `Stage  Job ID  Job name  Workflow name  Workflow file  Events`. Columns are
// sliced at the header's offsets, because a job name may contain spaces.
func parseList(out string) []listedJob {
	var jobs []listedJob
	idAt, nameAt, wfAt := -1, -1, -1
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if idAt < 0 {
			if strings.HasPrefix(line, "Stage") && strings.Contains(line, "Job ID") {
				idAt = strings.Index(line, "Job ID")
				nameAt = strings.Index(line, "Job name")
				wfAt = strings.Index(line, "Workflow name")
			}
			continue
		}
		if len(line) <= idAt || nameAt < 0 {
			continue
		}
		cut := func(from, to int) string {
			if from >= len(line) {
				return ""
			}
			if to < 0 || to > len(line) {
				to = len(line)
			}
			return strings.TrimSpace(line[from:to])
		}
		j := listedJob{ID: cut(idAt, nameAt), Name: cut(nameAt, wfAt)}
		if j.ID != "" {
			jobs = append(jobs, j)
		}
	}
	return jobs
}

// resolveJob finds the job a packet names among a workflow's jobs: by id, by
// id ignoring case, by name (spaces and hyphens alike), and — for a workflow
// with exactly one job — that job. note says how, when it was not by id.
func resolveJob(want string, jobs []listedJob) (id, note string, err error) {
	norm := func(s string) string {
		return strings.NewReplacer(" ", "-", "_", "-").Replace(strings.ToLower(strings.TrimSpace(s)))
	}
	for _, j := range jobs {
		if j.ID == want {
			return j.ID, "", nil
		}
	}
	for _, j := range jobs {
		if strings.EqualFold(j.ID, want) {
			return j.ID, fmt.Sprintf("the packet named job %q; the workflow's id is %q", want, j.ID), nil
		}
	}
	for _, j := range jobs {
		if norm(j.Name) == norm(want) {
			return j.ID, fmt.Sprintf("the packet named job %q, which is the name of job %q", want, j.ID), nil
		}
	}
	if len(jobs) == 1 {
		return jobs[0].ID, fmt.Sprintf("the packet named job %q, which the workflow does not have; it has one job, %q, and that ran", want, jobs[0].ID), nil
	}
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return "", "", fmt.Errorf("the workflow has no job %q; its jobs are %s", want, strings.Join(ids, ", "))
}

// ansiRE matches terminal control sequences. act runs a host step under a
// pseudo-terminal, so git and node colour their output even into a pipe.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[=>()][0-9A-Za-z]?`)

// clean strips control sequences and carriage returns from one line.
func clean(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	if i := strings.LastIndex(s, "\r"); i >= 0 && i < len(s)-1 {
		s = s[i+1:] // a progress line redrawn in place: keep what was last drawn
	}
	return strings.TrimRight(strings.ReplaceAll(s, "\r", ""), " \t")
}

// actRecord is the part of one `act --json` log record Rein reads.
type actRecord struct {
	Msg        string `json:"msg"`
	Level      string `json:"level"`
	RawOutput  bool   `json:"raw_output"`
	Stage      string `json:"stage"`
	Step       string `json:"step"`
	StepResult string `json:"stepResult"`
	JobResult  string `json:"jobResult"`
}

// tally is what the log said about the job.
type tally struct {
	jobResult   string // success | failure, from act's 🏁 line; "" when act never got there
	setupFailed bool   // act could not set the job up: Docker, the image, an unmapped label
	failedStep  string // the first step that failed, for the deliverable
	steps       int
	passed      int
	failed      int
	skipped     int
}

// render turns one line of act's output into the lines a person reads, and
// folds what it says into t. A line that is not act's JSON is passed through:
// act prints its warnings and its final error as plain text.
func (t *tally) render(line string) []string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		if c := clean(line); strings.TrimSpace(c) != "" {
			return []string{c}
		}
		return nil
	}
	var r actRecord
	if err := json.Unmarshal([]byte(trimmed), &r); err != nil {
		return []string{clean(line)}
	}
	if r.JobResult != "" {
		t.jobResult = r.JobResult
	}
	if r.StepResult != "" {
		switch {
		case r.Stage == "":
			// Set up job / Complete job: act's own steps, not the workflow's.
			if r.StepResult == "failure" && strings.EqualFold(r.Step, "Set up job") {
				t.setupFailed = true
			}
		default:
			t.steps++
			switch r.StepResult {
			case "success":
				t.passed++
			case "failure":
				t.failed++
				if t.failedStep == "" {
					t.failedStep = r.Step
				}
			case "skipped":
				t.skipped++
			}
		}
	}
	var out []string
	for _, l := range strings.Split(strings.TrimRight(r.Msg, "\n"), "\n") {
		l = clean(l)
		if strings.TrimSpace(l) == "" {
			continue
		}
		if r.RawOutput {
			l = "  | " + l
		}
		out = append(out, l)
	}
	return out
}

// counts is the record's `counts`: step counts from act's log when any step
// ran, else empty. They are named for steps, never pass/fail, because a
// workflow step is not a test and the record must not invent test counts.
func (t *tally) counts() map[string]int {
	if t.steps == 0 {
		return map[string]int{}
	}
	c := map[string]int{"steps": t.steps, "steps_passed": t.passed, "steps_failed": t.failed}
	if t.skipped > 0 {
		c["steps_skipped"] = t.skipped
	}
	return c
}
