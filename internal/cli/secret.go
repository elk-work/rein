package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/runner"
	"github.com/spf13/cobra"
)

// secretNameRE matches what Elk accepts for a secret's name (scout 0599).
var secretNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// newSecretCommand: put one secret's value in this computer's keychain for
// the Elk app's "Secrets it may use" (ark:scout#4308). The Elk app on this
// computer runs it; a person can too.
func newSecretCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Store or remove a secret a worker's tasks may use",
		Long: `Store one secret's value in this computer's keychain, for a worker whose
"Secrets it may use" in Elk names it.

  rein secret set POSTHOG_API_KEY    reads the value from stdin
  rein secret rm POSTHOG_API_KEY

The value is read from stdin only (never an argument, so it is never in ps or
shell history), stored under keychain service "` + runner.ManagedSecretService + `", and never sent to
Elk, printed or logged. Choosing which workers may use it happens in the Elk
app; Rein takes that choice at its next check-in.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	set := &cobra.Command{
		Use:   "set NAME",
		Short: "Store NAME's value, read from stdin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, w, err := secretTarget(args[0])
			if err != nil {
				return err
			}
			value, err := readSecretValue(cmd.InOrStdin())
			if err != nil {
				return err
			}
			if err := w.WriteItem(runner.ManagedSecretService, name, value); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stored %s in the %s keychain\n", name, w.Name())
			return nil
		},
	}
	rm := &cobra.Command{
		Use:   "rm NAME",
		Short: "Remove NAME's value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, w, err := secretTarget(args[0])
			if err != nil {
				return err
			}
			if err := w.DeleteItem(runner.ManagedSecretService, name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", name)
			return nil
		},
	}
	cmd.AddCommand(set, rm, newBrokerTokenCommand())
	return cmd
}

// newBrokerTokenCommand: store or remove one queue's vault broker caller token
// (ark:rein#175). Each of the queue's runs is handed it as ELK_CONNECTOR_TOKEN,
// so vault-run reads through Elk's vault broker instead of falling back to the
// Management API.
func newBrokerTokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "broker",
		Short: "Store or remove a queue's vault broker caller token",
		Long: `Store the token a queue's runs hand to Elk's vault broker, so vault-run
reads through the broker instead of falling back to the Management API.

  rein secret broker set --queue mac-claude    reads the token from stdin
  rein secret broker rm --queue mac-claude

The token is read from stdin only, stored in this computer's keychain under
service "` + keyring.BrokerService + `", account "<workspace>/<queue>", and never sent
to Elk, printed or logged. Each run of the queue reads it fresh and gets it as
` + runner.BrokerTokenEnv + `; it must not be the queue's own Elk token, which Rein
claims with and a run must never hold. Only a queue that keeps the vault
(supabase-vault) and has no secrets map uses it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.PersistentFlags().String("queue", "", "the queue, as name or workspace/name (default: the only queue)")
	set := &cobra.Command{
		Use:   "set",
		Short: "Store the queue's token, read from stdin",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, q, store, err := brokerTarget(cmd)
			if err != nil {
				return err
			}
			value, err := readSecretValue(cmd.InOrStdin())
			if err != nil {
				return err
			}
			value = strings.TrimSpace(value)
			if err := runner.CheckBrokerToken(value); err != nil {
				return fmt.Errorf("that value is %v; nothing was stored", err)
			}
			if err := store.SetBrokerToken(ws, q.Name, value); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stored the vault broker token for %s/%s; its next run gets it\n", ws, q.Name)
			if q.Scoped() {
				fmt.Fprintf(cmd.OutOrStdout(), "note: %s has a secrets map, so its runs do not use it\n", q.Name)
			}
			return nil
		},
	}
	rm := &cobra.Command{
		Use:   "rm",
		Short: "Remove the queue's token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, q, store, err := brokerTarget(cmd)
			if err != nil {
				return err
			}
			if err := store.DeleteBrokerToken(ws, q.Name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed the vault broker token for %s/%s\n", ws, q.Name)
			return nil
		},
	}
	cmd.AddCommand(set, rm)
	return cmd
}

func brokerTarget(cmd *cobra.Command) (string, config.Queue, keyring.BrokerTokens, error) {
	path, err := resolveConfigPath(cmd)
	if err != nil {
		return "", config.Queue{}, nil, err
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		return "", config.Queue{}, nil, err
	}
	q, err := pickQueue(cmd, cfg)
	if err != nil {
		return "", q, nil, err
	}
	ws := cfg.WorkspaceFor(q)
	if ws == "" {
		return "", q, nil, fmt.Errorf("queue %s has no workspace; re-enrol it first", q.Name)
	}
	dir, err := config.Dir()
	if err != nil {
		return "", q, nil, err
	}
	w, err := keyring.OpenItemWriter(dir)
	if err != nil {
		return "", q, nil, err
	}
	store, ok := w.(keyring.BrokerTokens)
	if !ok {
		return "", q, nil, fmt.Errorf("the %s keychain backend cannot hold a vault broker token", w.Name())
	}
	return ws, q, store, nil
}

func secretTarget(raw string) (string, keyring.ItemWriter, error) {
	name := strings.ToUpper(strings.TrimSpace(raw))
	if !secretNameRE.MatchString(name) {
		return "", nil, fmt.Errorf("%q is not a secret name: use capital letters, digits and _, starting with a letter", raw)
	}
	dir, err := config.Dir()
	if err != nil {
		return "", nil, err
	}
	w, err := keyring.OpenItemWriter(dir)
	return name, w, err
}

// readSecretValue takes everything on stdin, minus one trailing newline, so
// `printf %s "$v" | rein secret set X` and `echo "$v" | …` store the same.
func readSecretValue(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(bufio.NewReader(r), 64<<10))
	if err != nil {
		return "", err
	}
	v := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if v == "" {
		return "", errors.New("no value on stdin: pipe it in, for example from the Elk app")
	}
	return v, nil
}
