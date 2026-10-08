package adapter

import (
	"errors"
	"strings"
	"testing"
)

func TestMCPServerSpecValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    MCPServerSpec
		wantErr string
	}{
		{"remote", MCPServerSpec{Name: "posthog", URL: "https://mcp.posthog.com/mcp", BearerTokenEnvVar: "K"}, ""},
		{"local", MCPServerSpec{Name: "docs_2", Command: "npx", Args: []string{"-y", "x"}, EnvVars: []string{"K"}}, ""},
		{"bad name", MCPServerSpec{Name: "has space", URL: "https://x"}, "letters, digits"},
		{"neither", MCPServerSpec{Name: "x"}, "needs a url"},
		{"both", MCPServerSpec{Name: "x", URL: "https://x", Command: "y"}, "both"},
		{"args on a url", MCPServerSpec{Name: "x", URL: "https://x", Args: []string{"a"}}, "belong to a command"},
		{"bearer on a command", MCPServerSpec{Name: "x", Command: "y", BearerTokenEnvVar: "K"}, "belongs to a url"},
		{"not http", MCPServerSpec{Name: "x", URL: "file:///etc/passwd"}, "not http"},
	} {
		err := tc.spec.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want one mentioning %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestMCPServerSpecMissingEnvChecksPresenceOnly(t *testing.T) {
	t.Setenv("REIN_TEST_MCP_PRESENT", "")
	s := MCPServerSpec{Name: "x", Command: "y", EnvVars: []string{"REIN_TEST_MCP_PRESENT", "REIN_TEST_MCP_ABSENT_40"}}
	missing := s.MissingEnv()
	// Set to the empty string is still set: the value is the vendor's to judge.
	if len(missing) != 1 || missing[0] != "REIN_TEST_MCP_ABSENT_40" {
		t.Errorf("MissingEnv = %v", missing)
	}
	if s.Capability() != "mcp:x" {
		t.Errorf("Capability = %q", s.Capability())
	}
}

func TestMCPServerSpecMissingFromChecksTheGivenSet(t *testing.T) {
	t.Setenv("REIN_TEST_IN_PROCESS_ONLY", "x")
	s := MCPServerSpec{Name: "docs", Command: "docs-mcp", EnvVars: []string{"IN_MAP", "REIN_TEST_IN_PROCESS_ONLY"}}
	inMap := func(n string) bool { return n == "IN_MAP" }
	got := s.MissingFrom(inMap)
	if len(got) != 1 || got[0] != "REIN_TEST_IN_PROCESS_ONLY" {
		t.Errorf("MissingFrom = %v; a variable in this process but not the set is missing", got)
	}
}

func TestRunSpecInheritedEnvIsAlwaysScoped(t *testing.T) {
	t.Setenv("REIN_TEST_DAEMON_SECRET", "x")
	t.Setenv("REIN_TEST_DECLARED", "y")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-not-real")
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real")
	has := func(env []string, prefix string) bool {
		for _, kv := range env {
			if strings.HasPrefix(kv, prefix) {
				return true
			}
		}
		return false
	}
	plain := RunSpec{}.InheritedEnv()
	if has(plain, "REIN_TEST_DAEMON_SECRET=") {
		t.Error("a spec with no PassEnv inherited a daemon variable outside the system allow-list")
	}
	if !has(plain, "PATH=") {
		t.Error("a spec lost PATH")
	}
	passed := RunSpec{PassEnv: []string{"REIN_TEST_DECLARED", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"}}.InheritedEnv()
	if !has(passed, "REIN_TEST_DECLARED=") {
		t.Error("a variable named in PassEnv was not inherited")
	}
	if has(passed, "REIN_TEST_DAEMON_SECRET=") {
		t.Error("PassEnv let through a variable it does not name")
	}
	for _, m := range []string{"ANTHROPIC_API_KEY=", "OPENAI_API_KEY="} {
		if has(passed, m) || has(plain, m) {
			t.Errorf("%s reached a run; a metered key is never inherited, even when PassEnv names it", m)
		}
	}
}

func TestCheckPlanEnvRefusesEveryMeteredKey(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY", "XAI_API_KEY"} {
		err := RunSpec{Env: map[string]string{name: "value-not-real", "POSTHOG_API_KEY": "fine"}}.CheckPlanEnv()
		if !errors.Is(err, ErrNotPlanAuth) {
			t.Errorf("Env with %s: err = %v; want ErrNotPlanAuth", name, err)
			continue
		}
		if !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "value-not-real") {
			t.Errorf("error should name %s and never its value: %v", name, err)
		}
	}
	if err := (RunSpec{Env: map[string]string{"POSTHOG_API_KEY": "fine"}}).CheckPlanEnv(); err != nil {
		t.Errorf("an ordinary secret was refused: %v", err)
	}
}
