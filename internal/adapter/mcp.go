package adapter

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// The per-run MCP allow-list (ark:rein#40).
//
// A Rein-driven session sees NO MCP server unless Rein hands it one:
// ark:rein#21 and #22 cut every run off from the developer's own servers,
// because an inherited server is capability *as somebody else*. That left
// [RunSpec.MCPServers] as the only door, and nothing filled it — so no run
// could reach PostHog, Figma, or anything else a work order might need, and
// no queue could honestly declare that it could.
//
// The allow-list is that door, opened deliberately. A person names servers in
// config.toml in one vendor-neutral shape, [MCPServerSpec]; a queue names which
// of them its runs get; each adapter translates a spec into its own vendor's
// configuration through [MCPConfigurer] — or refuses one it cannot express —
// and the run loop declares each server that made it as the capability
// `mcp:<name>` (scout docs/specs/pace-dispatch.md §4.3), so Elk routes work
// that needs a server to a queue that has it.
//
// Secrets never pass through Rein. A spec carries the NAME of an environment
// variable — a bearer token for a remote server, a key for a local one — and
// each vendor binary reads the value from its own environment, the same way
// it reads its own login.

// MCPServerSpec is one MCP server, as config.toml describes it. Exactly one
// of URL and Command is set.
type MCPServerSpec struct {
	// Name is the key the vendor registers it under, and the `mcp:<name>`
	// capability it is declared as.
	Name string

	// URL is a remote server over streamable HTTP.
	URL string

	// BearerTokenEnvVar names an environment variable holding a bearer token
	// for URL. The value is read by the vendor binary, never by Rein.
	BearerTokenEnvVar string

	// Command and Args start a local server over stdio.
	Command string
	Args    []string

	// EnvVars names environment variables passed through to a stdio server
	// from Rein's own environment. Names only, never values.
	EnvVars []string
}

// MCPConfigurer is implemented by an [Adapter] that can express an
// [MCPServerSpec] in its vendor's own configuration shape — the opaque value
// [RunSpec.MCPServers] carries. It returns an error wrapping
// [ErrNotSupported] for a spec the vendor cannot take, and the run loop then
// neither passes nor declares that server.
type MCPConfigurer interface {
	MCPServerConfig(s MCPServerSpec) (any, error)
}

// mcpNameRE is the name a server may have: what every vendor accepts as a
// config key (Codex's is the narrowest), and what reads as a capability.
var mcpNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Validate reports the first thing wrong with a spec itself.
func (s MCPServerSpec) Validate() error {
	if !mcpNameRE.MatchString(s.Name) {
		return fmt.Errorf("MCP server name %q: letters, digits, _ and - only, at most 64", s.Name)
	}
	switch {
	case s.URL == "" && s.Command == "":
		return fmt.Errorf("MCP server %q: needs a url (remote) or a command (local)", s.Name)
	case s.URL != "" && s.Command != "":
		return fmt.Errorf("MCP server %q: has both a url and a command; pick one", s.Name)
	case s.URL != "" && (len(s.Args) > 0 || len(s.EnvVars) > 0):
		return fmt.Errorf("MCP server %q: args and env_vars belong to a command, not a url", s.Name)
	case s.Command != "" && s.BearerTokenEnvVar != "":
		return fmt.Errorf("MCP server %q: bearer_token_env_var belongs to a url, not a command", s.Name)
	}
	if s.URL != "" && !strings.HasPrefix(s.URL, "https://") && !strings.HasPrefix(s.URL, "http://") {
		return fmt.Errorf("MCP server %q: url %q is not http(s)", s.Name, s.URL)
	}
	return nil
}

// EnvNames is every environment variable name the spec depends on.
func (s MCPServerSpec) EnvNames() []string {
	out := append([]string(nil), s.EnvVars...)
	if s.BearerTokenEnvVar != "" {
		out = append(out, s.BearerTokenEnvVar)
	}
	return out
}

// MissingEnv returns the variables the spec names that this process does not
// have. A server whose key is not in the environment cannot authenticate, so
// declaring it would be claiming a capability the machine does not hold — the
// run loop leaves such a server out, and says which variable is missing.
//
// It checks presence only. The value is never read into anything.
func (s MCPServerSpec) MissingEnv() []string {
	var missing []string
	for _, name := range s.EnvNames() {
		if _, ok := os.LookupEnv(name); !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// MissingFrom returns the variables the spec names that has does not report.
// It is [MCPServerSpec.MissingEnv] for a scoped queue (ark:rein#48), whose
// runs see their secrets map and not the daemon's environment: a server there
// is checked against the map, because that is the only environment the
// session will actually have.
func (s MCPServerSpec) MissingFrom(has func(name string) bool) []string {
	var missing []string
	for _, name := range s.EnvNames() {
		if !has(name) {
			missing = append(missing, name)
		}
	}
	return missing
}

// Capability is the name the server is declared under: `mcp:<name>`.
func (s MCPServerSpec) Capability() string { return "mcp:" + s.Name }
