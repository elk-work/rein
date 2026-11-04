package cli

import (
	"errors"
	"testing"
)

func TestPairURLSitsNextToElkMCP(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.elk.work/functions/v1/elk-mcp":  "https://api.elk.work/functions/v1/worker-pair",
		"https://api.elk.work/functions/v1/elk-mcp/": "https://api.elk.work/functions/v1/worker-pair",
		"http://127.0.0.1:9999":                      "http://127.0.0.1:9999/worker-pair",
	} {
		if got := pairURLFor(in); got != want {
			t.Errorf("pairURLFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectAgentKindPrefersClaude(t *testing.T) {
	has := func(names ...string) func(string) (string, error) {
		return func(n string) (string, error) {
			for _, x := range names {
				if x == n {
					return "/bin/" + n, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	if got := detectAgentKind(has("codex", "claude")); got != "claude" {
		t.Errorf("got %q", got)
	}
	if got := detectAgentKind(has("grok")); got != "grok" {
		t.Errorf("got %q", got)
	}
	if got := detectAgentKind(has()); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestPairSecretIsLongAndURLSafe(t *testing.T) {
	a, _ := newPairSecret()
	b, _ := newPairSecret()
	if len(a) != 43 || a == b {
		t.Fatalf("secrets %q %q", a, b)
	}
	if got := pairPlatform("darwin"); got != "macos" {
		t.Errorf("darwin → %q", got)
	}
}
