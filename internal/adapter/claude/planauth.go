package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/secretenv"
)

// Plan login only. A Rein run draws on the developer's own Claude plan, the
// same limits the terminal uses; anything that would put it on metered API
// billing instead is refused, twice:
//
//   - before the process starts, against what Rein can see: the spec's Env
//     ([adapter.RunSpec.CheckPlanEnv]) and the settings files the session will
//     load ([checkSettingsAuth]) — an `apiKeyHelper`, or an `env` block that
//     sets a metered variable;
//   - once it has started, against what Claude Code itself says: the
//     `apiKeySource` on its system/init line must be [planAPIKeySource]. Any
//     other value stops the session before its first tool call
//     ([Session.run]).
//
// The second check is the guarantee and the first is the kinder message: a
// managed settings file or some source nobody thought of still names itself
// on the init line.

// planAPIKeySource is the init line's `apiKeySource` on a subscription login:
// no API key from anywhere. Every Rein run on record shows it.
const planAPIKeySource = "none"

// checkPlanInit refuses an init line whose apiKeySource is anything but the
// plan login, including an absent one: a line that does not say is not
// evidence of the plan.
func checkPlanInit(apiKeySource string) error {
	if apiKeySource == planAPIKeySource {
		return nil
	}
	got := apiKeySource
	if got == "" {
		got = "(not reported)"
	}
	return &adapter.PlanAuthError{Because: "Claude Code's start-up line reports apiKeySource " +
		strconv.Quote(got) + ", not " + strconv.Quote(planAPIKeySource) + " (the subscription login)"}
}

// settingsFiles maps the --setting-sources this session loads to the files
// they read. Only the sources named are checked, because only they apply.
func settingsFiles(worktree, sources string) []string {
	var out []string
	for _, src := range strings.Split(sources, ",") {
		switch strings.TrimSpace(src) {
		case "project":
			out = append(out, filepath.Join(worktree, ".claude", "settings.json"))
		case "local":
			out = append(out, filepath.Join(worktree, ".claude", "settings.local.json"))
		case "user":
			if home, err := os.UserHomeDir(); err == nil {
				out = append(out, filepath.Join(home, ".claude", "settings.json"))
			}
		}
	}
	return out
}

// checkSettingsAuth refuses a session whose settings would supply an API key:
// an `apiKeyHelper`, or an `env` block naming a metered variable. A file that
// is missing or does not parse is passed over — Claude Code would not load
// one that does not parse either, and the init line is the backstop.
//
// It reads names only. No value from the file is put in the error.
func checkSettingsAuth(worktree, sources string) error {
	for _, path := range settingsFiles(worktree, sources) {
		blob, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var st struct {
			APIKeyHelper string         `json:"apiKeyHelper"`
			Env          map[string]any `json:"env"`
		}
		if json.Unmarshal(blob, &st) != nil {
			continue
		}
		where := path
		if rel, err := filepath.Rel(worktree, path); err == nil && !strings.HasPrefix(rel, "..") {
			where = filepath.ToSlash(rel) + " in the repository"
		}
		if strings.TrimSpace(st.APIKeyHelper) != "" {
			return &adapter.PlanAuthError{Because: where + " sets apiKeyHelper, which makes Claude Code " +
				"bill an API key instead of the subscription; remove it from that file"}
		}
		var bad []string
		for name := range st.Env {
			if secretenv.Metered(name) {
				bad = append(bad, name)
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			return &adapter.PlanAuthError{Because: where + " sets " + strings.Join(bad, ", ") +
				" in its env block, which would switch Claude Code to metered API billing"}
		}
	}
	return nil
}
