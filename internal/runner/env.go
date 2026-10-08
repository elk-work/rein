package runner

import (
	"sort"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/secretenv"
)

// Every run's environment is scoped. A queue with a secrets map gets the
// system variables and its map (secrets.go). A queue without one gets the
// system variables and the daemon variables it names, by one of:
//
//   - inherit_env, top-level and per queue, for anything else a build needs;
//   - a credential-shaped capability (`SUPABASE_SERVICE_ROLE_KEY`), which
//     already said "this machine holds it in its environment";
//   - an allowed MCP server's key, because the agent starts the server.
//
// Until the plan-login change such a queue inherited the daemon's whole
// environment. Under the service that was little more than PATH and HOME; a
// `rein run` started by hand handed every run the developer's shell, API keys
// included, and `claude -p` bills ANTHROPIC_API_KEY whenever it is present.
// A metered-billing variable is dropped whatever names it
// ([secretenv.Filter]), and config refuses one named anywhere.

// passEnv is the queue's named passthrough, sorted and without duplicates.
// withMCP includes the MCP servers' keys: false for a run handed no servers.
func (qr *queueRunner) passEnv(withMCP bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name == "" || seen[name] || secretenv.Metered(name) {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, n := range qr.r.opts.Config.InheritEnv {
		add(n)
	}
	for _, n := range qr.q.InheritEnv {
		add(n)
	}
	if qr.host != nil {
		for _, n := range qr.host.Names() {
			if config.LooksLikeEnvName(n) {
				add(n)
			}
		}
	}
	if withMCP {
		for _, n := range qr.mcp.envNames {
			add(n)
		}
	}
	sort.Strings(out)
	return out
}

// orCLIDefault names an unset model or effort for the startup log.
func orCLIDefault(v string) string {
	if v == "" {
		return "(CLI default)"
	}
	return v
}
