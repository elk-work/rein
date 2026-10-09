package adapter

import (
	"errors"
	"testing"
)

func TestHostedEnvironmentRequiresVendorKeyAndRefusesCloudAuth(t *testing.T) {
	for _, tc := range []struct {
		kind string
		env  map[string]string
		ok   bool
	}{
		{"claude", map[string]string{"ANTHROPIC_API_KEY": "test-key"}, true},
		{"claude", map[string]string{"OPENAI_API_KEY": "test-key"}, false},
		{"codex", map[string]string{"OPENAI_API_KEY": "test-key"}, true},
		{"codex", map[string]string{"CODEX_API_KEY": "test-key"}, true},
		{"codex", nil, false},
		{"claude", map[string]string{"ANTHROPIC_API_KEY": "test-key", "ANTHROPIC_AUTH_TOKEN": "other"}, false},
		{"claude", map[string]string{"ANTHROPIC_API_KEY": "test-key", "CLAUDE_CODE_USE_VERTEX": "1"}, false},
	} {
		err := (RunSpec{Hosted: true, AgentKind: tc.kind, Env: tc.env}).CheckPlanEnv()
		if tc.ok && err != nil || !tc.ok && !errors.Is(err, ErrNotAPIAuth) {
			t.Fatalf("kind %s ok=%v err=%v", tc.kind, tc.ok, err)
		}
	}
}
