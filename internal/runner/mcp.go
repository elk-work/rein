package runner

import (
	"errors"
	"sort"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
)

// The per-run MCP allow-list, as the run loop applies it (ark:rein#40; the
// shape and the reasoning are in internal/adapter/mcp.go).
//
// Resolved once per queue at startup, because nothing in it changes between
// runs: the config is read once, the adapter's translation is pure, and the
// environment a service was started with is the one it keeps. What comes out
// is two things that must agree — the servers every run on the queue is
// handed, and the `mcp:<name>` capabilities the queue declares — so they are
// built together, from the same list, and a server is in both or in neither.

// mcpAllowList is one queue's resolved allow-list.
type mcpAllowList struct {
	// servers is RunSpec.MCPServers for every run on the queue, in the
	// adapter's own shape. Nil when the queue allows none.
	servers map[string]any

	// declared is the `mcp:<name>` capability for each server in servers.
	declared []string

	// envNames is every environment variable the servers in servers name. A
	// queue without a secrets map passes these through to its runs, since the
	// agent process starts the servers and they read their keys from it.
	envNames []string

	// skipped says, per server the queue named but will not get, why — for
	// the startup log, because a server that silently vanished from a run is
	// the kind of thing someone spends an afternoon on.
	skipped []string
}

// resolveMCP builds a queue's allow-list. A server is left out, and said so,
// when the queue's adapter cannot express it (Grok and a stdio server), when
// the adapter cannot take MCP servers at all, or when this process does not
// have an environment variable the server names — declaring a server whose
// key the machine does not hold would be claiming a capability it lacks. For a
// scoped queue "this process" is the queue's secrets map.
func resolveMCP(cfg config.Config, q config.Queue, a adapter.Adapter) mcpAllowList {
	var out mcpAllowList
	if len(q.MCPServers) == 0 {
		return out
	}
	conf, ok := a.(adapter.MCPConfigurer)
	switch {
	case !ok:
		out.skipped = append(out.skipped, "all of "+strings.Join(q.MCPServers, ", ")+
			": the "+a.Name()+" adapter cannot express an MCP server")
		return out
	case !a.Manifest().Has(adapter.CapMCP):
		out.skipped = append(out.skipped, "all of "+strings.Join(q.MCPServers, ", ")+
			": the "+a.Name()+" adapter does not declare mcp")
		return out
	}

	names := append([]string(nil), q.MCPServers...)
	sort.Strings(names)
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		def, ok := cfg.MCPServers[name]
		if !ok {
			// config.Validate refuses this; kept so a hand-built config in a
			// test cannot turn into a nil dereference.
			out.skipped = append(out.skipped, name+": no [mcp_servers."+name+"] definition")
			continue
		}
		spec := adapter.MCPServerSpec{
			Name: name, URL: def.URL, BearerTokenEnvVar: def.BearerTokenEnvVar,
			Command: def.Command, Args: def.Args, EnvVars: def.EnvVars,
		}
		if q.Scoped() {
			// A scoped queue's runs see their secrets map, not this process's
			// environment (secrets.go), so that is what a server is checked
			// against — and a server keyed on something outside the map is
			// left out rather than handed a variable the run will not have.
			if missing := spec.MissingFrom(func(n string) bool { _, ok := q.Secrets[n]; return ok }); len(missing) > 0 {
				out.skipped = append(out.skipped, name+": "+strings.Join(missing, ", ")+
					" is not in this queue's secrets map, and a scoped queue's MCP servers see only that")
				continue
			}
		} else if missing := spec.MissingEnv(); len(missing) > 0 {
			out.skipped = append(out.skipped, name+": this process has no "+strings.Join(missing, ", ")+
				" in its environment (a service keeps the environment it was installed with)")
			continue
		}
		v, err := conf.MCPServerConfig(spec)
		if err != nil {
			why := err.Error()
			if errors.Is(err, adapter.ErrNotSupported) {
				why = strings.TrimPrefix(why, adapter.ErrNotSupported.Error()+": ")
			}
			out.skipped = append(out.skipped, name+": "+why)
			continue
		}
		if out.servers == nil {
			out.servers = map[string]any{}
		}
		out.servers[name] = v
		out.declared = append(out.declared, spec.Capability())
		out.envNames = append(out.envNames, spec.EnvNames()...)
	}
	return out
}

// mcpServersForRun is a fresh copy of the queue's servers for one RunSpec, so
// no adapter can reach into another run's map.
func (qr *queueRunner) mcpServersForRun() map[string]any {
	if len(qr.mcp.servers) == 0 {
		return nil
	}
	out := make(map[string]any, len(qr.mcp.servers))
	for k, v := range qr.mcp.servers {
		out[k] = v
	}
	return out
}
