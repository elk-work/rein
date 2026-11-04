package gemini

import (
	"fmt"

	"github.com/elk-work/rein/internal/adapter"
)

// MCPServerConfig implements [adapter.MCPConfigurer]: one allow-listed server
// as a Gemini CLI `mcpServers` entry, written into the run's private settings
// (home.go, ark:rein#40).
//
// Secrets stay names. Gemini CLI expands `$NAME` in its settings from its own
// environment, so a bearer token is `Authorization: Bearer ${NAME}` and a
// local server's variables are `${NAME}` — the value is read by Gemini CLI,
// never by Rein.
//
// The server connects only in a folder Gemini CLI trusts; every run trusts its
// worktree (buildArgs, childEnv; ark:rein#119).
func (a *Adapter) MCPServerConfig(s adapter.MCPServerSpec) (any, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.URL != "" {
		m := map[string]any{"httpUrl": s.URL}
		if s.BearerTokenEnvVar != "" {
			m["headers"] = map[string]any{"Authorization": "Bearer ${" + s.BearerTokenEnvVar + "}"}
		}
		return m, nil
	}
	if s.Command == "" {
		return nil, fmt.Errorf("%w: MCP server %q has neither a url nor a command", adapter.ErrNotSupported, s.Name)
	}
	m := map[string]any{"command": s.Command}
	if len(s.Args) > 0 {
		m["args"] = append([]string(nil), s.Args...)
	}
	if len(s.EnvVars) > 0 {
		env := map[string]any{}
		for _, name := range s.EnvVars {
			env[name] = "${" + name + "}"
		}
		m["env"] = env
	}
	return m, nil
}
