package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
)

// `parallel` lives in a queue's own block, beside its repos list, and says how
// many runs that queue may drive at once. Absent is one.
func TestParallelIsReadPerQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	text := `workspace = "Elk Scout"
[repos]
rein = "/dev/rein"
[[queues]]
name = "mac-claude"
agent_kind = "claude"
repos = ["rein"]
parallel = 3
[[queues]]
name = "mac-codex"
agent_kind = "codex"
repos = ["rein"]
`
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Queues[0].ParallelOrDefault(); got != 3 {
		t.Errorf("mac-claude parallel = %d, want 3", got)
	}
	if got := cfg.Queues[1].ParallelOrDefault(); got != 1 {
		t.Errorf("mac-codex, which sets none, parallel = %d, want 1", got)
	}
}

func TestParallelMustBeANumberOfRuns(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, ""}, {1, ""}, {4, ""}, {config.MaxParallel, ""},
		{-1, "parallel = -1 is not a number of runs"},
		{config.MaxParallel + 1, "is not a number of runs"},
	} {
		cfg := config.Config{Queues: []config.Queue{{Name: "mac-claude", AgentKind: "claude", Parallel: tc.n}}}
		err := cfg.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("parallel = %d: %v", tc.n, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("parallel = %d: error %v, want %q", tc.n, err, tc.want)
		}
	}
}
