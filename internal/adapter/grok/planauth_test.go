package grok

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

func TestCheckPlanAuthMethod(t *testing.T) {
	for id, ok := range map[string]bool{"cached_token": true, "grok.com": true, "api_key": false, "": false} {
		err := checkPlanAuthMethod(id)
		if ok != (err == nil) {
			t.Errorf("%q: err = %v", id, err)
		}
		if err != nil && !errors.Is(err, adapter.ErrNotPlanAuth) {
			t.Errorf("%q: err %v is not ErrNotPlanAuth", id, err)
		}
	}
}

// The initialize reply naming another auth method stops the session before
// session/new.
func TestANonPlanAuthMethodIsStoppedBeforeASession(t *testing.T) {
	script := loadScript(t, "testdata/pong.jsonl")
	found := false
	for i := range script {
		if script[i].kind != opReply || script[i].method != methodInitialize {
			continue
		}
		var res map[string]any
		if err := json.Unmarshal(script[i].result, &res); err != nil {
			t.Fatal(err)
		}
		meta, _ := res["_meta"].(map[string]any)
		if meta == nil || meta["defaultAuthMethodId"] != "cached_token" {
			t.Fatalf("the recorded initialize does not say cached_token: %v", meta)
		}
		meta["defaultAuthMethodId"] = "api_key"
		script[i].result, _ = json.Marshal(res)
		found = true
	}
	if !found {
		t.Fatal("pong.jsonl has no initialize reply")
	}
	a := New()
	a.heartbeat = time.Hour
	ref := &peerRef{}
	a.spawn = func(ctx context.Context, spec adapter.RunSpec) (*process, error) {
		p, proc := newPeer(t, script)
		ref.set(p)
		return proc, nil
	}
	_, err := a.Start(context.Background(), baseSpec(adapter.PermissionFull))
	if !errors.Is(err, adapter.ErrNotPlanAuth) {
		t.Fatalf("err = %v; want ErrNotPlanAuth", err)
	}
	if _, ok := ref.get(t).clientCall(methodSessionNew); ok {
		t.Error("a session was opened on a login that is not the plan")
	}
}

func TestStartRefusesXAIKeyInEnv(t *testing.T) {
	a, _ := replayAdapter(t, "pong.jsonl")
	spec := baseSpec(adapter.PermissionFull)
	spec.Env = map[string]string{"XAI_API_KEY": "xai-test-not-real"}
	if _, err := a.Start(context.Background(), spec); !errors.Is(err, adapter.ErrNotPlanAuth) {
		t.Fatalf("err = %v; want ErrNotPlanAuth", err)
	}
}

func TestAgentArgsCarryModelAndEffort(t *testing.T) {
	got := agentArgs(adapter.RunSpec{Model: "grok-4.7", Effort: "xhigh"})
	want := []string{"agent", "--model", "grok-4.7", "--reasoning-effort", "xhigh", "stdio"}
	if !slices.Equal(got, want) {
		t.Errorf("args = %v; want %v", got, want)
	}
	if got := agentArgs(adapter.RunSpec{}); !slices.Equal(got, []string{"agent", "stdio"}) {
		t.Errorf("an unset model/effort changed the command line: %v", got)
	}
}
