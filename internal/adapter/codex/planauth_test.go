package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// replayWithAccount replays pong.jsonl with the recorded account/read reply
// replaced by result.
func replayWithAccount(t *testing.T, result string) (*Adapter, *peerRef) {
	t.Helper()
	script := loadScript(t, "testdata/pong.jsonl")
	found := false
	for i := range script {
		if script[i].kind == opReply && script[i].method == methodAccountRead {
			script[i].result = json.RawMessage(result)
			found = true
		}
	}
	if !found {
		t.Fatal("pong.jsonl has no account/read reply to replace")
	}
	a := New()
	a.heartbeat = time.Hour
	ref := &peerRef{}
	a.spawn = func(ctx context.Context, spec adapter.RunSpec) (*process, error) {
		p, proc := newPeer(t, script)
		ref.set(p)
		return proc, nil
	}
	return a, ref
}

func TestAPlanLoginStartsAndOthersAreStoppedBeforeAThread(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		ok           bool
	}{
		{"chatgpt", `{"account":{"type":"chatgpt","email":null,"planType":"pro"},"requiresOpenaiAuth":true}`, true},
		{"api key", `{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}`, false},
		{"bedrock", `{"account":{"type":"amazonBedrock"},"requiresOpenaiAuth":false}`, false},
		{"no login", `{"account":null,"requiresOpenaiAuth":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, ref := replayWithAccount(t, tc.result)
			sess, err := a.Start(context.Background(), baseSpec(adapter.PermissionFull))
			if tc.ok {
				if err != nil {
					t.Fatalf("a plan login was refused: %v", err)
				}
				drain(t, sess)
				return
			}
			if !errors.Is(err, adapter.ErrNotPlanAuth) {
				t.Fatalf("err = %v; want ErrNotPlanAuth", err)
			}
			if _, started := ref.clientCall(t, methodThreadStart); started {
				t.Error("a thread was started on a login that is not the plan")
			}
		})
	}
}

func TestStartRefusesAMeteredKeyInEnv(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "CODEX_API_KEY"} {
		a, _ := replayAdapter(t, "pong.jsonl")
		spec := baseSpec(adapter.PermissionFull)
		spec.Env = map[string]string{name: "sk-test-not-real"}
		if _, err := a.Start(context.Background(), spec); !errors.Is(err, adapter.ErrNotPlanAuth) {
			t.Errorf("%s: err = %v; want ErrNotPlanAuth", name, err)
		}
	}
}

func TestTheQueuesModelAndEffortReachTheTurn(t *testing.T) {
	a, ref := replayAdapter(t, "pong.jsonl")
	spec := baseSpec(adapter.PermissionFull)
	spec.Model, spec.Effort = "gpt-5.5-codex", "high"
	sess, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, sess)
	call, ok := ref.clientCall(t, methodTurnStart)
	if !ok {
		t.Fatal("no turn/start")
	}
	for _, want := range []string{`"effort":"high"`, `"model":"gpt-5.5-codex"`} {
		if !strings.Contains(string(call.Params), want) {
			t.Errorf("turn/start params %s do not carry %s", call.Params, want)
		}
	}

	a, ref = replayAdapter(t, "pong.jsonl")
	sess, err = a.Start(context.Background(), baseSpec(adapter.PermissionFull))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, sess)
	call, _ = ref.clientCall(t, methodTurnStart)
	if strings.Contains(string(call.Params), `"effort"`) {
		t.Errorf("an unset effort was still sent: %s", call.Params)
	}
}
