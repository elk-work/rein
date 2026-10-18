package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/elk-work/rein/internal/browser"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runner"
)

// newBrowserCommand: the Chrome agent runs are given (ark:rein#87).
func newBrowserCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "browser",
		Short: "Show or install the Chrome agent runs use",
		Long: `Every run gets one Chrome, with a fresh profile per launch and never a
window: a ` + "`chrome`" + ` command on its PATH, and REIN_CHROME, CHROME_PATH and
PUPPETEER_EXECUTABLE_PATH naming the binary.

Which Chrome: Rein's own Chrome for Testing when it is installed, otherwise the
Chrome already installed on this machine, otherwise none. Rein downloads its
own copy only when you ask, with ` + "`rein browser install`" + ` or
` + "`browser = \"rein\"`" + ` in config.toml.

With no subcommand, prints which Chrome runs get and why.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfigForBrowser(cmd)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), runner.ResolveChrome(cfg).Why)
			return nil
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Download Rein's own Chrome for Testing for agent runs",
		Long: `Download Chrome for Testing ` + browser.Version + ` — Google's build for automation,
which has no updater — into <rein home>/browsers/` + browser.Version + `/, and remove
any older copy. Running it again does nothing once the copy is there.

About 190 MB, from ` + browser.DownloadHost + `. Runs use it from their next
start; a running service picks it up when it restarts.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			_, err = browser.Install(cmdContext(cmd), dir, cmd.OutOrStdout())
			return err
		},
	})
	return cmd
}

// loadConfigForBrowser reads the config, treating a missing one as empty: a
// machine not yet enrolled can still install and inspect its Chrome.
func loadConfigForBrowser(cmd *cobra.Command) (config.Config, error) {
	path, err := resolveConfigPath(cmd)
	if err != nil {
		return config.Config{}, err
	}
	cfg, err := config.LoadFile(path)
	if err != nil && !errors.Is(err, config.ErrNotExist) {
		return config.Config{}, err
	}
	return cfg, nil
}
