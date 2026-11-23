package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

type claimAckAdapter struct {
	adapter.Adapter
	check func()
}

func (a claimAckAdapter) Preflight(ctx context.Context) error {
	a.check()
	return a.Adapter.Preflight(ctx)
}

// Exercise the real Elk HTTP contract via elktest, including a refusal of
// just the acknowledgement. Neither setup nor the agent may precede it.
func TestClaimAcknowledgedBeforeSetup(t *testing.T) {
	for _, refused := range []bool{false, true} {
		name := "recorded"
		if refused {
			name = "not_running"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.elk.Text("claim_run", order("run-1"))
			acknowledgements := 0
			h.elk.Handle("report_progress", func(args map[string]any) elktest.Reply {
				body, _ := args["body"].(string)
				if strings.Contains(body, "Rein received the work order") {
					acknowledgements++
					if n, _ := h.wts.counts(); n != 0 {
						t.Error("worktree created before acknowledgement")
					}
					if len(h.agent.Specs()) != 0 {
						t.Error("agent started before acknowledgement")
					}
					if refused {
						return elktest.Reply{Text: "Not recorded: This run is `done`, not running."}
					}
				}
				return elktest.Reply{Text: "Progress recorded on `run-1`. Lease refreshed."}
			})
			adapter.Unregister(kind)
			checked := false
			if err := adapter.Register(claimAckAdapter{Adapter: h.agent, check: func() {
				// Queue readiness also probes the adapter before claiming.
				if len(h.elk.CallsTo("claim_run")) == 0 {
					return
				}
				checked = true
				if acknowledgements != 1 {
					t.Errorf("acknowledgements before preflight = %d, want 1", acknowledgements)
				}
			}}); err != nil {
				t.Fatal(err)
			}
			if err := h.run(runner.Options{}); err != nil {
				t.Fatalf("%v\n%s", err, h.log)
			}
			if !checked || acknowledgements != 1 {
				t.Errorf("preflight = %v, acknowledgements = %d", checked, acknowledgements)
			}
			if len(h.agent.Specs()) != 1 || h.submitted().Arg("status") != "ready" {
				t.Fatalf("run did not finish normally:\n%s", h.log)
			}
			if refused && !strings.Contains(h.log.String(), "claim acknowledgement:") {
				t.Errorf("refusal was not logged:\n%s", h.log)
			}
		})
	}
}
