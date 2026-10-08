// Package service installs Rein as a **user** service and controls it:
// launchd on macOS, a Windows service, `systemd --user` on Linux, all through
// github.com/kardianos/service.
//
// It is a user service and not a system daemon for one reason that decides
// almost every other choice in this file: **the run loop must be the
// developer.** Its Elk tokens are in that person's keychain, the coding-agent
// CLIs it drives are logged in as that person, `gh` is authenticated as that
// person, and the repositories it cuts worktrees from are under that person's
// home. A LaunchDaemon or a LocalSystem service is a different user, and every
// one of those falls over.
//
// Three consequences worth knowing before reading the code:
//
//   - **`SessionCreate` stays false on macOS.** It is launchd's "start this job
//     in a new security session", and a new security session is one with no
//     access to the login keychain. Turning it on is exactly the way to make
//     `security find-generic-password` — which is how go-keyring reads a token
//     on darwin — fail from a service that works fine in a terminal.
//
//   - **PATH is captured at install time.** A launchd agent inherits
//     `/usr/bin:/bin:/usr/sbin:/sbin` and nothing else, so `claude`, `codex`,
//     `grok` and `gh` are all invisible to it. The installer writes the
//     installing shell's PATH into the service definition. The cost is that a
//     tool installed somewhere new afterwards needs a reinstall, which
//     `rein service install` says out loud.
//
//   - **The service runs `rein run` with no `--once`.** All configured queues,
//     daemon mode, the same config file and the same keychain as the person who
//     installed it.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/runner"
	kservice "github.com/kardianos/service"
)

// Name identifies the service on every platform: the launchd `Label` and
// `~/Library/LaunchAgents/<Name>.plist`, the systemd unit
// `~/.config/systemd/user/<Name>.service`, and the Windows service name.
//
// Reverse-DNS because launchd labels are, and one name everywhere because a
// per-platform name is one more thing to be wrong about in a runbook.
const Name = "work.elk.rein"

// DisplayName and Description are what a service list shows a person.
const (
	DisplayName = "Rein (Elk runner)"
	Description = "Claims dispatched Elk runs and drives a coding agent against each one."
)

// LogDirName is the subdirectory of the Rein home the service's logs go in.
// Everything Rein owns lives under one directory (see internal/config), and
// logs are no exception.
const LogDirName = "log"

// LoopLogName is the run loop's own log inside [LogDirName]. It is passed to
// `rein run --log-file`, which is what makes the Windows service legible at
// all: a Windows service has no stdout to redirect.
const LoopLogName = "rein.log"

// Passthrough are the environment variables carried from the installing shell
// into the service, on top of PATH and HOME.
//
// Deliberately short, and deliberately not "everything": a service definition
// is a world-readable file on macOS and a registry value on Windows, so
// anything copied into it stops being a secret. None of these three is one —
// they are a directory, a backend name and a list of capability names.
var Passthrough = []string{"REIN_HOME", "REIN_KEYRING", "REIN_HOST_CAPABILITIES"}

// ErrNotInstalled reports that no service is installed. It wraps the
// underlying library's sentinel so callers need not import it.
var ErrNotInstalled = kservice.ErrNotInstalled

// Config describes the service to install. The zero value is usable: it
// installs the running binary against the default Rein home.
type Config struct {
	// Home is the Rein home the service will use — `$REIN_HOME`, else
	// `~/.rein`. Logs go under <Home>/log.
	Home string

	// ConfigPath, when set, is passed through as `rein run --config <path>`.
	// Empty means the default, which is what almost every install wants.
	ConfigPath string

	// Executable is the binary to install. Empty means the running one,
	// resolved through any symlinks so the service definition points at the
	// file rather than at a link that may be repointed later.
	Executable string

	// UserName is the account the service logs on as. It is Windows-only and
	// Windows-required: Credential Manager is per-user, so a service running
	// as LocalSystem cannot read the token `rein enrol` stored. On macOS and
	// Linux the service is already the installing user's.
	UserName string

	// Password is that account's password, needed by the Windows SCM to create
	// a service that logs on as a named user. Never written to the config file
	// and never logged.
	Password string

	// Env adds or overrides environment entries in the service definition.
	Env map[string]string

	// Args overrides the arguments the service runs the binary with. Empty
	// means the default — `run --service --log-file <Home>/log/rein.log` — and
	// nothing outside tests should need to set it.
	Args []string
}

// Manager installs and controls one Rein service.
type Manager struct {
	svc    kservice.Service
	kcfg   *kservice.Config
	home   string
	logDir string
}

// New builds a [Manager] for cfg.
func New(cfg Config) (*Manager, error) {
	home := strings.TrimSpace(cfg.Home)
	if home == "" {
		return nil, errors.New("service: no Rein home")
	}
	logDir := filepath.Join(home, LogDirName)

	exe := strings.TrimSpace(cfg.Executable)
	if exe == "" {
		var err error
		if exe, err = currentExecutable(); err != nil {
			return nil, err
		}
	}

	args := cfg.Args
	if args == nil {
		args = []string{"run", "--service"}
		if p := strings.TrimSpace(cfg.ConfigPath); p != "" {
			args = append(args, "--config", p)
		}
		args = append(args, "--log-file", filepath.Join(logDir, LoopLogName))
	}

	kcfg := &kservice.Config{
		Name:        Name,
		DisplayName: DisplayName,
		Description: Description,
		Executable:  exe,
		Arguments:   args,
		UserName:    cfg.UserName,
		EnvVars:     environment(cfg.Env),
		Option: kservice.KeyValue{
			// A user service, not a system daemon: the loop must be the
			// developer, because the tokens, the agent logins and the
			// checkouts all are. On macOS this is ~/Library/LaunchAgents; on
			// Linux, `systemctl --user`. Windows has no user services in this
			// library, which is why UserName above is required there.
			"UserService": true,

			// Start at login, and come back if the process dies.
			"RunAtLoad": true,
			"KeepAlive": true,
			"Restart":   "always", // systemd's spelling of KeepAlive

			// NOT true, and this is the whole keychain story on macOS:
			// SessionCreate asks launchd for a *new security session*, and a
			// new security session has no login keychain. go-keyring reads a
			// token on darwin by shelling out to /usr/bin/security, so the
			// service would start fine and then fail at the first token read.
			"SessionCreate": false,

			// launchd's StandardOutPath/StandardErrorPath, and systemd's
			// StandardOutput=file:. The loop writes its own log via
			// --log-file; these catch whatever gets past it, a panic most of
			// all.
			"LogDirectory": logDir,
			"LogOutput":    true,

			// Windows: come back after a crash rather than sitting stopped.
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "10s",
			"OnFailureResetPeriod":   10,
		},
	}
	if cfg.Password != "" {
		kcfg.Option["Password"] = cfg.Password
	}

	svc, err := kservice.New(&program{}, kcfg)
	if err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	return &Manager{svc: svc, kcfg: kcfg, home: home, logDir: logDir}, nil
}

// currentExecutable is the running binary, with symlinks resolved. A service
// definition that names ~/bin/rein keeps working only for as long as that link
// points where it did at install time; the real path is the honest answer, and
// it is the one `rein service status` prints.
func currentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("service: cannot find this binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// environment is what the service definition carries: PATH and HOME as the
// installing shell has them, the [Passthrough] names that are set, then
// anything the caller added.
//
// PATH is the load-bearing one. Under launchd an agent gets
// /usr/bin:/bin:/usr/sbin:/sbin, which contains none of the coding-agent CLIs
// and no `gh`; a service installed without this would start, heartbeat, claim
// a run, and fail preflight on every queue.
func environment(extra map[string]string) map[string]string {
	env := map[string]string{}
	for _, k := range append([]string{"PATH", "HOME"}, Passthrough...) {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env[k] = v
		}
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// Home is the Rein home the service was built against.
func (m *Manager) Home() string { return m.home }

// LogDir is where the service's logs are written.
func (m *Manager) LogDir() string { return m.logDir }

// Executable is the binary the service runs.
func (m *Manager) Executable() string { return m.kcfg.Executable }

// Arguments are the arguments it runs with.
func (m *Manager) Arguments() []string { return m.kcfg.Arguments }

// Command renders the whole command line, for a status table.
func (m *Manager) Command() string {
	return strings.Join(append([]string{m.kcfg.Executable}, m.kcfg.Arguments...), " ")
}

// Environment returns the service's environment as sorted "K=V" lines.
func (m *Manager) Environment() []string {
	out := make([]string, 0, len(m.kcfg.EnvVars))
	for k, v := range m.kcfg.EnvVars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Platform names the service system in use: "darwin-launchd", "systemd",
// "windows-service".
func (m *Manager) Platform() string { return m.svc.Platform() }

// Install writes the service definition and enables it at login.
func (m *Manager) Install() error {
	// launchd will not create the directory its StandardOutPath lives in, and
	// silently drops the output when it is missing.
	if err := os.MkdirAll(m.logDir, 0o700); err != nil {
		return fmt.Errorf("service: create %s: %w", m.logDir, err)
	}
	if err := m.svc.Install(); err != nil {
		return fmt.Errorf("service: install: %w", err)
	}
	// systemd's user manager has no multi-user.target, which is what the
	// library's unit is written against. See fixup_*.go.
	if err := fixupInstall(m.definitionPath()); err != nil {
		return err
	}
	return nil
}

// Uninstall stops the service if it is running and removes its definition.
func (m *Manager) Uninstall() error {
	if err := m.svc.Uninstall(); err != nil {
		return fmt.Errorf("service: uninstall: %w", err)
	}
	return nil
}

// Start loads and starts the installed service.
func (m *Manager) Start() error {
	if err := m.svc.Start(); err != nil {
		return fmt.Errorf("service: start: %w", err)
	}
	return nil
}

// Stop stops it, leaving it installed.
func (m *Manager) Stop() error {
	if err := m.svc.Stop(); err != nil {
		return fmt.Errorf("service: stop: %w", err)
	}
	return nil
}

// Status is what a person wants to know: installed, and running.
type Status struct {
	Installed bool
	Running   bool
}

// String renders a [Status] as one word.
func (s Status) String() string {
	switch {
	case !s.Installed:
		return "not installed"
	case s.Running:
		return "running"
	default:
		return "installed, not running"
	}
}

// Status reports whether the service is installed and running.
func (m *Manager) Status() (Status, error) {
	st, err := m.svc.Status()
	switch {
	case errors.Is(err, kservice.ErrNotInstalled):
		return Status{}, nil
	case err != nil:
		return Status{}, fmt.Errorf("service: status: %w", err)
	}
	return Status{Installed: true, Running: st == kservice.StatusRunning}, nil
}

// LogPaths are the files the service manager captures the process's output
// into, plus the run loop's own log. All three live under [Manager.LogDir].
func (m *Manager) LogPaths() []string {
	return []string{
		filepath.Join(m.logDir, LoopLogName),
		filepath.Join(m.logDir, Name+".out.log"),
		filepath.Join(m.logDir, Name+".err.log"),
	}
}

// DefinitionPath is where the platform keeps the service definition, for a
// status table and for a runbook. It is a best-effort answer: on Windows there
// is no file, and the empty string says so.
func (m *Manager) DefinitionPath() string { return m.definitionPath() }

func (m *Manager) definitionPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", Name+".plist")
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", Name+".service")
	default:
		return ""
	}
}

// Interactive reports whether this process was started by a person rather than
// by a service manager.
func Interactive() bool { return kservice.Interactive() }

// program adapts a plain function to the service interface. It exists so
// `rein run --service` behaves correctly under the Windows SCM, which requires
// the process to answer a control handler within seconds of starting or be
// killed; on launchd and systemd it collapses to "run until SIGTERM".
type program struct {
	ctx    context.Context
	run    func(context.Context) error
	cancel context.CancelFunc
	done   chan struct{}
	err    error

	// stopping is closed by Stop before the loop's context is cancelled, so
	// the goroutine can tell "the manager asked us to stop" from "the loop
	// fell over on its own" — two situations with opposite right answers.
	stopping chan struct{}

	// stopGrace bounds how long Stop waits for the loop to unwind after the
	// context is cancelled. The SCM's own patience is 30 seconds by default.
	stopGrace time.Duration

	// exit is os.Exit, replaced in tests.
	exit func(int)
	// errOut receives the message printed before an early exit.
	errOut *os.File
}

// Start implements the service interface. It must return promptly: the work
// goes on a goroutine.
func (p *program) Start(kservice.Service) error {
	if p.run == nil {
		return errors.New("service: nothing to run")
	}
	go func() {
		err := p.run(p.ctx)
		p.err = err
		close(p.done)

		select {
		case <-p.stopping:
			// Asked to stop. Stop is waiting on p.done; let it return.
			return
		default:
		}

		// The loop ended on its own — a bad config, an unreadable token, a
		// fatal error. Under launchd a service whose work has finished still
		// sits there loaded, doing nothing and reporting "running", which is
		// the worst of the three possible states. Exiting hands the decision
		// to the manager's restart policy and puts the reason in the log.
		if err != nil {
			fmt.Fprintln(p.errOut, "rein service:", err)
			// kardianos/service v1.3.0 keeps the Windows handler Running
			// until SCM Stop/Shutdown. Exiting here terminates unexpectedly,
			// so its configured OnFailure recovery starts the replacement.
			if errors.Is(err, runner.ErrRestartForUpgrade) {
				p.exit(runner.RestartExitCode)
				return
			}
			p.exit(1)
			return
		}
		p.exit(0)
	}()
	return nil
}

// Stop implements the service interface: cancel the loop's context and wait
// for it, so an in-flight run is interrupted rather than killed mid-write.
func (p *program) Stop(kservice.Service) error {
	close(p.stopping)
	if p.cancel != nil {
		p.cancel()
	}
	select {
	case <-p.done:
	case <-time.After(p.stopGrace):
	}
	return nil
}

// Run drives fn under this platform's service manager and returns when the
// manager stops it. fn is given a context that is cancelled on stop.
//
// It is what `rein run --service` calls. Running it outside a service manager
// works too — kardianos falls back to "run until SIGTERM" — which is what
// makes the flag safe to pass by hand while debugging an install.
func Run(ctx context.Context, cfg Config, fn func(context.Context) error) error {
	m, err := New(cfg)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	p := &program{
		ctx:       runCtx,
		run:       fn,
		cancel:    cancel,
		done:      make(chan struct{}),
		stopping:  make(chan struct{}),
		stopGrace: 25 * time.Second,
		exit:      os.Exit,
		errOut:    os.Stderr,
	}
	svc, err := kservice.New(p, m.kcfg)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	if err := svc.Run(); err != nil {
		return fmt.Errorf("service: run: %w", err)
	}
	// Stop waits on p.done, so a clean stop leaves it closed and p.err safe to
	// read. A Stop that timed out leaves the goroutine live; do not race it.
	select {
	case <-p.done:
		return p.err
	default:
		return nil
	}
}
