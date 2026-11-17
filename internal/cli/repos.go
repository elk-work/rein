package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/elk-work/rein/internal/config"
	"github.com/spf13/cobra"
)

// newReposCommand changes a queue's repositories after enrol (ark:rein#160).
// It writes config.toml the way `rein enrol --repo` does, every other queue
// kept on what it serves. A running service restarts on an added repository
// between tasks, and its next heartbeat declares the new list to Elk.
func newReposCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repos",
		Short: "Add or remove a queue's repositories without enrolling again",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.PersistentFlags().String("queue", "", "the queue to change, as name or workspace/name (default: the only queue)")
	cmd.AddCommand(newReposAddCommand(), newReposRemoveCommand())
	return cmd
}

func newReposAddCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add owner/name",
		Short: "Let a queue work in a repository: a checkout on this machine, or one cloned now",
		Long: `Add a repository to a queue. Give the checkout with --path, or pass --clone to
download it first (from https://github.com/owner/name.git into ~/Developer/name,
unless --from or --into say otherwise; a checkout already there is reused).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			path, _ := cmd.Flags().GetString("path")
			clone, _ := cmd.Flags().GetBool("clone")
			var dir string
			var err error
			switch {
			case path != "" && clone:
				return fmt.Errorf("pass --path or --clone, not both")
			case path != "":
				dir, err = checkoutPath(path)
			case clone:
				from, _ := cmd.Flags().GetString("from")
				into, _ := cmd.Flags().GetString("into")
				dir, err = cloneRepo(cmd, name, from, into)
			default:
				return fmt.Errorf("say where %s is: --path <checkout>, or --clone to download it", name)
			}
			if err != nil {
				return err
			}
			force, _ := cmd.Flags().GetBool("force")
			return changeRepos(cmd, func(cfg config.Config, q config.Queue) (config.Config, config.Queue, error) {
				return applyEnrolRepos(cfg, q, enrolRepos{Repos: []namedRepo{{Name: name, Path: dir}}}, force)
			}, addNote)
		},
	}
	cmd.Flags().String("path", "", "the repository's checkout on this machine")
	cmd.Flags().Bool("clone", false, "download the repository first")
	cmd.Flags().String("from", "", "with --clone: the URL or path to clone (default: https://github.com/owner/name.git)")
	cmd.Flags().String("into", "~/Developer", "with --clone: the folder the checkout goes in")
	cmd.Flags().Bool("force", false, "point an existing [repos] name at this checkout")
	return cmd
}

func newReposRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove owner/name",
		Short: "Stop a queue working in a repository (the checkout is left alone)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			return changeRepos(cmd, func(cfg config.Config, q config.Queue) (config.Config, config.Queue, error) {
				var kept []string
				for _, r := range q.Repos {
					if r != name {
						kept = append(kept, r)
					}
				}
				switch {
				case len(kept) == len(q.Repos):
					return cfg, q, fmt.Errorf("queue %s does not list %s; it lists %s", q.Name, name, strings.Join(q.Repos, ", "))
				case len(kept) == 0:
					return cfg, q, fmt.Errorf("%s is queue %s's last repository, and a queue with none stops every run; add another first, or revoke the queue", name, q.Name)
				}
				q.Repos = kept
				if q.Repo == name {
					// A run that names none still needs a default.
					q.Repo = kept[0]
				}
				return cfg, q, nil
			}, removalNote)
		},
	}
}

// changeRepos applies edit to the chosen queue and saves config.toml, every
// other queue kept on what it serves, as an enrolment does (planEnrolment).
// What a running service does with each change. An added repository is picked
// up between tasks. A removed one is not: the service will not restart into a
// config that takes a repository from a queue (checkConfigChange), so it
// keeps the old list until it is restarted.
const (
	addNote     = "A running Rein service picks this up between tasks and tells Elk."
	removalNote = "A running Rein service keeps the old list until it restarts: rein service stop, then rein service start\n" +
		"(stop interrupts a run in flight; its claim lapses and Elk hands it on)."
)

func changeRepos(cmd *cobra.Command, edit func(config.Config, config.Queue) (config.Config, config.Queue, error), note string) error {
	path, err := resolveConfigPath(cmd)
	if err != nil {
		return err
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		return err
	}
	cfg.Queues = append([]config.Queue(nil), cfg.Queues...)
	before := cfg
	q, err := pickQueue(cmd, cfg)
	if err != nil {
		return err
	}
	next, q, err := edit(cfg, q)
	if err != nil {
		return err
	}
	next.Queues = append([]config.Queue(nil), next.Queues...)
	next.SetQueue(q)
	skip := queueKey{next.WorkspaceFor(q), q.Name}
	next, pinned := keepOthersServed(before, next, skip)
	if err := checkReposStrandNothing(before, next, skip); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	var backup string
	if len(pinned) > 0 {
		if backup, err = backupConfig(path); err != nil {
			return err
		}
	}
	if err := next.SaveFile(path); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "queue   %s\n", next.QueueLabel(q))
	printEnrolRepos(out, next, q)
	printPinned(out, pinned, backup)
	fmt.Fprintln(out, note)
	return nil
}

// pickQueue is --queue, or the only queue there is.
func pickQueue(cmd *cobra.Command, cfg config.Config) (config.Queue, error) {
	name, _ := cmd.Flags().GetString("queue")
	if name != "" {
		q, ok := cfg.Queue(name)
		if !ok {
			return q, fmt.Errorf("no queue %q in this config (or more than one: name it as workspace/name)", name)
		}
		return q, nil
	}
	if len(cfg.Queues) == 1 {
		return cfg.Queues[0], nil
	}
	labels := make([]string, 0, len(cfg.Queues))
	for _, q := range cfg.Queues {
		labels = append(labels, cfg.QueueLabel(q))
	}
	if len(labels) == 0 {
		return config.Queue{}, fmt.Errorf("no queue is enrolled here; run rein enrol first")
	}
	return config.Queue{}, fmt.Errorf("this machine has several queues; name one with --queue: %s", strings.Join(labels, ", "))
}

// cloneRepo downloads name into into/<last segment>, or reuses the checkout
// already there.
func cloneRepo(cmd *cobra.Command, name, from, into string) (string, error) {
	if from == "" {
		if strings.Count(name, "/") != 1 {
			return "", fmt.Errorf("--clone needs owner/name to know where to download from, or --from")
		}
		from = "https://github.com/" + name + ".git"
	}
	dest := filepath.Join(expandHome(into), lastPathSegment(name))
	if _, err := os.Stat(dest); err == nil {
		return checkoutPath(dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	c := exec.CommandContext(cmd.Context(), "git", "clone", "--quiet", from, dest)
	if out, err := c.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone %s: %v\n%s", from, err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "cloned  %s into %s\n", from, dest)
	return checkoutPath(dest)
}

func lastPathSegment(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}
