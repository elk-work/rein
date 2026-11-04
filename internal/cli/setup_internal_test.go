package cli

import (
	"bytes"
	"strings"

	"errors"
	"github.com/spf13/cobra"
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

func TestConfirmJoinAsksAPersonAndCancelsOnNo(t *testing.T) {
	old := setupInteractive
	defer func() { setupInteractive = old }()
	setupInteractive = func(*cobra.Command) bool { return true }
	for in, want := range map[string]bool{"y\n": true, "yes\n": true, "n\n": false, "\n": false} {
		cmd := newSetupCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetIn(strings.NewReader(in))
		if got := confirmJoin(cmd, pairPoll{Workspace: "Acme", ApprovedBy: "Ana"}); got != want {
			t.Errorf("answer %q: joined = %v, want %v", in, got, want)
		}
		if !strings.Contains(out.String(), "Ana, in Acme, allowed this computer") || !strings.Contains(out.String(), "Join now?") {
			t.Errorf("prompt: %q", out.String())
		}
	}
	cmd := newSetupCommand()
	_ = cmd.Flags().Set("yes", "true")
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader("n\n"))
	if !confirmJoin(cmd, pairPoll{}) {
		t.Error("--yes still asked")
	}
}

func TestWorkerDisplayNameFitsElksCap(t *testing.T) {
	for in, want := range map[string]string{
		"MacBook Air":         "MacBook Air",
		"DESKTOP-4F7K2LQ":     "DESKTOP-4F7K",
		"Caseys-MacBook-Pro":  "Caseys-MacBo",
		"abcdefghijk-xyz":     "abcdefghijk",
		"  ":                  "Computer",
		"Café-Ordinateur-Été": "Café-Ordinat",
	} {
		if got := workerDisplayName(in); got != want {
			t.Errorf("workerDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}
