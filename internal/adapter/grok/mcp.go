package grok

import (
	"fmt"

	"github.com/elk-work/rein/internal/adapter"
)

// MCPServerConfig implements [adapter.MCPConfigurer]: one allow-listed server
// as the ACP `McpServer` entry `session/new` takes, keyed by name the way
// [mcpServers] expects (ark:rein#40).
//
// Grok can take less than the other two, and says so rather than degrading:
//
//   - no stdio. `grok agent`'s initialize reports `mcpCapabilities: {http,
//     sse}` and no stdio, and a stdio entry earns "Invalid params" mid-handshake.
//   - no bearer token by name. ACP headers are literal `{name, value}` pairs,
//     so filling one would mean Rein reading the token out of its environment
//     and writing it into a protocol message — handling a credential, which
//     Rein does not do. A remote server that needs no header is fine.
//
// Refused servers are neither passed nor declared, so Elk does not route work
// that needs one to a Grok queue.
func (a *Adapter) MCPServerConfig(s adapter.MCPServerSpec) (any, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.Command != "" {
		return nil, fmt.Errorf("%w: MCP server %q is a local (stdio) server; `grok agent` takes http and sse only",
			adapter.ErrNotSupported, s.Name)
	}
	if s.BearerTokenEnvVar != "" {
		return nil, fmt.Errorf("%w: MCP server %q needs a bearer token; ACP takes header values literally, "+
			"and Rein does not read a credential to fill one", adapter.ErrNotSupported, s.Name)
	}
	return map[string]any{"type": "http", "url": s.URL, "headers": []any{}}, nil
}
