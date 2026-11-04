package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/service"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// EnvPairURL overrides where `rein setup` asks for a pairing code. Tests point
// it at an httptest server; a staging Elk is a different host.
const EnvPairURL = "REIN_PAIR_URL"

// newSetupCommand: pair this computer with a short code, enrol, and start the
// service. Elk #1401 lane 7; the server half is scout migration 0596 and the
// worker-pair edge function.
func newSetupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Pair this computer with Elk using a short code, then start working",
		Long: `Pair this computer with your Elk workspace. Rein shows a code; type it into Elk
on your phone or another computer (Connect › Have an AI do tasks, or Advanced ›
A computer showing a code) and press Allow. Nothing is copied or pasted: no
token, link or invite.

Then Rein enrols this computer as your private worker, the same as
` + "`rein enrol --claim-code`" + `, and installs and starts its service so it keeps
working after you close this window (macOS and Linux; on Windows a task
that starts when you sign in, which needs no password).

Before joining, Rein says which workspace and which person allowed it, and
asks you to confirm when you are at the keyboard (--yes skips the question).

The code lives ten minutes and works once. Only a computer holding the secret
Rein made for it can collect the answer, and nothing exists to collect until
someone signed in to Elk presses Allow.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runSetup(cmd) },
	}
	f := cmd.Flags()
	addEnrolConnectFlags(f)
	// A pairing always arrives as a claim; the person never types one.
	for _, hidden := range []string{"claim-code", "token", "workspace", "shared"} {
		_ = f.MarkHidden(hidden)
	}
	f.String("name", "", "what Elk calls this computer (default: its name)")
	f.String("pair-url", "", "Elk's pairing endpoint (default: next to the MCP endpoint, or "+EnvPairURL+")")
	f.Bool("no-service", false, "enrol only; do not install or start the service")
	f.Duration("poll", 0, "how often to check for Allow (default: what Elk says, 3s)")
	f.Bool("yes", false, "join without asking once someone allows the code")
	return cmd
}

// pairStart and pairPoll are the worker-pair edge function's two answers.
type pairStart struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	Interval  int       `json:"interval"`
	Error     string    `json:"error"`
}

type pairPoll struct {
	Status     string `json:"status"`
	Workspace  string `json:"workspace"`
	ApprovedBy string `json:"approved_by"`
	QueueName  string `json:"queue_name"`
	Error      string `json:"error"`
}

type pairCollect struct {
	ClaimCode string `json:"claim_code"`
	Workspace string `json:"workspace"`
	QueueName string `json:"queue_name"`
}

// setupInteractive reports whether a person can answer on stdin. A variable
// so tests can say yes or no.
var setupInteractive = func(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// confirmJoin shows the person at this computer who it is about to work for,
// and asks when they can answer (security review, ark:scout#4320): a code
// seen over a shoulder or guessed must not quietly put this computer to work
// for someone else. --yes, or no one to ask, continues.
func confirmJoin(cmd *cobra.Command, got pairPoll) bool {
	out := cmd.OutOrStdout()
	who := got.ApprovedBy
	if who == "" {
		who = "someone"
	}
	ws := got.Workspace
	if ws == "" {
		ws = "an Elk workspace"
	}
	fmt.Fprintf(out, "%s, in %s, allowed this computer to do their tasks.\n", who, ws)
	if yes, _ := cmd.Flags().GetBool("yes"); yes || !setupInteractive(cmd) {
		return true
	}
	fmt.Fprint(out, "Is that you? Join now? [y/N] ")
	var answer string
	_, _ = fmt.Fscanln(cmd.InOrStdin(), &answer)
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}

func runSetup(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	f := cmd.Flags()
	ctx := cmdContext(cmd)

	path, err := resolveConfigPath(cmd)
	if err != nil {
		return err
	}
	cfg, err := config.LoadFile(path)
	if err != nil && !errors.Is(err, config.ErrNotExist) {
		return err
	}
	// Opened before the code is shown: a keychain that cannot be written
	// should fail now, not after the person has pressed Allow.
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	store, err := keyring.Open(dir)
	if err != nil {
		return err
	}

	// Everything enrol needs, chosen here so the person is asked nothing.
	if v, _ := f.GetString("host-id"); v == "" && cfg.HostID == "" {
		_ = f.Set("host-id", config.DefaultHostID())
	}
	kind, _ := f.GetString("agent-kind")
	if kind == "" {
		kind = detectAgentKind(exec.LookPath)
		if kind == "" {
			return errors.New("no coding agent found on this computer. Install Claude Code (https://claude.com/claude-code), " +
				"sign in once, then run rein setup again")
		}
		_ = f.Set("agent-kind", kind)
	}
	hint := ""
	{
		c := cfg
		if v, _ := f.GetString("host-id"); v != "" {
			c.HostID = v
		}
		if n, ok := c.SuggestQueueName(kind); ok {
			hint = n
		}
	}
	name, _ := f.GetString("name")
	if name == "" {
		name = machineName()
	}
	// Elk lists the worker under a name of at most 12 characters and refuses a
	// longer one at connect, which comes after Allow. So the default is the
	// computer's name cut to fit, and a longer --display-name is refused here,
	// before any code is shown. The Allow screen still gets the full name.
	if v, _ := f.GetString("display-name"); v == "" {
		_ = f.Set("display-name", workerDisplayName(name))
	} else if n := utf8.RuneCountInString(v); n > elk.QueueNameMaxRunes {
		return fmt.Errorf("--display-name %q is %d characters; Elk's cap is %d. Pass a shorter one", v, n, elk.QueueNameMaxRunes)
	}

	mcpURL, _ := f.GetString("mcp-url")
	if mcpURL == "" {
		mcpURL = firstNonEmpty(cfg.Elk.MCPURL, DefaultMCPURL)
	}
	pairURL, _ := f.GetString("pair-url")
	pairURL = firstNonEmpty(pairURL, os.Getenv(EnvPairURL), pairURLFor(mcpURL))

	secret, err := newPairSecret()
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 20 * time.Second}

	var start pairStart
	if err := postPair(ctx, client, pairURL+"/start", map[string]any{
		"secret": secret, "machine_name": name, "agent_kind": kind,
		"platform": pairPlatform(runtime.GOOS), "queue_hint": hint,
	}, &start); err != nil {
		return fmt.Errorf("could not get a code from Elk: %w", err)
	}
	if len(start.Code) != 8 {
		return errors.New("Elk answered without a code; try again in a minute")
	}

	fmt.Fprintf(out, "\n  Your code:  %s-%s\n\n", start.Code[:4], start.Code[4:])
	fmt.Fprintln(out, "  In Elk, press Connect, choose Have an AI do tasks, and type this code.")
	fmt.Fprintln(out, "  On a Mac, it's under Connect › Advanced › A computer showing a code.")
	fmt.Fprintf(out, "  It works for ten minutes. Waiting for you to press Allow…\n\n")

	every, _ := f.GetDuration("poll")
	if every <= 0 {
		every = time.Duration(max(start.Interval, 1)) * time.Second
	}
	deadline := start.ExpiresAt
	if deadline.IsZero() {
		deadline = time.Now().Add(10 * time.Minute)
	}

	var got pairPoll
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
		got = pairPoll{}
		if err := postPair(ctx, client, pairURL+"/poll", map[string]any{"secret": secret}, &got); err != nil {
			// One bad answer is not the end: the network blips. The code's
			// own expiry ends the wait.
			if time.Now().After(deadline) {
				return fmt.Errorf("the code expired. Run rein setup again for a new one (%v)", err)
			}
			continue
		}
		switch got.Status {
		case "pending":
			if time.Now().After(deadline.Add(30 * time.Second)) {
				return errors.New("the code expired. Run rein setup again for a new one")
			}
			continue
		case "approved":
		case "declined":
			return errors.New("someone pressed Not my computer, so this computer did not join")
		case "expired":
			return errors.New("the code expired. Run rein setup again for a new one")
		default:
			return fmt.Errorf("Elk lost this code (%s). Run rein setup again for a new one", got.Status)
		}
		break
	}
	if !confirmJoin(cmd, got) {
		_ = postPair(ctx, client, pairURL+"/cancel", map[string]any{"secret": secret}, &struct{}{})
		return errors.New("not joined. Nothing was set up on this computer")
	}
	var claim pairCollect
	if err := postPair(ctx, client, pairURL+"/collect", map[string]any{"secret": secret}, &claim); err != nil {
		return fmt.Errorf("could not finish joining: %w. Run rein setup again", err)
	}
	if claim.ClaimCode == "" {
		return errors.New("Elk said Allow but sent nothing to collect. Run rein setup again")
	}

	fmt.Fprintln(out, "Joining Elk…")
	// The claim travels in-process only, never through argv or the
	// environment, and enrol never prints it.
	_ = f.Set("claim-code", claim.ClaimCode)
	if ws := firstNonEmpty(claim.Workspace, got.Workspace); ws != "" {
		_ = f.Set("workspace", ws)
	}
	if q := firstNonEmpty(claim.QueueName, got.QueueName); q != "" {
		_ = f.Set("queue", q)
	}
	_ = f.Set("mcp-url", mcpURL)
	if err := runEnrolConnect(cmd, path, cfg, store); err != nil {
		return err
	}

	if noSvc, _ := f.GetBool("no-service"); noSvc {
		return nil
	}
	return startServiceAfterSetup(cmd, out)
}

// startServiceAfterSetup installs (or refreshes) the service and starts it, so
// the worker survives this window closing. Windows asks for the account's
// password to run a service as you, which setup must not collect, so there it
// registers a task that starts at the person's sign-in instead.
func startServiceAfterSetup(cmd *cobra.Command, out io.Writer) error {
	if runtime.GOOS == "windows" {
		// A task that starts at this person's sign-in, with no password
		// (ark:scout#4306), instead of a Windows service that needs one.
		if err := runServiceInstall(cmd); err != nil {
			return fmt.Errorf("joined Elk, but Rein could not be set to start at sign-in: %w\n       run `rein service install`", err)
		}
		if err := service.StartLogonTask(); err != nil {
			return fmt.Errorf("joined Elk, but Rein did not start: %w\n       run `rein service start`", err)
		}
		fmt.Fprintln(out, "\nDone. This computer now does tasks for you, and starts again when you sign in.")
		return nil
	}
	m, err := serviceManager(cmd, service.Config{})
	if err != nil {
		return err
	}
	if st, err := m.Status(); err == nil && st.Installed {
		_ = m.Stop()
	} else if err := m.Install(); err != nil {
		return fmt.Errorf("joined Elk, but the service could not be installed: %w\n       run `rein service install` then `rein service start`", err)
	}
	if err := m.Start(); err != nil {
		return fmt.Errorf("joined Elk, but the service did not start: %w\n       run `rein service start`", err)
	}
	fmt.Fprintln(out, "\nDone. This computer now does tasks for you, and starts again at login.")
	return nil
}

// detectAgentKind picks the coding agent to drive, in the order Elk's plan
// prefers: Claude Code, then Codex, then grok, then Gemini — Google's
// Antigravity CLI, whose program is `agy` and which runs on the person's own
// Google key rather than a plan, so it comes last.
func detectAgentKind(look func(string) (string, error)) string {
	for _, k := range []struct{ kind, binary string }{
		{"claude", "claude"}, {"codex", "codex"}, {"grok", "grok"}, {"gemini", "agy"},
	} {
		if _, err := look(k.binary); err == nil {
			return k.kind
		}
	}
	return ""
}

// pairURLFor puts the pairing endpoint next to elk-mcp:
// https://api.elk.work/functions/v1/elk-mcp → …/functions/v1/worker-pair.
func pairURLFor(mcpURL string) string {
	u := strings.TrimRight(mcpURL, "/")
	if i := strings.LastIndex(u, "/elk-mcp"); i >= 0 {
		return u[:i] + "/worker-pair"
	}
	return u + "/worker-pair"
}

func pairPlatform(goos string) string {
	switch goos {
	case "darwin":
		return "macos"
	case "windows", "linux":
		return goos
	}
	return ""
}

// machineName is what the person sees on Allow: the computer's own name,
// with the ".local" suffix macOS adds dropped.
func machineName() string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return "A computer"
	}
	h = strings.TrimSuffix(strings.TrimSpace(h), ".local")
	if len(h) > 40 {
		h = h[:40]
	}
	return h
}

// workerDisplayName keeps both ends of long computer names so their distinctive
// suffixes remain visible within Elk's rune cap. Blank names use a fallback.
func workerDisplayName(machine string) string {
	if strings.TrimSpace(machine) == "" {
		return "Computer"
	}
	r := []rune(machine)
	if len(r) <= elk.QueueNameMaxRunes {
		return machine
	}
	// Reserve one rune for the ellipsis, favouring the prefix for an odd split.
	tail := (elk.QueueNameMaxRunes - 1) / 2
	head := elk.QueueNameMaxRunes - 1 - tail
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

func newPairSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func postPair(ctx context.Context, c *http.Client, url string, body any, into any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("Elk answered %s", resp.Status)
	}
	return json.Unmarshal(raw, into)
}
