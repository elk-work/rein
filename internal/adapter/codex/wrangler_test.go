package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

func TestWranglerConnectorConfig(t *testing.T) {
	secret := "https://example.invalid/owner?token=private&claim=private"
	for _, cycle := range []bool{false, true} {
		a := New()
		calls := 0
		a.ResolveWranglerConnector = func(_ context.Context, account string) (string, error) {
			calls++
			if account != "workspace/owner" {
				t.Fatal("wrong owner binding")
			}
			return secret, nil
		}
		reached := errors.New("stop before launching")
		a.spawn = func(_ context.Context, spec adapter.RunSpec) (*process, error) {
			s := &session{spec: spec}
			servers := s.threadConfig()["mcp_servers"].(map[string]any)
			if cycle {
				if len(servers) != 1 || servers["elk"].(map[string]any)["url"] != secret {
					t.Fatal("wrong cycle servers")
				}
			} else if len(servers) != 1 || servers["other"] != true {
				t.Fatal("ordinary servers changed")
			}
			return nil, reached
		}
		spec := adapter.RunSpec{WorktreeDir: t.TempDir(), Prompt: "test", PermissionMode: adapter.PermissionFull, MCPServers: map[string]any{"other": true}}
		if cycle {
			spec.WranglerConnectorAccount = "workspace/owner"
		}
		_, err := a.Start(context.Background(), spec)
		if !errors.Is(err, reached) {
			t.Fatal("did not reach spawn")
		}
		if (calls == 1) != cycle {
			t.Fatal("connector resolved outside cycle")
		}
	}
}

func TestWranglerResolverFailuresArePrivate(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		a := New()
		a.ResolveWranglerConnector = func(context.Context, string) (string, error) {
			if invalid {
				return "http://example.invalid/private", nil
			}
			return "", errors.New("private")
		}
		a.spawn = func(context.Context, adapter.RunSpec) (*process, error) { t.Fatal("spawned"); return nil, nil }
		_, err := a.Start(context.Background(), adapter.RunSpec{WorktreeDir: t.TempDir(), Prompt: "test", PermissionMode: adapter.PermissionFull, WranglerConnectorAccount: "owner"})
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("unsafe resolver failure")
		}
	}
}

func TestWranglerEventsAndStartupErrorsAreScrubbed(t *testing.T) {
	secret := "https://example.invalid/owner?token=private&claim=private"
	s := &session{connectorURL: secret, a: New(), proc: &process{stderrTail: newTail(4)}}
	raw, _ := json.Marshal(map[string]string{"output": secret})
	s.emit(adapter.Event{Kind: adapter.EventError, Text: secret, Raw: raw, Err: errors.New(secret)})
	blob, _ := json.Marshal(s.buf[0])
	if strings.Contains(string(blob), "private") || strings.Contains(s.buf[0].Err.Error(), "private") {
		t.Fatal("event leaked")
	}
	s.proc.stderrTail.lines = []string{secret}
	if strings.Contains(s.redact(s.startupError("thread/start", errors.New(secret)).Error()), "private") {
		t.Fatal("startup error leaked")
	}
	for _, value := range []string{secret, strings.ReplaceAll(string(raw), "/", `\/`)} {
		if strings.Contains(s.redact(value), "private") {
			t.Fatal("escaped URL leaked")
		}
	}
}

func TestWranglerConnectorReachesThreadStart(t *testing.T) {
	a, ref := replayAdapter(t, "pong.jsonl")
	secret := "https://example.invalid/owner?token=private&claim=private"
	a.ResolveWranglerConnector = func(context.Context, string) (string, error) { return secret, nil }
	spec := baseSpec(adapter.PermissionFull)
	spec.WranglerConnectorAccount = "owner"
	spec.MCPServers = map[string]any{"other": true}
	sess, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatal("cycle failed to start")
	}
	drain(t, sess)
	call, ok := ref.clientCall(t, methodThreadStart)
	if !ok {
		t.Fatal("no thread/start")
	}
	var params struct {
		Config struct {
			Servers map[string]struct{ URL string } `json:"mcp_servers"`
		}
	}
	if json.Unmarshal(call.Params, &params) != nil || len(params.Config.Servers) != 1 || params.Config.Servers["elk"].URL != secret {
		t.Fatal("thread/start did not carry exactly the owner connector")
	}
}
