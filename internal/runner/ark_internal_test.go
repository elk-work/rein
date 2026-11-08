package runner

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/worktree"
)

func TestArkCleanupAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	qr := &queueRunner{r: &Runner{opts: Options{
		Out: io.Discard,
		ArkCommand: func(ctx context.Context, dir string, args ...string) (string, error) {
			if err := ctx.Err(); err != nil {
				t.Fatal(err)
			}
			calls++
			if args[1] == "list" {
				return `[{"id":"one","branch_name":"run","status":"running"},{"id":"two","branch_name":"run","status":"running"}]`, nil
			}
			if args[4] != "failed" || !strings.Contains(args[6], "without a submitted deliverable") {
				t.Errorf("finish args: %v", args)
			}
			return "", errors.New("Ark unavailable")
		},
	}}, logStatus: runlog.StatusInterrupted}
	// A failed finish must not prevent an attempt to finish the next record.
	qr.finishArkRuns(ctx, &worktree.Worktree{Branch: "run", Ark: worktree.ArkState{OK: true}})
	if calls != 3 {
		t.Errorf("commands = %d", calls)
	}
}
