// Package check runs a CI check as a Rein run: the agent kind `runner`.
//
// A check is one job of one workflow file, run at one exact commit, with
// nektos/act, outside GitHub. Elk dispatches it like any other run — a packet
// on a queue — and the run loop treats it like any other adapter's session:
// claim, capability checks, preflight, worktree, progress, submit, reap all
// stay as they are. What differs is what executes in the worktree: act, not a
// coding agent. The ruling is Elk Scout #1163 (a), 2026-10-18: Rein takes the
// check run kind, and there is no separate runner binary. The plan is elk
// docs/milestones/elk-ci.md §5.2, Phase B.
//
// The packet (see [ParsePacket]) names the repository, the commit, the
// workflow file, the job, its runs-on labels, the suite it is recorded as and
// the provider. [Adapter.Prepare] hands the commit to the run loop, which cuts
// the worktree detached at exactly that commit (internal/worktree). Then:
//
//	act <event> -W <workflow> -j <job> --json --rm -P <label>=<image>          # container executor
//	act <event> -W <workflow> -j <job> --json --rm -P <label>=-self-hosted     # host executor
//
// Every line act prints becomes an [adapter.EventProgress], which the run loop
// throttles exactly as it throttles an agent's. When act exits, Rein writes the
// suite record (pulse/docs/suite-records.md, schema_version 1) outside the
// worktree, sends it with `pulse suite-record` when it can, and finishes with a
// deliverable naming the result, the record and the last 40 lines of the log,
// and ending with the record itself for Elk to forward (see [recordBlock]).
//
// A job that ran and failed is a finished run whose deliverable says fail —
// the session succeeded at its job, which was to find out. Only a harness
// fault is a failure of the session: act missing, the commit unfetchable,
// Docker not answering, act unable to set the job up.
//
// See docs/adapter-check.md.
package check

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
)

// Kind is the agent kind this adapter registers under. Elk's `agent_kind`
// enum spells it the same way.
const Kind = "runner"

func init() { adapter.MustRegister(New()) }

// Adapter runs checks with act. The zero value is not usable; call [New].
type Adapter struct {
	// Act, Git, Docker and Pulse override the executables. Empty means the
	// name looked up on PATH: act, git, docker, pulse.
	Act, Git, Docker, Pulse string

	// RecordRoot is where each run's record and log are kept. Empty means
	// `<Rein home>/checks`. The oldest are removed at the end of each run,
	// keeping as many as log_keep_runs (see [Adapter.ReapRun]).
	RecordRoot string

	// CacheRoot is act's own cache directory, where it clones actions and
	// makes a scratch directory for every host-executor job. Empty means
	// where act itself puts it, read from the environment Rein hands act:
	// `$XDG_CACHE_HOME/act`, else `~/.cache/act` (see [actCacheRoot]). Only
	// the scratch directories are ever removed (see [Adapter.ReapRun]).
	CacheRoot string

	// Heartbeat is how long act may print nothing before the session says it
	// is still running. The run loop treats fifteen silent minutes as a
	// stall, and a compile step is often silent for a while. Zero means one
	// minute.
	Heartbeat time.Duration

	// InterruptGrace is how long an interrupted act gets to tear its
	// containers down before it is killed. Zero means ten seconds.
	InterruptGrace time.Duration

	// TailLines is how many lines of the log the deliverable quotes. Zero
	// means 40.
	TailLines int
}

// New returns an adapter that finds its binaries on PATH.
func New() *Adapter { return &Adapter{} }

// Name implements [adapter.Adapter].
func (a *Adapter) Name() string { return Kind }

// Manifest implements [adapter.Adapter].
//
// The runtime half only: what the session can do. A check reads and runs a
// workflow in its worktree, through git and a shell, and reports through
// act's JSON log; it edits nothing, takes no MCP servers, cannot be resumed
// or steered, and asks nobody for permission. What the MACHINE has — docker,
// act, xcode — is the environment half, established by the host probes in
// internal/runner/hostcap.go and declared to Elk beside `agent:runner`,
// which the run loop derives from this manifest's kind.
func (a *Adapter) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Kind:   Kind,
		Binary: "act",
		Capabilities: map[adapter.Capability]adapter.Support{
			adapter.CapGit:              adapter.SupportYes,
			adapter.CapWorktree:         adapter.SupportYes,
			adapter.CapShell:            adapter.SupportYes,
			adapter.CapStructuredEvents: adapter.SupportYes,
			adapter.CapFileEdit:         adapter.SupportNo,
			adapter.CapMCP:              adapter.SupportNo,
			adapter.CapResume:           adapter.SupportNo,
			adapter.CapApprovals:        adapter.SupportNo,
			adapter.CapSteer:            adapter.SupportNo,
		},
		Platforms: []adapter.Platform{adapter.AnyArch("darwin"), adapter.AnyArch("linux"), adapter.AnyArch("windows")},
		Notes: "Runs one workflow job with nektos/act at the packet's commit. The container executor needs a " +
			"reachable Docker daemon; the host executor runs the steps on the machine itself.",
	}
}

func (a *Adapter) bin(override, name string) string {
	if override != "" {
		return override
	}
	return name
}

// look finds a binary, as a preflight error a person can act on.
func (a *Adapter) look(override, name, why string) (string, error) {
	p, err := exec.LookPath(a.bin(override, name))
	if err != nil {
		return "", fmt.Errorf("%w: %s is not on PATH (%s): %w", adapter.ErrPreflight, name, why, err)
	}
	return p, nil
}

// Preflight implements [adapter.Adapter]: git and act on PATH. Whether Docker
// is needed depends on the packet, so the run loop calls [Adapter.Prepare],
// which checks it, in place of this; `rein status` calls this.
func (a *Adapter) Preflight(ctx context.Context) error {
	if _, err := a.look(a.Git, "git", "the check's worktree and its record need it"); err != nil {
		return err
	}
	if _, err := a.look(a.Act, "act", "install it with `brew install act`, or see https://nektosact.com"); err != nil {
		return err
	}
	return nil
}

// Prepare implements [adapter.RunPreparer]. Before anything exists on disk it
// reads the packet out of the work order, checks the queue's settings,
// confirms git and act, and — when the job will run in a container —
// confirms that the Docker daemon answers. It hands back the commit the
// worktree is cut at.
func (a *Adapter) Prepare(ctx context.Context, spec adapter.RunSpec) (adapter.Prepared, error) {
	if spec.Hosted {
		return adapter.Prepared{}, fmt.Errorf("%w: a hosted Rein does not run checks yet", adapter.ErrPreflight)
	}
	c, err := packetOf(spec.Prompt)
	if err != nil {
		return adapter.Prepared{}, err
	}
	s, err := ParseSettings(spec.Settings)
	if err != nil {
		return adapter.Prepared{}, fmt.Errorf("%w: queue %s: %w", adapter.ErrPreflight, spec.Queue, err)
	}
	if err := a.Preflight(ctx); err != nil {
		return adapter.Prepared{}, err
	}
	if ChooseExecutor(s, c.RunsOn) == ExecutorContainer {
		if err := a.dockerAnswers(ctx, a.env(spec)); err != nil {
			return adapter.Prepared{}, err
		}
	}
	return adapter.Prepared{Commit: c.SHA}, nil
}

// packetOf parses and validates the work order's check packet. A missing or
// malformed packet is the work order's fault, not the machine's.
func packetOf(prompt string) (Check, error) {
	c, err := ParsePacket(prompt)
	if errors.Is(err, ErrNoPacket) {
		return c, fmt.Errorf("%w: this run came to a runner queue without a check packet (a `### Check` block "+
			"naming repo, sha, workflow and job); a runner runs checks, not tasks", adapter.ErrWorkOrder)
	}
	if err != nil {
		return c, fmt.Errorf("%w: %w", adapter.ErrWorkOrder, err)
	}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("%w: %w", adapter.ErrWorkOrder, err)
	}
	return c, nil
}

// dockerAnswers asks the daemon the environment names whether it is up.
func (a *Adapter) dockerAnswers(ctx context.Context, env []string) error {
	docker, err := a.look(a.Docker, "docker", "the container executor talks to the Docker daemon through it")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, docker, "info", "--format", "{{.ServerVersion}}")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: the Docker daemon is not answering (`docker info`: %s). This check runs in a container; "+
			"start Docker on this machine, or set executor = \"host\" in the queue's settings to run it on the host",
			adapter.ErrPreflight, firstLine(string(out)))
	}
	return nil
}

// dockerVariables locate the daemon. They are configuration, not credentials,
// and the run loop's scoped environment does not carry them, so the adapter
// passes the daemon's own along unless the queue's environment sets them.
var dockerVariables = []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"}

// strideVariables are pulse's, and only pulse's: the record's emitter needs
// them and the workflow's own steps never should, so they are kept out of
// act's environment, where the host executor would hand them to every step.
var strideVariables = map[string]bool{"STRIDE_INGEST_URL": true, "STRIDE_TOKEN": true}

// githubTokens reach a check only from the queue's scoped secrets map — a
// token the daemon happens to inherit is not one the queue granted.
var githubTokens = map[string]bool{"GITHUB_TOKEN": true, "GH_TOKEN": true}

// env is act's environment: the run's inherited environment, the daemon's
// Docker location, the queue's scoped secrets, and a pager that never pages.
// Stride's variables are left out (they are pulse's; see [Adapter.pulseEnv]),
// and so is a GitHub token the queue's secrets did not provide.
func (a *Adapter) env(spec adapter.RunSpec) []string {
	set := map[string]string{}
	var order []string
	put := func(k, v string) {
		if strideVariables[k] {
			return
		}
		if _, ok := set[k]; !ok {
			order = append(order, k)
		}
		set[k] = v
	}
	for _, kv := range spec.InheritedEnv() {
		if k, v, ok := strings.Cut(kv, "="); ok && !githubTokens[k] {
			put(k, v)
		}
	}
	for _, k := range dockerVariables {
		if v, ok := os.LookupEnv(k); ok {
			put(k, v)
		}
	}
	for k, v := range spec.Env {
		put(k, v)
	}
	put("GIT_PAGER", "cat")
	put("PAGER", "cat")
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+set[k])
	}
	return out
}

// pulseEnv is `pulse suite-record`'s environment: the run's inherited
// environment and the queue's Stride variables, nothing else of its secrets.
func (a *Adapter) pulseEnv(spec adapter.RunSpec) []string {
	out := spec.InheritedEnv()
	for k, v := range spec.Env {
		if strideVariables[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// secretValues is every value the run must never print: the queue's scoped
// secrets, and any Stride or GitHub variable it inherits.
func secretValues(spec adapter.RunSpec) map[string]string {
	out := map[string]string{}
	for k, v := range spec.Env {
		out[k] = v
	}
	for _, kv := range spec.InheritedEnv() {
		if k, v, ok := strings.Cut(kv, "="); ok && (strideVariables[k] || githubTokens[k]) {
			out[k] = v
		}
	}
	return out
}

// recordRoot is where this adapter keeps records.
func (a *Adapter) recordRoot() (string, error) {
	if a.RecordRoot != "" {
		return a.RecordRoot, nil
	}
	home, err := config.Dir()
	if err != nil {
		return "", err
	}
	return home + string(os.PathSeparator) + "checks", nil
}
