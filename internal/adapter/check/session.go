package check

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/secretenv"
)

// Start implements [adapter.Adapter]: it checks the worktree is the commit
// the packet names, resolves the job, and starts act. It returns once act is
// running; the session reports everything else as events.
func (a *Adapter) Start(ctx context.Context, spec adapter.RunSpec) (adapter.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Hosted {
		return nil, errors.New("check: a hosted Rein does not run checks yet")
	}
	if spec.ResumeID != "" {
		return nil, fmt.Errorf("check: a check cannot be resumed; %w", adapter.ErrNotSupported)
	}
	c, err := packetOf(spec.Prompt)
	if err != nil {
		return nil, err
	}
	st, err := ParseSettings(spec.Settings)
	if err != nil {
		return nil, fmt.Errorf("check: queue %s: %w", spec.Queue, err)
	}
	if st.Event != "" {
		c.Event = st.Event
	}
	act, err := a.look(a.Act, "act", "install it with `brew install act`")
	if err != nil {
		return nil, err
	}
	git, err := a.look(a.Git, "git", "the check's worktree needs it")
	if err != nil {
		return nil, err
	}
	env := a.env(spec)
	dir := spec.WorktreeDir

	// The worktree must be the commit, exactly: the run loop cut it there, and
	// a record that names a commit it did not test is worse than none.
	if head := gitOut(ctx, git, dir, "rev-parse", "HEAD"); head != c.SHA {
		return nil, fmt.Errorf("check: the worktree is at %q, not the check's commit %s", head, c.SHA)
	}
	origin := gitOut(ctx, git, dir, "remote", "get-url", "origin")
	if c.Repo == "" {
		c.Repo = originRepo(origin)
	} else if got := originRepo(origin); got != "" && !strings.EqualFold(got, c.Repo) {
		return nil, fmt.Errorf("check: the packet's repository is %s, but this queue's checkout of it is %s", c.Repo, got)
	}
	if c.Repo == "" {
		return nil, errors.New("check: the packet names no repository and the checkout's origin is not a GitHub repository")
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(c.Workflow))); err != nil {
		return nil, fmt.Errorf("check: %s does not exist at %s", c.Workflow, c.SHA)
	}
	jobs, err := listJobs(ctx, act, dir, c.Workflow, env)
	if err != nil {
		return nil, err
	}
	jobID, jobNote, err := resolveJob(c.Job, jobs)
	if err != nil {
		return nil, fmt.Errorf("check: %s: %w", c.Workflow, err)
	}

	executor := ChooseExecutor(st, c.RunsOn)
	args := []string{c.Event, "-W", c.Workflow, "-j", jobID, "--json"}
	args = append(args, platformArgs(executor, st.Image, c.RunsOn, st.Labels)...)
	if executor == ExecutorContainer {
		// A worktree's .git is a file pointing into the source repository,
		// which act's copy of the worktree does not carry into the container.
		// Mounting the repository's git directory, read-only and at the same
		// path, lets a step's `git diff` or `git grep` work as it does on
		// GitHub.
		if common := gitOut(ctx, git, dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); common != "" &&
			!strings.ContainsAny(common, " \t\"'") {
			args = append(args, "--container-options", "-v "+common+":"+common+":ro")
		}
		if a.imagePresent(ctx, st.Image, env) {
			args = append(args, "--pull=false")
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") && len(kv) > len("GITHUB_TOKEN=") {
			// By name only: act reads the value from its environment, so the
			// token is never on a command line.
			args = append(args, "-s", "GITHUB_TOKEN")
		}
	}

	timeout := DefaultTimeout
	if st.Timeout > 0 {
		timeout = st.Timeout
	}
	if t := spec.Timeouts.Total; t > 0 && t < timeout {
		timeout = t
	}
	root, err := a.recordRoot()
	if err != nil {
		return nil, fmt.Errorf("check: locating the record directory: %w", err)
	}
	recDir, err := recordDir(root, spec.RunID)
	if err != nil {
		return nil, fmt.Errorf("check: creating the record directory: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(recDir, "act.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("check: opening the log: %w", err)
	}

	cmd := exec.Command(act, args...)
	cmd.Dir = dir
	cmd.Env = env
	setProcAttr(cmd)
	pr, pw, err := os.Pipe()
	if err != nil {
		logFile.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	started := time.Now()
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		logFile.Close()
		return nil, fmt.Errorf("check: starting act: %w", err)
	}
	pw.Close()

	s := &session{
		a: a, spec: spec, check: c, jobID: jobID, jobNote: jobNote, executor: executor, image: st.Image,
		cmd: cmd, out: pr, log: logFile, recDir: recDir, git: git, env: env, timeout: timeout,
		pulseEnv: a.pulseEnv(spec), redact: secretenv.NewRedactor(secretValues(spec)),
		started: started, events: make(chan adapter.Event, 64), done: make(chan struct{}),
		stop: make(chan struct{}),
	}
	fmt.Fprintf(logFile, "$ act %s\n", strings.Join(args, " "))
	go s.run()
	return s, nil
}

// session is one act process.
type session struct {
	a        *Adapter
	spec     adapter.RunSpec
	check    Check
	jobID    string
	jobNote  string
	executor string
	image    string
	git      string
	env      []string
	timeout  time.Duration
	started  time.Time

	// pulseEnv is the emitter's environment, which alone holds Stride's
	// variables; redact takes every secret value out of a line before the
	// log, a progress event or the deliverable sees it — a workflow step can
	// print its environment, and on the host executor that environment is
	// act's own.
	pulseEnv []string
	redact   *secretenv.Redactor

	cmd    *exec.Cmd
	out    *os.File
	log    *os.File
	recDir string

	events chan adapter.Event
	seq    uint64

	stopOnce    sync.Once
	stop        chan struct{}
	interrupted bool // guarded by mu
	mu          sync.Mutex

	done   chan struct{}
	result adapter.Result
	err    error
}

func (s *session) ID() string                   { return "" }
func (s *session) Events() <-chan adapter.Event { return s.events }

func (s *session) Send(context.Context, string) error {
	return fmt.Errorf("check: a check takes no input once it has started: %w", adapter.ErrNotSupported)
}

func (s *session) Respond(context.Context, adapter.PermissionResponse) error {
	return fmt.Errorf("check: a check asks for no permissions: %w", adapter.ErrNotSupported)
}

// Interrupt asks act to stop — SIGINT to its process group, so it can take
// its containers down — and kills it after the grace period.
func (s *session) Interrupt(context.Context) error {
	select {
	case <-s.done:
		return adapter.ErrSessionClosed
	default:
	}
	s.mu.Lock()
	s.interrupted = true
	s.mu.Unlock()
	s.stopOnce.Do(func() { close(s.stop) })
	return nil
}

func (s *session) Wait() (adapter.Result, error) {
	<-s.done
	return s.result, s.err
}

func (s *session) emit(ev adapter.Event) {
	s.seq++
	ev.Seq = s.seq
	ev.At = time.Now()
	s.events <- ev
}

func (s *session) grace() time.Duration {
	if s.a.InterruptGrace > 0 {
		return s.a.InterruptGrace
	}
	return 10 * time.Second
}

func (s *session) heartbeat() time.Duration {
	if s.a.Heartbeat > 0 {
		return s.a.Heartbeat
	}
	return time.Minute
}

func (s *session) tailLines() int {
	if s.a.TailLines > 0 {
		return s.a.TailLines
	}
	return 40
}

// run reads act until it exits, then records and reports the outcome.
func (s *session) run() {
	defer close(s.events)
	defer close(s.done)
	defer s.log.Close()

	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		r := bufio.NewReaderSize(s.out, 64*1024)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				lines <- strings.TrimRight(line, "\r\n")
			}
			if err != nil {
				return
			}
		}
	}()
	waited := make(chan error, 1)
	go func() { waited <- s.cmd.Wait() }()

	s.emit(adapter.Event{Kind: adapter.EventProgress, Text: fmt.Sprintf(
		"Running the %s job from %s with act (%s executor) at the check's commit.", s.jobID, s.check.Workflow, s.executor)})

	var (
		t        tally
		tail     []string
		count    int
		lastLine = time.Now()
		timedOut bool
		exitErr  error
		exited   bool
		deadline = time.NewTimer(s.timeout)
		beat     = time.NewTicker(s.heartbeat())
		killAt   <-chan time.Time
		drainBy  <-chan time.Time
		stopC    = s.stop
	)
	defer deadline.Stop()
	defer beat.Stop()
	stopping := func() {
		_ = interruptProcess(s.cmd)
		killAt = time.After(s.grace())
	}
	for lines != nil {
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			for _, l := range t.render(line) {
				l = s.redact.String(l)
				count++
				lastLine = time.Now()
				fmt.Fprintln(s.log, l)
				if s.noise(l) {
					continue // in the log, but not worth a reader's line
				}
				tail = append(tail, l)
				if len(tail) > s.tailLines() {
					tail = tail[1:]
				}
				s.emit(adapter.Event{Kind: adapter.EventProgress, Text: truncate(l, 500)})
			}
		case <-beat.C:
			if time.Since(lastLine) >= s.heartbeat() {
				s.emit(adapter.Event{Kind: adapter.EventProgress, Text: fmt.Sprintf(
					"act is still running the %s job: %d lines so far, the last %s ago.",
					s.jobID, count, time.Since(lastLine).Round(time.Second))})
			}
		case <-deadline.C:
			timedOut = true
			fmt.Fprintf(s.log, "rein: the check ran past its %s limit; stopping act\n", s.timeout)
			stopping()
		case <-stopC:
			stopC = nil
			fmt.Fprintln(s.log, "rein: interrupted; stopping act")
			stopping()
		case <-killAt:
			_ = killProcess(s.cmd)
			killAt = nil
		case exitErr = <-waited:
			exited = true
			waited = nil
			// A grandchild may still hold the pipe open after act has gone;
			// give the reader a moment to finish, then stop waiting for it.
			drainBy = time.After(2 * time.Second)
		case <-drainBy:
			s.out.Close()
			drainBy = nil
		}
	}
	if !exited {
		exitErr = <-waited
	}
	s.out.Close()
	ended := time.Now()

	s.mu.Lock()
	interrupted := s.interrupted
	s.mu.Unlock()

	exitCode := 0
	var ee *exec.ExitError
	if errors.As(exitErr, &ee) {
		exitCode = ee.ExitCode()
	} else if exitErr != nil {
		exitCode = -1
	}

	result, why := classify(t, exitErr, timedOut, interrupted, s.timeout)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	branch := verifiedBranch(ctx, s.git, s.spec.WorktreeDir, s.check.Ref, s.check.SHA)
	rec := newRecord(s.check, s.spec.Queue, branch, result, &t, s.started, ended)
	recPath := filepath.Join(s.recDir, "suite-record.json")
	em := emission{note: "kept on this machine"}
	if err := rec.write(recPath); err != nil {
		em = emission{note: "NOT written: " + err.Error()}
	} else if !interrupted {
		em = emit(ctx, s.lookPulse(), recPath, s.pulseEnv)
	}

	status := adapter.StatusSucceeded
	switch {
	case interrupted:
		status = adapter.StatusInterrupted
	case timedOut:
		status = adapter.StatusTimedOut
	case result == ResultError:
		status = adapter.StatusFailed
	}
	summary := s.deliverable(rec, result, why, &t, em, recPath, filepath.Join(s.recDir, "act.log"), tail)
	s.result = adapter.Result{
		Status: status, Summary: summary, ExitCode: exitCode, Duration: ended.Sub(s.started),
		TranscriptPath: filepath.Join(s.recDir, "act.log"),
	}
	if status != adapter.StatusSucceeded {
		s.err = fmt.Errorf("check: %s: %s", result, why)
	}
	s.emit(adapter.Event{Kind: adapter.EventDone, Result: &s.result})
}

// noise is act's own chatter that says nothing about the job: the
// Apple-silicon architecture hint on every run, and — on the host executor,
// which never uses Docker — its complaint that it found no Docker.
func (s *session) noise(line string) bool {
	if strings.Contains(line, "You are using Apple M-series chip") {
		return true
	}
	return s.executor == ExecutorHost && strings.Contains(line, "get a valid docker connection")
}

func (s *session) lookPulse() string {
	p, err := exec.LookPath(s.a.bin(s.a.Pulse, "pulse"))
	if err != nil {
		return ""
	}
	return p
}

// classify turns what act said into the record's result and a sentence
// saying why. A job that ran and failed is fail; act that never got the job
// going — no 🏁 line, or a failed "Set up job" — is error, a harness fault.
func classify(t tally, exitErr error, timedOut, interrupted bool, limit time.Duration) (result, why string) {
	switch {
	case interrupted:
		return ResultCancelled, "the run was stopped before the job finished"
	case timedOut:
		return ResultError, fmt.Sprintf("the job ran past its %s limit and was stopped", limit)
	case t.setupFailed:
		return ResultError, "act could not set the job up (usually Docker, the image, or a runs-on label with no mapping)"
	case t.jobResult == "success" && exitErr == nil:
		return ResultPass, "the job succeeded"
	case t.jobResult == "failure":
		if t.failedStep != "" {
			return ResultFail, fmt.Sprintf("the step %q failed", t.failedStep)
		}
		return ResultFail, "the job failed"
	case t.jobResult == "success":
		return ResultError, fmt.Sprintf("act reported the job succeeded but exited with %v", exitErr)
	case exitErr != nil:
		return ResultError, fmt.Sprintf("act exited (%v) without running the job", exitErr)
	}
	return ResultError, "act exited without saying how the job ended"
}

// deliverable is what a person reads in Elk: a plain lead, the check, the
// record, and the end of the log.
func (s *session) deliverable(rec SuiteRecord, result, why string, t *tally, em emission,
	recPath, logPath string, tail []string) string {

	var b strings.Builder
	name := s.check.Suite
	repo := rec.Repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repo = repo[i+1:]
	}
	switch result {
	case ResultPass:
		fmt.Fprintf(&b, "The %s check passed on %s. Rein ran the workflow job on this machine at the commit Elk asked for, and recorded the result.\n", name, repo)
	case ResultFail:
		fmt.Fprintf(&b, "The %s check failed on %s: %s. Rein ran the workflow job on this machine at the commit Elk asked for, and recorded the failure.\n", name, repo, why)
	case ResultCancelled:
		fmt.Fprintf(&b, "The %s check on %s was stopped before it finished, so it has no result.\n", name, repo)
	default:
		fmt.Fprintf(&b, "The %s check on %s could not be run on this machine, so it says nothing about the code: %s. It is recorded as an error, not a failure.\n", name, repo, why)
	}

	b.WriteString("\n### Check\n\n")
	fmt.Fprintf(&b, "- Result: **%s** (%s)\n", result, why)
	fmt.Fprintf(&b, "- Repository: %s at `%s`", rec.Repo, rec.SHA)
	if s.check.Ref != "" {
		fmt.Fprintf(&b, " (%s)", s.check.Ref)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "- Job: `%s` from `%s`, as `%s`", s.jobID, s.check.Workflow, s.check.Event)
	if s.jobNote != "" {
		fmt.Fprintf(&b, " — %s", s.jobNote)
	}
	b.WriteString("\n")
	if s.executor == ExecutorContainer {
		fmt.Fprintf(&b, "- Executor: container, image `%s`\n", s.image)
	} else {
		b.WriteString("- Executor: host — the steps ran on this machine itself\n")
	}
	fmt.Fprintf(&b, "- Suite: %s · provider %s · runner %s\n", rec.Suite, rec.Provider, rec.Runner)
	if t.steps > 0 {
		fmt.Fprintf(&b, "- Steps: %d ran, %d failed\n", t.steps, t.failed)
	}
	fmt.Fprintf(&b, "- Duration: %s\n", time.Duration(rec.DurationMS)*time.Millisecond)

	b.WriteString("\n### Suite record\n\n")
	fmt.Fprintf(&b, "- `%s` — %s\n", recPath, em.note)
	fmt.Fprintf(&b, "- Full log: `%s`\n\n", logPath)
	if j, err := json.Marshal(rec); err == nil {
		b.WriteString("```json\n" + string(j) + "\n```\n")
	}

	fmt.Fprintf(&b, "\n### Last %d lines of the log\n\n", len(tail))
	b.WriteString("```text\n")
	for _, l := range tail {
		b.WriteString(strings.ReplaceAll(l, "```", "'''") + "\n")
	}
	b.WriteString("```\n")
	return b.String()
}

// listJobs asks act which jobs the workflow has.
func listJobs(ctx context.Context, act, dir, workflow string, env []string) ([]listedJob, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, act, "-W", workflow, "--list")
	cmd.Dir = dir
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("check: act could not read %s: %v: %s", workflow, err, firstLine(lastError(stderr.String())))
	}
	jobs := parseList(string(out))
	if len(jobs) == 0 {
		return nil, fmt.Errorf("check: act found no jobs in %s", workflow)
	}
	return jobs, nil
}

// lastError picks act's "Error: …" line out of its stderr, which is
// otherwise warnings.
func lastError(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "Error:") {
			return lines[i]
		}
	}
	if len(lines) > 0 {
		return lines[len(lines)-1]
	}
	return ""
}

// imagePresent reports whether the daemon already holds the image, in which
// case act is told not to pull it.
func (a *Adapter) imagePresent(ctx context.Context, image string, env []string) bool {
	docker, err := exec.LookPath(a.bin(a.Docker, "docker"))
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, docker, "image", "inspect", "--format", "{{.Id}}", image)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run() == nil
}

func gitOut(ctx context.Context, git, dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, git, append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// originRepo is owner/name of a GitHub remote URL, or "".
func originRepo(remote string) string {
	r := strings.TrimSpace(remote)
	switch {
	case strings.HasPrefix(r, "git@github.com:"):
		r = strings.TrimPrefix(r, "git@github.com:")
	case strings.HasPrefix(r, "https://github.com/"):
		r = strings.TrimPrefix(r, "https://github.com/")
	case strings.HasPrefix(r, "ssh://git@github.com/"):
		r = strings.TrimPrefix(r, "ssh://git@github.com/")
	default:
		return ""
	}
	r = strings.TrimSuffix(strings.TrimSuffix(r, "/"), ".git")
	if !repoRE.MatchString(r) {
		return ""
	}
	return r
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
