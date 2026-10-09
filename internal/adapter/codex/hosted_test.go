package codex

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/elk-work/rein/internal/adapter"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostedAccountRequiresAPIKey(t *testing.T) {
	for _, kind := range []string{"apiKey", "chatgpt", "none", ""} {
		var response accountReadResponse
		if err := json.Unmarshal([]byte(`{"account":{"type":"`+kind+`"}}`), &response); err != nil {
			t.Fatal(err)
		}
		err := checkPlanAccount(response, true)
		if kind == "apiKey" {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, adapter.ErrNotAPIAuth) {
			t.Fatalf("kind %q accepted: %v", kind, err)
		}
	}
}
func TestHostedCodexHomeNeedsNoSubscriptionFile(t *testing.T) {
	t.Setenv("REIN_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "absent"))
	h, err := newHome("hosted", false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer h.remove()
	t.Cleanup(func() {
		if _, err := os.Stat(h.dir); !os.IsNotExist(err) {
			t.Error("hosted private home not removed")
		}
	})
	if _, err := os.Lstat(filepath.Join(h.dir, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("hosted home inherited authentication")
	}
}

func TestHostedSessionLogsInWithKeyBeforeCheckingAccount(t *testing.T) {
	for _, kind := range []string{"apiKey", "chatgpt", "none"} {
		t.Run(kind, func(t *testing.T) {
			script := loadScript(t, "testdata/pong.jsonl")
			for i := range script {
				if script[i].kind == opReply && script[i].method == methodAccountRead {
					script[i].result = json.RawMessage(`{"account":{"type":"` + kind + `"}}`)
					login := opStep{kind: opReply, method: "account/login/start", result: json.RawMessage(`{"type":"apiKey"}`)}
					script = append(script[:i], append([]opStep{login}, script[i:]...)...)
					break
				}
			}
			a := New()
			a.heartbeat = time.Hour
			ref := &peerRef{}
			a.spawn = func(context.Context, adapter.RunSpec) (*process, error) {
				p, proc := newPeer(t, script)
				ref.set(p)
				return proc, nil
			}
			spec := baseSpec(adapter.PermissionFull)
			spec.Hosted = true
			spec.Env = map[string]string{"CODEX_API_KEY": "synthetic-model-key"}
			sess, err := a.Start(context.Background(), spec)
			if kind == "apiKey" {
				if err != nil {
					t.Fatal(err)
				}
				drain(t, sess)
			} else {
				if !errors.Is(err, adapter.ErrNotAPIAuth) {
					t.Fatalf("err=%v", err)
				}
				if _, started := ref.clientCall(t, methodThreadStart); started {
					t.Fatal("thread started without API account")
				}
			}
			call, ok := ref.clientCall(t, "account/login/start")
			if !ok {
				t.Fatal("key login omitted")
			}
			var params map[string]string
			if err := json.Unmarshal(call.Params, &params); err != nil {
				t.Fatal(err)
			}
			if params["type"] != "apiKey" || params["apiKey"] != "synthetic-model-key" {
				t.Fatal("wrong login")
			}
		})
	}
}
