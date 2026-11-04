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

// secretNameRE matches what Elk accepts for a secret's name (scout 0597).
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
	cmd.AddCommand(set, rm)
	return cmd
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
