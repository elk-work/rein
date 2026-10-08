package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/runlog"
)

func TestWranglerConnectorPrivateConfigAndSecrecy(t *testing.T) {
	secret := "https://example.invalid/owner?key=private&x=1"
	a := &Adapter{DisableWatchdog: true, ResolveWranglerConnector: func(_ context.Context, name string) (string, error) {
		if name != "workspace/queue" {
			t.Fatal("wrong owner binding")
		}
		return secret, nil
	}}
	worktree := t.TempDir()
	s := testSession(t)
	args, err := a.buildArgs(adapter.RunSpec{WranglerConnectorAccount: "workspace/queue", WorktreeDir: worktree, MCPServers: map[string]any{"other": true}}, "plan", s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), secret) {
		t.Fatal("secret in argv")
	}
	path := filepath.Join(s.tmpDir, "mcp.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("unsafe permissions")
	}
	if strings.HasPrefix(path, worktree+string(os.PathSeparator)) {
		t.Fatal("config in worktree")
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Servers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(blob, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Servers) != 1 || config.Servers["elk"].URL != secret {
		t.Fatal("incorrect MCP config")
	}
	// The same decoded event used by the runner is safe even when Claude echoes
	// the connector in tool output, including JSON's escaped ampersand.
	line, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": secret}}}})
	events, _, _ := s.dec.decode([]byte(s.redact(string(line))))
	logDir := t.TempDir()
	logs := &runlog.Store{Dir: logDir}
	writer := logs.Create(runlog.Meta{RunID: "wrangler-test"})
	if len(events) == 0 {
		t.Fatal("no events exercised")
	}
	for _, ev := range events {
		writer.Event(ev)
	}
	writer.Close(runlog.StatusSubmitted)
	if writer.Err() != nil {
		t.Fatal(writer.Err())
	}
	logged, err := os.ReadFile(writer.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "private") {
		t.Fatal("secret in run log")
	}
	entries, err := os.ReadDir(worktree)
	if err != nil || len(entries) != 0 {
		t.Fatal("secret material in worktree")
	}
	if strings.Contains(s.redact(secret), secret) {
		t.Fatal("stderr not redacted")
	}
	s.cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("config survived cleanup")
	}
}

func TestWranglerResolverErrorDoesNotLeak(t *testing.T) {
	a := &Adapter{ResolveWranglerConnector: func(context.Context, string) (string, error) { return "", errors.New("private secret") }}
	_, err := a.buildArgs(adapter.RunSpec{WranglerConnectorAccount: "owner"}, "plan", testSession(t))
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("resolver failure leaked")
	}
}

func TestWranglerHookAndErrorEventsAreScrubbed(t *testing.T) {
	s := &Session{connectorURL: "https://example.invalid/private"}
	raw, _ := json.Marshal(map[string]string{"output": s.connectorURL})
	ev := s.redactEvent(adapter.Event{Kind: adapter.EventError, Text: s.connectorURL, Raw: raw, Err: errors.New(s.connectorURL)})
	blob, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "private") || ev.Err == nil || strings.Contains(ev.Err.Error(), "private") {
		t.Fatal("hook or error leaked")
	}
}
