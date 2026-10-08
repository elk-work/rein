package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/runner"
	"github.com/spf13/cobra"
)

// DefaultMCPURL is Elk's connector endpoint. It is a default rather than a
// constant because a self-hosted or staging Elk is a different host, and
// because `--mcp-url` is how the tests point at an httptest server.
const DefaultMCPURL = "https://api.elk.work/functions/v1/elk-mcp"

// Environment variables that carry a credential, so it need not appear in
// argv where `ps` can read it.
const (
	EnvToken     = "REIN_ELK_TOKEN"
	EnvClaimCode = "REIN_ELK_CLAIM_CODE"
)

// newEnrolCommand: claim code → connect_executor → run-scoped token in the OS
// keychain. ark:rein#2 (01M17TTZHTBTYQFFSGF5MM7Y11).
func newEnrolCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enrol",
		Short: "Enrol this machine into an Elk workspace as a named queue",
		Long: `Bind this machine to an Elk workspace as one named queue for one agent kind,
and store the queue's Elk token in the OS keychain under service "rein", keyed
by workspace and queue.

Two ways in:

  --claim-code <code>   a single-use invite from Elk (Connected agents →
                        Connect another agent). Rein exchanges it for a durable
                        run-scoped token and stores that. This is the narrow
                        credential and the one to prefer.
  --token <token>       your own connector token. Broader than an invite — it
                        is your personal, interactive-tier credential — but it
                        is the path that works without an invite.

Either can come from the environment instead of the command line, which keeps
it out of ` + "`ps`" + `: ` + EnvClaimCode + ` and ` + EnvToken + `.

One queue per (machine, agent kind): mac-claude, mac-codex, win-codex. Queue
names are capped at 12 characters and a longer one is rejected, not truncated —
the name is the address claim_run resolves, so two names cut to the same prefix
would be one queue draining two machines' work.

A queue is PRIVATE by default: only the person whose credential connected it
can send work to it. Pass --shared to let any member of the workspace queue
work here. The choice is not permanent either way — its owner changes it later
in Elk → Settings → Connected agents. On the --claim-code path the inviting
person already chose it on the invite sheet, so Elk ignores --shared there and
says so in its reply.

  rein enrol --revoke --queue mac-claude

forgets a queue locally: it deletes the keychain entry first, then drops the
queue from the config. It does not revoke the credential in Elk — an admin does
that in Settings → Connected agents.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnrol(cmd)
		},
	}
	f := cmd.Flags()
	f.String("claim-code", "", "single-use claim code from Elk (or "+EnvClaimCode+")")
	f.String("token", "", "an existing Elk connector token (or "+EnvToken+")")
	f.String("workspace", "", "Elk workspace to enrol into")
	f.String("queue", "", "queue name (default: <host-id>-<agent-kind>)")
	f.String("agent-kind", "", "coding agent this queue drives: "+strings.Join(elk.AgentKinds, ", "))
	f.String("display-name", "", "name shown on Elk's Connected agents page (default: the queue name)")
	f.String("host-id", "", "short machine label used in the queue name (default: suggested from the hostname)")
	f.String("mcp-url", "", "Elk MCP endpoint (default: "+DefaultMCPURL+")")
	f.Bool("shared", false, "let any workspace member send work to this queue (default: private to you; ignored with --claim-code)")
	f.Bool("revoke", false, "forget a queue: delete its keychain entry, then drop it from the config")
	f.Bool("force", false, "replace an existing enrolment for this queue")
	return cmd
}

func runEnrol(cmd *cobra.Command) error {
	path, err := resolveConfigPath(cmd)
	if err != nil {
		return err
	}
	cfg, err := config.LoadFile(path)
	if err != nil && !errors.Is(err, config.ErrNotExist) {
		return err
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	store, err := keyring.Open(dir)
	if err != nil {
		return err
	}

	if revoke, _ := cmd.Flags().GetBool("revoke"); revoke {
		return runRevoke(cmd, path, cfg, store)
	}
	return runEnrolConnect(cmd, path, cfg, store)
}

func runEnrolConnect(cmd *cobra.Command, path string, cfg config.Config, store keyring.Store) error {
	out := cmd.OutOrStdout()
	f := cmd.Flags()

	if v, _ := f.GetString("host-id"); v != "" {
		cfg.HostID = v
	}
	if cfg.HostID == "" {
		return errors.New("no host id: pass --host-id (a short label like \"mac\"; it becomes half of the queue name)")
	}
	if v, _ := f.GetString("mcp-url"); v != "" {
		cfg.Elk.MCPURL = v
	}
	if cfg.Elk.MCPURL == "" {
		cfg.Elk.MCPURL = DefaultMCPURL
	}

	agentKind, _ := f.GetString("agent-kind")
	if agentKind == "" {
		return fmt.Errorf("--agent-kind is required: one of %s", strings.Join(elk.AgentKinds, ", "))
	}
	if !elk.KnownAgentKind(agentKind) {
		return fmt.Errorf("--agent-kind %q is not one Elk knows; use one of %s",
			agentKind, strings.Join(elk.AgentKinds, ", "))
	}

	queue, _ := f.GetString("queue")
	if queue == "" {
		name, ok := cfg.SuggestQueueName(agentKind)
		if !ok {
			return fmt.Errorf("no queue name: %q-%q is over the %d-character cap, so pass a shorter --host-id or an explicit --queue",
				cfg.HostID, agentKind, elk.QueueNameMaxRunes)
		}
		queue = name
	}
	if n := utf8.RuneCountInString(queue); n > elk.QueueNameMaxRunes {
		return fmt.Errorf("queue name %q is %d characters; Elk's cap is %d and an over-long name is refused, not shortened — the name is the address claim_run resolves",
			queue, n, elk.QueueNameMaxRunes)
	}

	workspace, _ := f.GetString("workspace")
	if workspace == "" {
		workspace = cfg.Workspace
	}

	flagCode, _ := f.GetString("claim-code")
	flagToken, _ := f.GetString("token")
	claimCode := firstNonEmpty(flagCode, os.Getenv(EnvClaimCode))
	token := firstNonEmpty(flagToken, os.Getenv(EnvToken))
	switch {
	case claimCode != "" && token != "":
		return errors.New("pass --claim-code or --token, not both")
	case claimCode == "" && token == "":
		return fmt.Errorf("no credential: pass --claim-code (preferred) or --token, or set %s / %s",
			EnvClaimCode, EnvToken)
	}
	// A claim code carries its own workspace and queue; anything else needs
	// to be told which workspace, because a person's token can reach several
	// and Elk asks rather than guessing.
	if claimCode == "" && workspace == "" {
		return errors.New("--workspace is required unless you are enrolling with a --claim-code, which carries its own")
	}

	force, _ := f.GetBool("force")
	if !force && workspace != "" {
		if _, err := store.Get(workspace, queue); err == nil {
			return fmt.Errorf("queue %q in workspace %q already has a stored token; pass --force to replace it", queue, workspace)
		}
	}

	displayName, _ := f.GetString("display-name")
	if displayName == "" {
		displayName = queue
	}

	// What Elk is told this machine can do spans both namespaces the run loop
	// checks: the adapter's own runtime manifest, and this machine's
	// environment list. One source each, so the declaration and the preflight
	// cannot drift apart — and Pace sees the half that actually varies between
	// machines rather than only the runtime half.
	host := runner.DetectHostCapabilities(cmdContext(cmd), cfg)
	var declared []string
	if a, ok := adapter.Lookup(agentKind); ok {
		declared = runner.DeclaredCapabilities(a.Manifest(), host)
	} else {
		declared = host.Names()
		fmt.Fprintf(out, "note: this build has no %q adapter, so only this machine's environment\n", agentKind)
		fmt.Fprintln(out, "      capabilities are declared to Elk, not the agent's runtime ones.")
		if kinds := adapter.Kinds(); len(kinds) > 0 {
			fmt.Fprintf(out, "      it can drive: %s\n", strings.Join(kinds, ", "))
		}
	}

	if cfg.MachineID == "" {
		id, err := config.NewMachineID()
		if err != nil {
			return err
		}
		cfg.MachineID = id
	}

	bearer := firstNonEmpty(claimCode, token)
	client, err := elk.New(cfg.Elk.MCPURL, bearer,
		elk.WithVersion(cmd.Root().Version),
		elk.WithLogger(func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }))
	if err != nil {
		return err
	}

	shared, _ := f.GetBool("shared")

	ctx := cmdContext(cmd)
	res, err := client.ConnectExecutor(ctx, elk.ConnectRequest{
		Workspace:            workspace,
		DisplayName:          displayName,
		Queue:                queue,
		HostID:               cfg.MachineID,
		AgentKind:            agentKind,
		DeclaredCapabilities: declared,
		ClaimCode:            claimCode,
		// Rein qualifies for self_configure: it consumes the durable URL
		// straight into the OS keychain and never prints, logs or repeats it.
		SelfConfigure: claimCode != "",
		// Only when asked for. Private is Elk's default and Rein does not
		// restate it, so an Elk older than scout 0201 sees no `shared` key.
		Shared: shared,
	})
	if err != nil {
		var tier *elk.TierRefusalError
		if errors.As(err, &tier) && tier.Tool == elk.ToolConnectExecutor {
			return fmt.Errorf("%w\n       an invited agent's token cannot re-bind a queue. Enrol with --claim-code, "+
				"or use the interactive connector URL from Elk → Tweaks → Elk Connector", err)
		}
		return err
	}
	fmt.Fprintln(out, res.Text)

	// The claim carries its own queue and workspace, so trust Elk's answer
	// over the request for the name we file under.
	if res.Queue != "" {
		queue = res.Queue
	}
	if workspace == "" {
		ws, err := resolveSoleWorkspace(ctx, client)
		if err != nil {
			return fmt.Errorf("enrolled, but Rein could not tell which workspace to file %q under: %w\n"+
				"       re-run with --workspace to finish", queue, err)
		}
		workspace = ws
	}

	stored := firstNonEmpty(res.Token, token)
	if stored == "" {
		return fmt.Errorf("Elk bound queue %q but returned no durable credential.\n"+
			"       An admin reveals it once in Settings → Connected agents; re-run with --token <that token>", queue)
	}

	// Config first, then the token. The OS keychain has no enumeration, so
	// the config is the only index of which tokens exist: a token written
	// without its config row is a secret nothing can find again, while a
	// config row without a token is visible in `rein status` as
	// "missing — run `rein enrol`" and costs one re-run.
	cfg.Workspace = firstNonEmpty(cfg.Workspace, workspace)
	q := config.Queue{Name: queue, AgentKind: agentKind}
	if workspace != cfg.Workspace {
		q.Workspace = workspace
	}
	replaced := cfg.SetQueue(q)
	if err := cfg.SaveFile(path); err != nil {
		return err
	}
	if err := store.Set(workspace, queue, stored); err != nil {
		return fmt.Errorf("queue %q is in %s but its token could not be stored: %w", queue, path, err)
	}

	verb := "Enrolled"
	if replaced {
		verb = "Re-enrolled"
	}
	fmt.Fprintf(out, "\n%s queue %q (%s) in workspace %q.\n", verb, queue, agentKind, workspace)
	fmt.Fprintf(out, "config  %s\n", path)
	fmt.Fprintf(out, "token   %s keyring, as %s/%s\n", store.Name(), workspace, queue)
	if len(declared) > 0 {
		fmt.Fprintf(out, "declared %s\n", strings.Join(declared, ", "))
	}
	// Who may send work here. Only on the self-connect path: a claim exchange
	// takes the inviting person's choice, which Rein never learns — Elk's own
	// reply above is the authority there, and it says so when --shared was
	// passed and ignored.
	if claimCode == "" {
		if shared {
			fmt.Fprintln(out, "access  shared — any member of the workspace can send work to this queue")
		} else {
			fmt.Fprintln(out, "access  private to you — pass --shared to open it to the workspace")
		}
		fmt.Fprintln(out, "        (its owner changes this later in Elk → Settings → Connected agents)")
	}

	// Prove the stored credential works, without claiming anything. A
	// heartbeat is the better probe because it also registers this machine as
	// online in `list_executors` — the view people and Elk's router dispatch
	// from. (Elk itself does not refuse work to an offline queue; nothing on
	// the claim path checks.)
	live, err := elk.New(cfg.Elk.MCPURL, stored, elk.WithVersion(cmd.Root().Version))
	if err != nil {
		return err
	}
	verifyEnrolment(ctx, out, live, workspace, queue, cfg.MachineID, agentKind, declared)
	fmt.Fprintf(out, "\nNext: rein status, then rein run --queue %s --once\n", queue)
	return nil
}

// verifyEnrolment proves the stored token works and, where Elk supports it,
// marks the queue online. Nothing here is fatal: the enrolment has already
// landed, and a probe that cannot run is worth saying out loud rather than
// unwinding a good enrolment over.
func verifyEnrolment(ctx context.Context, out io.Writer, c *elk.Client, workspace, queue, machineID, agentKind string, declared []string) {
	hb, err := c.HeartbeatExecutor(ctx, elk.HeartbeatRequest{
		Workspace: workspace, Queue: queue,
		HostID: machineID, AgentKind: agentKind, DeclaredCapabilities: declared,
	})
	switch {
	case err == nil:
		fmt.Fprintf(out, "elk     %s\n", hb.Text)
		return
	case !errors.As(err, new(*elk.UnknownToolError)):
		fmt.Fprintf(out, "elk     heartbeat failed: %v\n", err)
		return
	}
	// A deployment that has not shipped heartbeat_executor yet. Fall back to
	// the cheapest read that proves the token, rather than to a claim.
	fmt.Fprintln(out, "elk     this Elk has no heartbeat_executor yet; the queue will not show as online until it does")
	if _, err := c.ListWorkspaces(ctx); err != nil {
		fmt.Fprintf(out, "elk     the stored token did not work: %v\n", err)
		return
	}
	fmt.Fprintln(out, "elk     the stored token works")
}

// resolveSoleWorkspace answers "which workspace" for a claim-code enrolment,
// where the claim carried the workspace and the reply did not name it. It is
// only willing to answer when there is exactly one.
func resolveSoleWorkspace(ctx context.Context, c *elk.Client) (string, error) {
	text, err := c.ListWorkspaces(ctx)
	if err != nil {
		return "", err
	}
	names := workspaceNames(text)
	if len(names) != 1 {
		return "", fmt.Errorf("this token reaches %d workspaces", len(names))
	}
	return names[0], nil
}

// workspaceNames pulls quoted or bulleted workspace names out of
// list_workspaces' text. It is deliberately forgiving: it is used only to
// resolve the single-workspace case, and anything ambiguous falls back to
// asking for --workspace.
func workspaceNames(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		if line == "" || strings.HasSuffix(line, ":") || strings.HasPrefix(line, "#") {
			continue
		}
		if name, _, ok := strings.Cut(line, " — "); ok {
			line = name
		}
		if name, _, ok := strings.Cut(line, " ("); ok {
			line = name
		}
		if line = strings.TrimSpace(strings.Trim(line, `"`)); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func runRevoke(cmd *cobra.Command, path string, cfg config.Config, store keyring.Store) error {
	out := cmd.OutOrStdout()
	f := cmd.Flags()

	queue, _ := f.GetString("queue")
	if queue == "" {
		return errors.New("--revoke needs --queue <name>")
	}
	q, ok := cfg.Queue(queue)
	if !ok {
		return fmt.Errorf("no queue %q in %s", queue, path)
	}
	workspace, _ := f.GetString("workspace")
	if workspace == "" {
		workspace = cfg.WorkspaceFor(q)
	}
	if workspace == "" {
		return fmt.Errorf("queue %q names no workspace; pass --workspace", queue)
	}

	// Keychain first, config second — the exact reverse of enrolment, and for
	// the same reason. The OS keychain cannot be enumerated, so this config
	// entry is the only record that a token for (workspace, queue) exists.
	// Drop the config row first and the secret stays in the keychain with
	// nothing left that knows to ask for it.
	if err := store.Delete(workspace, queue); err != nil {
		return fmt.Errorf("refusing to drop queue %q from the config: its token could not be deleted (%w) "+
			"and the config is the only record that it exists", queue, err)
	}
	cfg.RemoveQueue(queue)
	if err := cfg.SaveFile(path); err != nil {
		return err
	}

	fmt.Fprintf(out, "Forgot queue %q in workspace %q: token deleted from the %s keyring, queue dropped from %s.\n",
		queue, workspace, store.Name(), path)
	fmt.Fprintln(out, "This is local only. The credential still exists in Elk until an admin revokes it in")
	fmt.Fprintln(out, "Settings → Connected agents.")
	return nil
}

// cmdContext is the command's context, or a background one. Cobra leaves
// Context nil when a caller drives the tree directly, which the tests do.
func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// resolveConfigPath turns the root's --config flag into a path, falling back
// to the Rein home. Every command that reads or writes the config goes through
// it, so a test pointing REIN_HOME somewhere and a person passing --config
// take the same route.
func resolveConfigPath(cmd *cobra.Command) (string, error) {
	if p, _ := cmd.Flags().GetString("config"); p != "" {
		return p, nil
	}
	return config.Path()
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
