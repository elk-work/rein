package codex

import "github.com/elk-work/rein/internal/adapter"

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
