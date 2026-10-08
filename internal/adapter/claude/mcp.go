package claude

import "github.com/elk-work/rein/internal/adapter"

// MCPServerConfig implements [adapter.MCPConfigurer]: one allow-listed server
// in the shape `--mcp-config` takes, the same as a `.mcp.json` entry
// (ark:rein#40).
//
// Secrets stay names. Claude Code expands `${VAR}` in a command-line MCP
// config — verified in 2.1.285, where the `--mcp-config` path is parsed with
// `expandVars: true` — so a bearer token becomes the header
// `Authorization: Bearer ${VAR}` and a stdio server's env passes `${VAR}`
// through, and the value is read by Claude Code from its own environment.
// A variable that is unset is Claude Code's "Missing environment variables"
// error, which is why the run loop does not declare a server whose names are
// missing in the first place.
func (a *Adapter) MCPServerConfig(s adapter.MCPServerSpec) (any, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.URL != "" {
		m := map[string]any{"type": "http", "url": s.URL}
		if s.BearerTokenEnvVar != "" {
			m["headers"] = map[string]any{"Authorization": "Bearer ${" + s.BearerTokenEnvVar + "}"}
		}
		return m, nil
	}
	args := s.Args
	if args == nil {
		args = []string{}
	}
	m := map[string]any{"type": "stdio", "command": s.Command, "args": args}
	if len(s.EnvVars) > 0 {
		env := make(map[string]any, len(s.EnvVars))
		for _, name := range s.EnvVars {
			env[name] = "${" + name + "}"
		}
		m["env"] = env
	}
	return m, nil
}
