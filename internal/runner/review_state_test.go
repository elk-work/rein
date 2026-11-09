package runner

import (
	"os"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/secretenv"
)

func TestReviewCheckpointExcludesRuntimeCredentials(t *testing.T) {
	dir := t.TempDir()
	qr := &queueRunner{r: &Runner{opts: Options{StateDir: dir}}}
	qr.redact.Store(secretenv.NewRedactor(map[string]string{"TEST": "private-test-value"}))
	c := reviewCont{round: 2, wo: &elk.WorkOrder{RunID: "run-1"}, sessionID: "session-1", sentBack: "private-test-value",
		spec: adapter.RunSpec{Env: map[string]string{"KEY": "environment-value"}, ProviderKey: "provider-value",
			MCPServers: map[string]any{"server": "connector-value"}, Settings: map[string]string{"key": "setting-value"}}}
	if err := qr.saveReview(c); err != nil {
		t.Fatal(err)
	}
	path := reviewFile(dir, "run-1")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"private-test-value", "environment-value", "provider-value", "connector-value", "setting-value"} {
		if strings.Contains(string(b), s) {
			t.Fatal("checkpoint contains runtime credentials")
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows does not expose Unix permission bits.
	if info.Mode().Perm()&0077 != 0 && os.PathSeparator != '\\' {
		t.Fatalf("checkpoint mode: %v", info.Mode())
	}
}
