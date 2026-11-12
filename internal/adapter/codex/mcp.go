package codex

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
)

// MCPServerConfig implements [adapter.MCPConfigurer]: one allow-listed server
// as a `mcp_servers.<name>` table, the shape `thread/start` takes in
// `config.mcp_servers` (threadConfig) and `~/.codex/config.toml` takes on disk
// (ark:rein#40).
//
// Codex has a field for each secret-by-name: `bearer_token_env_var` for a
// streamable-HTTP server (`codex mcp add --bearer-token-env-var`) and
// `env_vars` for a stdio one — both confirmed in codex-cli 0.159.0's
// `RawMcpServerConfig`. Codex reads the values from its own environment; Rein
// only ever writes the names.
func (a *Adapter) MCPServerConfig(s adapter.MCPServerSpec) (any, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.URL != "" {
		m := map[string]any{"url": s.URL}
		if s.BearerTokenEnvVar != "" {
			m["bearer_token_env_var"] = s.BearerTokenEnvVar
		}
		return m, nil
	}
	args := s.Args
	if args == nil {
		args = []string{}
	}
	m := map[string]any{"command": s.Command, "args": args}
	if len(s.EnvVars) > 0 {
		m["env_vars"] = append([]string(nil), s.EnvVars...)
	}
	return m, nil
}

// MCPToolName implements adapter.MCPToolDenier. Codex 0.159.0 qualifies
// MCP tools as mcp__<server>__<tool>. Restrict this to unambiguous names
// that fit its 64-character limit; longer names are rewritten by Codex.
func (a *Adapter) MCPToolName(server, tool string) (string, bool) {
	name := "mcp__" + server + "__" + tool
	return name, mcpIdentifier.MatchString(server) && !strings.Contains(server, "__") && !strings.HasSuffix(server, "_") &&
		mcpIdentifier.MatchString(tool) && len(name) <= 64
}

var mcpIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func splitMCPTool(name string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, "mcp__")
	if !found {
		return "", "", false
	}
	server, tool, found = strings.Cut(rest, "__")
	if !found {
		return "", "", false
	}
	_, ok = (*Adapter)(nil).MCPToolName(server, tool)
	return
}

// Built-in tools and allow lists still have no verified enforcement. MCP
// denials are supported only for servers actually supplied to this session.
func validateToolRestrictions(spec adapter.RunSpec) error {
	if len(spec.AllowedTools) > 0 {
		return fmt.Errorf("%w: codex does not support AllowedTools", adapter.ErrNotSupported)
	}
	for _, name := range spec.DeniedTools {
		server, _, ok := splitMCPTool(name)
		_, configured := spec.MCPServers[server].(map[string]any)
		if !ok || !configured {
			return fmt.Errorf("%w: codex DeniedTools requires a qualified tool on a configured MCP server", adapter.ErrNotSupported)
		}
	}
	return nil
}
