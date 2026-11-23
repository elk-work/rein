package cli

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runner"
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
				dir, err = runner.CheckoutPath(path)
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
				if have, ok := cfg.RepositoriesFor(q)[name]; ok {
					if filepath.Clean(expandHome(have)) == dir {
						return cfg, q, fmt.Errorf("queue %s already has %s, at %s; nothing changed", q.Name, name, have)
					}
					if !force {
						return cfg, q, fmt.Errorf("queue %s already has %s, at %s; nothing changed (pass --force to point it at %s)", q.Name, name, have, dir)
					}
				}
				return applyEnrolRepos(cfg, pinServed(cfg, q), enrolRepos{Repos: []namedRepo{{Name: name, Path: dir}}}, force)
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
				if _, ok := cfg.RepositoriesFor(q)[name]; !ok {
					return cfg, q, fmt.Errorf("queue %s does not have %s, so there is nothing to remove; it has %s",
						q.Name, name, orDash(strings.Join(servedNames(cfg, q), ", ")))
				}
				q = pinServed(cfg, q)
				var kept []string
				for _, r := range q.Repos {
					if r != name {
						kept = append(kept, r)
					}
				}
				if len(kept) == 0 {
					return cfg, q, fmt.Errorf("%s is queue %s's last repository, and a queue with none stops every run; add another first, or revoke the queue", name, q.Name)
				}
				q.Repos = kept
				if q.Repo == name {
					// A run that names none still needs a default.
					q.Repo = kept[0]
				}
				// Lists in one workspace combine, so a peer that lists it
				// would keep it reachable: say so rather than claim it gone.
				next := cfg
				next.Queues = append([]config.Queue(nil), cfg.Queues...)
				next.SetQueue(q)
				if _, still := next.RepositoriesFor(q)[name]; still {
					var via []string
					for _, peer := range cfg.Queues {
						if peer.Name != q.Name && cfg.WorkspaceFor(peer) == cfg.WorkspaceFor(q) && slices.Contains(peer.Repos, name) {
							via = append(via, cfg.QueueLabel(peer))
						}
					}
					return cfg, q, fmt.Errorf("queue %s would still reach %s through %s, which lists it in the same workspace; nothing changed",
						q.Name, name, orDash(strings.Join(via, ", ")))
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
	skip := runner.QueueKey{Workspace: next.WorkspaceFor(q), Name: q.Name}
	next, pinned := runner.KeepOthersServed(before, next, skip)
	if err := runner.CheckReposStrandNothing(before, next, skip); err != nil {
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
	dir, cloned, err := runner.CloneRepo(cmd.Context(), name, from, into, true)
	if cloned {
		fmt.Fprintf(cmd.OutOrStdout(), "cloned  %s into %s\n", firstNonEmpty(from, "https://github.com/"+name+".git"), dir)
	}
	return dir, err
}

// servedNames is what a run on q can be cut from today, sorted.
func servedNames(cfg config.Config, q config.Queue) []string {
	names := make([]string, 0)
	for name := range cfg.RepositoriesFor(q) {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// pinServed gives a queue with no repos list of its own an explicit list of
// what it serves today, before an add or remove edits that list. Without it,
// a hand-set-up queue that reached all of [repos] would be narrowed to only
// the repository just added, dropping every other one it had.
func pinServed(cfg config.Config, q config.Queue) config.Queue {
	if len(q.Repos) == 0 {
		q.Repos = servedNames(cfg, q)
	}
	return q
}
