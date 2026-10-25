//go:build !windows

package check

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

// TestRealActRunsATinyWorkflow runs the real act, not the stub, on the host
// executor — no Docker needed. It is opt-in, because it needs act installed
// and reaches github.com for `uses:` actions:
//
//	REIN_REAL_ACT=1 go test ./internal/adapter/check -run RealAct -v
//
// With REIN_REAL_ACT_REPO naming a checkout of elk-work/scout, it also runs
// scout's quick-checks job at that checkout's HEAD, named the way the
// milestone's example packet names it (job "quick-checks", which resolves to
// the workflow's one job, `checks`). REIN_REAL_ACT_RECORDS keeps the records.
func TestRealActRunsATinyWorkflow(t *testing.T) {
	if os.Getenv("REIN_REAL_ACT") == "" {
		t.Skip("set REIN_REAL_ACT=1 to run the real act")
	}
	if _, err := exec.LookPath("act"); err != nil {
		t.Skip("act is not on PATH")
	}
	records := os.Getenv("REIN_REAL_ACT_RECORDS")
	if records == "" {
		records = t.TempDir()
	}
	a := &Adapter{RecordRoot: records, Pulse: "rein-test-no-pulse"}

	dir, sha := testRepo(t)
	spec := baseSpec(dir, sha)
	spec.RunID = "real-tiny"
	r := runCheck(t, a, spec, "")
	if r.res.Status != adapter.StatusSucceeded || r.rec.Result != ResultPass {
		t.Fatalf("tiny workflow: %s / %s\n%s", r.res.Status, r.rec.Result, r.res.Summary)
	}
	t.Logf("tiny workflow:\n%s", r.res.Summary)

	scout := os.Getenv("REIN_REAL_ACT_REPO")
	if scout == "" {
		return
	}
	head := strings.TrimSpace(gitRun(t, scout, "rev-parse", "HEAD"))
	spec = adapter.RunSpec{
		RunID: "real-scout-quick-checks", AgentKind: Kind, WorktreeDir: scout,
		Prompt: order("```json\n{\"repo\":\"elk-work/scout\",\"sha\":\"" + head + "\",\"ref\":\"refs/heads/main\"," +
			"\"suite\":\"migration-numbers\",\"workflow\":\".github/workflows/quick-checks.yml\",\"job\":\"quick-checks\"," +
			"\"runs_on\":[\"ubuntu-latest\"],\"provider\":\"rein\",\"changed_paths\":[]}\n```"),
		PermissionMode: adapter.PermissionFull, Queue: "mac-runner",
		Settings: map[string]string{"executor": "host"},
	}
	r = runCheck(t, a, spec, "")
	t.Logf("scout quick-checks (%s):\n%s", r.res.Status, r.res.Summary)
	if r.rec.Result != ResultPass && r.rec.Result != ResultFail {
		t.Fatalf("scout quick-checks did not run: %s", r.rec.Result)
	}
}
