package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/runner"
)

// TestConfigCheck is the command a running service asks a newly installed
// binary before restarting into it (ark:rein#67): the report on stdout, and
// exit 1 when a queue would strand its runs.
func TestConfigCheck(t *testing.T) {
	const old = `workspace = "Elk Scout"
default_repo = "/dev/elk"
[repos]
"elk-work/scout" = "/dev/scout"
[[queues]]
name = "mac-codex"
agent_kind = "codex"
[[queues]]
name = "mac-claude"
agent_kind = "claude"
workspace = "Signal"
`
	half := strings.Replace(old, `agent_kind = "codex"`, "agent_kind = \"codex\"\nrepos = [\"elk-work/scout\"]", 1)
	for _, tc := range []struct {
		name, text string
		code       int
		mode       string
	}{
		{"compatibility", old, 0, "compatibility"},
		{"half scoped", half, 1, "scoped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
				t.Fatal(err)
			}
			out, _, code := run(t, "config", "check", "--json", "--config", path)
			if code != tc.code {
				t.Fatalf("exit = %d, want %d\n%s", code, tc.code, out)
			}
			var rep runner.ConfigReport
			if err := json.Unmarshal([]byte(out), &rep); err != nil {
				t.Fatalf("not a report: %v\n%s", err, out)
			}
			if rep.Schema != runner.ConfigCheckSchema || rep.RepositoryMode != tc.mode || rep.Config != path || len(rep.Queues) != 2 {
				t.Errorf("report = %+v", rep)
			}
			human, _, _ := run(t, "config", "check", "--config", path)
			if tc.code == 0 && !strings.Contains(human, "WARNING: repository compatibility mode") {
				t.Errorf("the human report does not warn:\n%s", human)
			}
			if tc.code == 1 && !strings.Contains(human, "PROBLEM Signal/mac-claude") {
				t.Errorf("the human report does not name the stranded queue:\n%s", human)
			}
		})
	}
	out, _, code := run(t, "config", "check", "--json", "--config", filepath.Join(t.TempDir(), "missing.toml"))
	if code != 1 || !strings.Contains(out, `"error"`) {
		t.Errorf("a missing config = exit %d\n%s", code, out)
	}
}
