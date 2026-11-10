package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

func expiredLogin(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "oauth session expired and could not be refreshed") ||
		strings.Contains(text, "login expired")
}

// A stored login is not proof of recovery after a vendor authentication failure.
// Verify once at startup and again after a failure; healthy heartbeat checks never do.
// No tools, hooks, project, MCP servers or saved conversation participate.
func (a *Adapter) verifyLogin(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "rein-claude-auth-")
	if err != nil {
		return fmt.Errorf("%w: cannot verify Claude login", adapter.ErrPreflight)
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, path, "-p", "Reply OK.", "--output-format", "json",
		"--tools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--setting-sources", "", "--settings", `{"disableAllHooks":true}`, "--no-session-persistence", "--max-turns", "1")
	cmd.Dir = dir
	cmd.Env = (adapter.RunSpec{}).InheritedEnv()
	out, err := cmd.Output()
	var result struct {
		Type    string   `json:"type"`
		Subtype string   `json:"subtype"`
		IsError bool     `json:"is_error"`
		Result  string   `json:"result"`
		Errors  []string `json:"errors"`
	}
	decodeErr := json.Unmarshal(out, &result)
	if err != nil || decodeErr != nil || result.Type != "result" || result.Subtype != "success" || result.IsError {
		var exit *exec.ExitError
		expired := expiredLogin(result.Result + strings.Join(result.Errors, " "))
		if errors.As(err, &exit) {
			expired = expired || expiredLogin(string(exit.Stderr))
		}
		if expired {
			return fmt.Errorf("%w: %w: Claude sign-in expired; run `claude auth login`", adapter.ErrPreflight, adapter.ErrLoginRequired)
		}
		return fmt.Errorf("%w: Claude readiness could not be verified", adapter.ErrPreflight)
	}
	return nil
}
