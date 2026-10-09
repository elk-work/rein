package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/runner"
	"github.com/spf13/cobra"
)

// preflightTimeout bounds each adapter's local readiness check. A preflight
// shells out to a vendor binary, and one that hangs must not hang `rein
// status` — the command a person runs precisely when something is wrong.
const preflightTimeout = 10 * time.Second

// newStatusCommand reports what this machine is configured to do, and whether
// it could actually do it.
func newStatusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report this machine's configuration, enrolled queues and adapter readiness",
		Long: `Print the config, every enrolled queue, whether each queue's token is in the
keychain, and whether the adapter for each queue's agent kind can actually run
here.

It talks to nothing by default: everything above is local. Pass --online to ask
Elk what it thinks — which queues it can see, whether they are online, and how
much work is waiting.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(cmd)
			if err != nil {
				return err
			}
			online, _ := cmd.Flags().GetBool("online")
			return runStatus(cmd, path, online)
		},
	}
	cmd.Flags().Bool("online", false, "also ask Elk what it can see (needs a stored token)")
	return cmd
}

func runStatus(cmd *cobra.Command, path string, online bool) error {
	out := cmd.OutOrStdout()

	cfg, err := config.LoadFile(path)
	missing := errors.Is(err, config.ErrNotExist)
	if err != nil && !missing {
		return err
	}

	dir, dirErr := config.Dir()
	if dirErr != nil {
		return dirErr
	}
	store, err := keyring.Open(dir)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	note := ""
	if missing {
		note = " (not created yet)"
	}
	fmt.Fprintf(w, "config\t%s%s\n", path, note)
	fmt.Fprintf(w, "home\t%s\n", dir)
	fmt.Fprintf(w, "host_id\t%s\n", orDash(cfg.HostID))
	fmt.Fprintf(w, "machine_id\t%s\n", orDash(cfg.MachineID))
	fmt.Fprintf(w, "workspace\t%s\n", orDash(cfg.Workspace))
	fmt.Fprintf(w, "work_dir\t%s\n", orDash(cfg.WorkDir))
	fmt.Fprintf(w, "elk\t%s\n", orDash(cfg.Elk.MCPURL))
	fmt.Fprintf(w, "keyring\t%s\n", store.Name())
	if err := w.Flush(); err != nil {
		return err
	}

	printAdapters(out)
	printHostCapabilities(cmd, out, cfg)
	printMachine(out, cfg)

	if len(cfg.Queues) == 0 {
		fmt.Fprintln(out, "\nqueues   none — run `rein enrol`")
		return nil
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	fmt.Fprintln(out)
	qw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(qw, "QUEUE\tAGENT\tWORKSPACE\tLAND\tTOKEN\tADAPTER\tSUBSCRIPTION")
	for _, q := range cfg.Queues {
		ws := cfg.WorkspaceFor(q)
		fmt.Fprintf(qw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			cfg.QueueLabel(q), q.AgentKind, orDash(ws), q.LandOrDefault(), tokenState(store, ws, q.Name),
			preflightState(ctx, q.AgentKind), subscriptionState(dir, cfg.QueueLabel(q)))
	}
	if err := qw.Flush(); err != nil {
		return err
	}
	if w := cfg.RepositoryCompatibilityWarning(); w != "" {
		fmt.Fprintf(out, "\nWARNING: %s\n", w)
	}

	if online {
		printOnline(ctx, out, cmd.Root().Version, cfg, store)
	}
	return nil
}

// subscriptionState is what the running service last knew about a queue's
// subscription window, read from the state it keeps under the Rein home
// (internal/runner/subscription.go, ark:rein#40). A dash is "never read":
// Claude's window is only known once a run has seen it.
func subscriptionState(home, queue string) string {
	s, ok := runner.ReadSubscriptionState(filepath.Join(home, "state"), queue)
	if !ok {
		return "—"
	}
	return s.Summary(time.Now())
}

// printAdapters says which agent kinds this binary can drive at all, before
// any question of whether they are installed. It is the answer to "why does
// my queue say no adapter registered": the adapters a build has are the
// packages it imports, so the honest answer is a property of the binary.
func printAdapters(out io.Writer) {
	kinds := adapter.Kinds()
	if len(kinds) == 0 {
		fmt.Fprintln(out, "adapters  none compiled into this build")
		return
	}
	fmt.Fprintf(out, "adapters  %s\n", strings.Join(kinds, ", "))
}

// printHostCapabilities lists the ENVIRONMENT capability names this machine
// holds, with how each was established.
//
// It is on the status page because it is half of what a packet's
// `required_capabilities` is checked against, and the invisible half: the
// adapter's declaration is in the code, while this one depends on what happens
// to be installed and logged in. A run that goes `stuck` on "github-cli" is
// answered here.
func printHostCapabilities(cmd *cobra.Command, out io.Writer, cfg config.Config) {
	host := runner.DetectHostCapabilities(cmdContext(cmd), cfg)
	fmt.Fprintf(out, "host caps %s\n", strings.Join(host.Describe(), ", "))
}

// printMachine shows the `host` half of the fleet reading this machine sends
// Elk on every beat, measured now.
//
// It is here for the same reason the capability list is: it is what Elk will
// be told, and the only other way to find out is to look at a fleet page and
// guess. A platform that cannot measure something says so in words —
// "load1 —" — because the whole design turns on "could not measure" and "zero"
// being different claims, and a status page that printed 0 would be the first
// place that stopped being true.
func printMachine(out io.Writer, cfg config.Config) {
	workDir := cfg.WorkDir
	if workDir == "" {
		if dir, err := config.Dir(); err == nil {
			workDir = dir
		}
	}
	m := runner.DetectMachine(workDir)

	fmt.Fprintf(out, "machine   %s, %s, %d cpu\n",
		dashIf(m.OS, m.OSKnown), dashIf(m.CPUModel, m.CPUModelKnown), m.CPUCount)

	mem := dashIf(bytesOr(m.Headroom.FreeRAM, m.Headroom.RAMKnown)+" free of "+
		bytesOr(m.TotalRAM, m.TotalRAMKnown), m.TotalRAMKnown || m.Headroom.RAMKnown)
	disk := dashIf(bytesOr(m.Headroom.FreeDisk, m.Headroom.DiskKnown)+" free of "+
		bytesOr(m.TotalDisk, m.TotalDiskKnown), m.TotalDiskKnown || m.Headroom.DiskKnown)
	load := "—"
	if m.Load1Known {
		load = fmt.Sprintf("%.2f", m.Load1)
	}
	fmt.Fprintf(out, "          memory %s, disk %s (%s), load1 %s\n", mem, disk, workDir, load)

	// The same sentence the run loop logs when the count changes, so a person
	// comparing the two is comparing the same thing.
	_, why := runner.Slots(m.Headroom, runner.DefaultMaxConcurrent)
	fmt.Fprintf(out, "          %s\n", why)

	if cfg.Telemetry.Disabled {
		fmt.Fprintln(out, "          telemetry off — no host or session object rides the beat")
	}
}

// dashIf renders an unmeasured value as an em dash rather than as a zero.
func dashIf(s string, known bool) string {
	if !known || strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// bytesOr renders a byte count, or a dash when it was never measured.
func bytesOr(n uint64, known bool) string {
	if !known {
		return "—"
	}
	return runner.HumanBytes(n)
}

// preflightState reports whether the adapter for an agent kind can run here.
//
// An unregistered kind is not an error: adapters land one at a time, and a
// config naming a kind this build has no adapter for is the ordinary state of
// a half-built runner. It says so plainly instead.
func preflightState(ctx context.Context, kind string) string {
	a, ok := adapter.Lookup(kind)
	if !ok {
		return "no adapter registered"
	}
	m := a.Manifest()
	if !m.SupportsHost() {
		platforms := make([]string, len(m.Platforms))
		for i, p := range m.Platforms {
			platforms[i] = p.String()
		}
		sort.Strings(platforms)
		return "not supported on this host (" + strings.Join(platforms, ", ") + ")"
	}
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	if err := a.Preflight(ctx); err != nil {
		return "not ready: " + firstLine(err.Error())
	}
	return "ready"
}

// printOnline asks Elk what it can see. It uses the first queue that has a
// token: a token is per (workspace, queue) but list_executors answers for the
// whole workspace, so one is enough per workspace.
func printOnline(ctx context.Context, out io.Writer, version string, cfg config.Config, store keyring.Store) {
	if cfg.Elk.MCPURL == "" {
		fmt.Fprintln(out, "\nonline   no Elk endpoint in the config — run `rein enrol`")
		return
	}
	seen := map[string]bool{}
	asked := false
	for _, q := range cfg.Queues {
		ws := cfg.WorkspaceFor(q)
		if ws == "" || seen[ws] {
			continue
		}
		token, err := store.Get(ws, q.Name)
		if err != nil {
			continue
		}
		seen[ws] = true
		asked = true
		fmt.Fprintf(out, "\nonline   %s\n", ws)
		client, err := elk.New(cfg.Elk.MCPURL, token, elk.WithVersion(version))
		if err != nil {
			fmt.Fprintf(out, "  %v\n", err)
			continue
		}
		text, err := client.ListExecutors(ctx, ws)
		if err != nil {
			var unknown *elk.UnknownToolError
			if errors.As(err, &unknown) {
				fmt.Fprintln(out, "  this Elk has no list_executors yet")
				continue
			}
			fmt.Fprintf(out, "  %v\n", err)
			continue
		}
		for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
	if !asked {
		fmt.Fprintln(out, "\nonline   no queue has a stored token — run `rein enrol`")
	}
}

// tokenState reports whether a queue has a token without ever printing one.
func tokenState(store keyring.Store, workspace, queue string) string {
	if workspace == "" {
		return "no workspace"
	}
	switch _, err := store.Get(workspace, queue); {
	case err == nil:
		return "stored"
	case errors.Is(err, keyring.ErrNotFound):
		return "missing — run `rein enrol`"
	default:
		return "unreadable: " + err.Error()
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
