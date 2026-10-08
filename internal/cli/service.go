package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/service"
	"github.com/spf13/cobra"
)

// EnvServicePassword carries the Windows logon password for `rein service
// install` without putting it in `ps` or in a shell history. It is read once
// and never stored: the SCM keeps it, Rein does not.
const EnvServicePassword = "REIN_SERVICE_PASSWORD"

// newServiceCommand builds `rein service`: install, uninstall, start, stop,
// status. ark:rein#8 (01M17TTZP26D58Z0Y4FE3MADWV).
func newServiceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install and control Rein as a user service",
		Long: `Manage Rein's user service: launchd on macOS, a Windows service, or
systemd --user on Linux.

The service runs ` + "`rein run`" + ` — every configured queue, daemon mode, no
--once — as you, with your config file and your keychain. It has to be you:
the Elk tokens are in your keychain, the coding agents are logged in as you,
and the repositories it cuts worktrees from are under your home.

  rein service install     write the service definition and enable it at login
  rein service start       load and start it
  rein service status      installed? running? where are the logs?
  rein service stop        stop it, leave it installed
  rein service uninstall   stop it and remove the definition

docs/service.md has the whole picture, including what to do when a run cannot
read its token.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		newServiceInstallCommand(),
		newServiceUninstallCommand(),
		newServiceStartCommand(),
		newServiceStopCommand(),
		newServiceStatusCommand(),
	)
	return cmd
}

func newServiceInstallCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install Rein as a user service and enable it at login",
		Long: `Write the service definition for this platform and enable it at login.

The installing shell's PATH is captured into the definition. A launchd agent
otherwise inherits /usr/bin:/bin:/usr/sbin:/sbin, which holds none of the
coding-agent CLIs and no gh — so a service installed from a shell that cannot
see ` + "`claude`" + ` is a service that will fail preflight on every run. If you
install a tool somewhere new later, reinstall.

Installing does not start it: ` + "`rein service start`" + ` does.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runServiceInstall(cmd) },
	}
	f := cmd.Flags()
	f.String("executable", "", "the rein binary to install (default: this one)")
	f.String("service-user", "", "Windows only: the account the service logs on as (default: you)")
	f.String("password", "", "Windows only: that account's password — prefer "+EnvServicePassword)
	return cmd
}

func newServiceUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the user service",
		Long: `Stop the service if it is running and remove its definition.

Nothing else is touched: the config, the keychain tokens and any worktrees
under the work directory are left exactly as they are.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runServiceSimple(cmd, "uninstall") },
	}
}

func newServiceStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the installed service",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runServiceSimple(cmd, "start") },
	}
}

func newServiceStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the installed service",
		Long: `Stop the service, leaving it installed and enabled at login.

A run in flight is interrupted and nothing is submitted for it, so its claim
lapses and Elk hands it to whoever asks next — the same contract as Ctrl-C on
` + "`rein run`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runServiceSimple(cmd, "stop") },
	}
}

func newServiceStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the service is installed and running",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runServiceStatus(cmd) },
	}
}

// serviceManager builds the manager the way every subcommand needs it: the
// Rein home from the environment, and `--config` passed through only when it
// was given explicitly, so the service resolves the default itself and keeps
// working if the default ever moves.
func serviceManager(cmd *cobra.Command, cfg service.Config) (*service.Manager, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	cfg.Home = dir
	cfg.ConfigPath, _ = cmd.Flags().GetString("config")
	return service.New(cfg)
}

func runServiceInstall(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	f := cmd.Flags()

	// Refuse to install a service that has nothing to serve. A launchd agent
	// that starts, fails to find a queue and exits is a restart loop with a
	// legible reason nobody reads, and the reason is always the same one.
	path, err := resolveConfigPath(cmd)
	if err != nil {
		return err
	}
	cfg, err := config.LoadFile(path)
	if errors.Is(err, config.ErrNotExist) {
		return fmt.Errorf("no config at %s — run `rein enrol` before installing the service", path)
	}
	if err != nil {
		return err
	}
	if len(cfg.Queues) == 0 {
		return fmt.Errorf("%s has no queues — run `rein enrol` before installing the service", path)
	}

	exe, _ := f.GetString("executable")
	svcUser, _ := f.GetString("service-user")
	password, _ := f.GetString("password")
	if password == "" {
		password = os.Getenv(EnvServicePassword)
	}
	if runtime.GOOS == "windows" {
		// Credential Manager is per-user. A service logging on as LocalSystem
		// starts, heartbeats, claims a run and then cannot read the token
		// `rein enrol` stored — a failure three steps away from its cause.
		if svcUser == "" {
			u, err := user.Current()
			if err != nil {
				return fmt.Errorf("cannot determine the current user for the service logon: %w", err)
			}
			svcUser = u.Username
		}
		if password == "" {
			return fmt.Errorf("a Windows service that logs on as %s needs that account's password: set %s (preferred) or pass --password",
				svcUser, EnvServicePassword)
		}
	}

	m, err := serviceManager(cmd, service.Config{
		Executable: exe,
		UserName:   svcUser,
		Password:   password,
	})
	if err != nil {
		return err
	}
	if err := m.Install(); err != nil {
		return err
	}

	fmt.Fprintf(out, "installed %s (%s)\n", service.Name, m.Platform())
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if p := m.DefinitionPath(); p != "" {
		fmt.Fprintf(w, "  definition\t%s\n", p)
	}
	fmt.Fprintf(w, "  command\t%s\n", m.Command())
	fmt.Fprintf(w, "  logs\t%s\n", m.LogDir())
	fmt.Fprintf(w, "  queues\t%s\n", strings.Join(queueNames(cfg), ", "))
	for _, e := range m.Environment() {
		fmt.Fprintf(w, "  env\t%s\n", e)
	}
	w.Flush()

	if warn := temporaryPathWarning(m.Executable()); warn != "" {
		fmt.Fprintln(out, warn)
	}
	fmt.Fprintf(out, "\nstart it with `rein service start`, then `rein service status`.\n")
	return nil
}

// temporaryPathWarning catches the install that will stop working at the next
// reboot: a binary under a temp or build directory, which is what
// `go run ./cmd/rein service install` produces.
func temporaryPathWarning(exe string) string {
	tmp := filepath.Clean(os.TempDir())
	if tmp != "" && tmp != "." && strings.HasPrefix(filepath.Clean(exe), tmp+string(filepath.Separator)) {
		return "\nwarning: " + exe + " is under the temporary directory, so this service will\n" +
			"break the moment that file is cleaned up. Install a released binary from a\n" +
			"stable path instead."
	}
	return ""
}

func queueNames(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.Queues))
	for _, q := range cfg.Queues {
		names = append(names, q.Name+" ("+q.AgentKind+")")
	}
	return names
}

func runServiceSimple(cmd *cobra.Command, action string) error {
	m, err := serviceManager(cmd, service.Config{})
	if err != nil {
		return err
	}
	switch action {
	case "uninstall":
		if err := m.Uninstall(); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "uninstalled %s\n", service.Name)
	case "start":
		if err := m.Start(); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "started %s — `rein service status` to confirm, logs in %s\n",
			service.Name, m.LogDir())
	case "stop":
		if err := m.Stop(); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "stopped %s\n", service.Name)
	default:
		return fmt.Errorf("service: unknown action %q", action)
	}
	return nil
}

func runServiceStatus(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	m, err := serviceManager(cmd, service.Config{})
	if err != nil {
		return err
	}
	st, err := m.Status()
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "%s: %s\n", service.Name, st)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  platform\t%s\n", m.Platform())
	if p := m.DefinitionPath(); p != "" {
		fmt.Fprintf(w, "  definition\t%s%s\n", p, existsSuffix(p))
	}
	fmt.Fprintf(w, "  command\t%s\n", m.Command())
	for i, p := range m.LogPaths() {
		label := "logs"
		if i > 0 {
			label = ""
		}
		fmt.Fprintf(w, "  %s\t%s%s\n", label, p, existsSuffix(p))
	}
	if store, err := keyring.Open(m.Home()); err == nil {
		fmt.Fprintf(w, "  token store\t%s\n", store.Name())
	}
	w.Flush()

	if !st.Installed {
		fmt.Fprintln(out, "\nnothing installed — `rein service install`.")
	}
	return nil
}

// existsSuffix annotates a path with whether it is there, because half of
// reading this table is working out which file to open.
func existsSuffix(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "  (not yet)"
	}
	if info.IsDir() {
		return ""
	}
	return fmt.Sprintf("  (%d bytes, %s)", info.Size(), info.ModTime().Format("15:04:05"))
}

// logWriter opens the run loop's log file for `rein run --log-file`, creating
// its directory. Appending rather than truncating: a KeepAlive restart must not
// erase the reason for the last one.
func logWriter(path string) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, nil
}
