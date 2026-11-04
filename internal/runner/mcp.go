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

	// connector is true when servers carries the Elk run connector: the
	// server named elk, on the queue's own run-scoped token (ark:rein#112).
	connector bool

	// denied is RunSpec.DeniedTools for a run that gets the run connector:
	// Rein's own Elk tools, in the adapter's naming.
	denied []string
}

// runConnectorUnavailable says why an adapter cannot be handed the Elk run
// connector, or "" when it can. It must express a remote MCP server with a
// bearer token and deny that server's tools by name; without the second, the
// session could submit its own deliverable or rotate the queue's token
// (adapter.RunConnectorDeniedTools).
func runConnectorUnavailable(a adapter.Adapter) string {
	if _, ok := a.(adapter.MCPConfigurer); !ok || !a.Manifest().Has(adapter.CapMCP) {
		return "the " + a.Name() + " adapter cannot take an MCP server"
	}
	d, ok := a.(adapter.MCPToolDenier)
	if !ok {
		return "the " + a.Name() + " adapter cannot keep Rein's own Elk tools out of a session"
	}
	if _, ok := d.MCPToolName(adapter.RunConnectorName, "submit_deliverable"); !ok {
		return "the " + a.Name() + " adapter cannot keep Rein's own Elk tools out of a session"
	}
	return ""
}

// addRunConnector gives every run on the queue the Elk run connector
// (ark:rein#112): the MCP server elk at the configured Elk endpoint, whose
// bearer token is the queue's own run-scoped token. The run loop puts the
// token in the session's environment under adapter.RunConnectorTokenEnv, so
// no file and no argument ever holds it.
//
// It is the queue's credential, never a person's: bound to one workspace and
// one executor, tier run-scoped, which is what ark:rein#21 requires of
// anything a run holds. And it is the credential Rein itself claims and
// reports with, which is why Rein's own tools on it are denied to the session.
func (l *mcpAllowList) addRunConnector(endpoint string, a adapter.Adapter) {
	if endpoint == "" {
		return
	}
	if why := runConnectorUnavailable(a); why != "" {
		l.skipped = append(l.skipped, adapter.RunConnectorName+" (Rein's Elk connector on this queue's token): "+why)
		return
	}
	spec := adapter.MCPServerSpec{
		Name: adapter.RunConnectorName, URL: endpoint, BearerTokenEnvVar: adapter.RunConnectorTokenEnv,
	}
	v, err := a.(adapter.MCPConfigurer).MCPServerConfig(spec)
	if err != nil {
		l.skipped = append(l.skipped, adapter.RunConnectorName+" (Rein's Elk connector on this queue's token): "+err.Error())
		return
	}
	d := a.(adapter.MCPToolDenier)
	denied := make([]string, 0, len(adapter.RunConnectorDeniedTools))
	for _, tool := range adapter.RunConnectorDeniedTools {
		name, _ := d.MCPToolName(adapter.RunConnectorName, tool)
		denied = append(denied, name)
	}
	if l.servers == nil {
		l.servers = map[string]any{}
	}
	l.servers[adapter.RunConnectorName] = v
	l.declared = append(l.declared, spec.Capability())
	sort.Strings(l.declared)
	l.connector = true
	l.denied = denied
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
		if name == adapter.RunConnectorName && runConnectorUnavailable(a) == "" {
			// The name is the run connector's on an adapter that gets one
			// (addRunConnector): Elk on the queue's own token, never on a
			// credential config.toml names, which may be a person's.
			out.skipped = append(out.skipped, name+": the name is Rein's own Elk connector, on this queue's token "+
				"(ark:rein#112); rename the [mcp_servers."+name+"] entry to keep it")
			continue
		}
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

// withOwnToken is values plus the queue's own token, for a redactor: the token
// Rein claims and reports with never reaches a run log, whether or not the run
// holds the Elk run connector (ark:rein#112). values is not modified.
func (qr *queueRunner) withOwnToken(values map[string]string) map[string]string {
	if qr.elkToken == "" {
		return values
	}
	out := make(map[string]string, len(values)+1)
	for k, v := range values {
		out[k] = v
	}
	if _, taken := out[adapter.RunConnectorTokenEnv]; !taken {
		out[adapter.RunConnectorTokenEnv] = qr.elkToken
	}
	return out
}

// runConnectorNote tells a run that holds the Elk run connector what it is
// for and what it is not (ark:rein#112). The ordinary prompt already says not
// to call Rein's own tools; on this server they are not offered at all.
const runConnectorNote = "## Elk\n\n" +
	"This run has an Elk connector: the MCP server `elk`, acting as this queue's agent in its own workspace. " +
	"Use it when the task asks you to read or file work in Elk: list and search the List, create, link and " +
	"comment on items, capture to Elk, send work to an agent. It is not a person's connector, so a call that " +
	"needs a person present is refused; say so in your deliverable rather than looking for another way in. " +
	"Rein's own tools (claiming, progress, submitting) are not offered on it: Rein reports this run for you."
