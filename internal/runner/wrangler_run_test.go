package runner_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// ark:rein#50: the Wrangler rule and the owner's connector are granted per
// run, to a Wrangler cycle — a packet that requires `pm` on a queue that opted
// in — and to no other run, even on the same queue. mac-claude is Elk Scout's
// Wrangler and an ordinary Claude build queue at once; its build runs must
// never be told they may migrate and deploy, nor be handed the owner's
// connector.

const preflightHeading = "### Declared environment capabilities — preflight list\n"

// preflightList is the environment list a run's system prompt declares.
func preflightList(t *testing.T, prompt string) []string {
	t.Helper()
	_, after, ok := strings.Cut(prompt, preflightHeading)
	if !ok {
		t.Fatalf("the system prompt has no preflight list:\n%s", prompt)
	}
	line, _, _ := strings.Cut(after, "\n")
	return strings.Split(line, ", ")
}

func wranglerHarness(t *testing.T, spelling string) *harness {
	h := newHarness(t)
	if spelling == "pm" {
		h.cfg.Queues[0].PM = true
		h.cfg.Queues[0].PMConnectorAccount = "workspace/owner"
	} else {
		h.cfg.Queues[0].Wrangler = true
		h.cfg.Queues[0].WranglerConnectorAccount = "workspace/owner"
	}
	return h
}

func TestABuildRunOnAWranglerQueueGetsNeitherTheRuleNorTheConnector(t *testing.T) {
	for _, spelling := range []string{"wrangler", "pm"} {
		t.Run(spelling, func(t *testing.T) {
			h := wranglerHarness(t, spelling)
			h.elk.Text("claim_run", order("run-1", "git"))
			if err := h.run(runner.Options{}); err != nil {
				t.Fatalf("%v\nlog:\n%s", err, h.log)
			}
			specs := h.agent.Specs()
			if len(specs) != 1 {
				t.Fatalf("the build run did not start\nlog:\n%s", h.log)
			}
			s := specs[0]
			if s.WranglerConnectorAccount != "" {
				t.Errorf("a build run was handed the owner's connector %q", s.WranglerConnectorAccount)
			}
			if s.MCPServers != nil {
				t.Errorf("a build run on a Wrangler queue got MCP servers %#v", s.MCPServers)
			}
			// The prompt a queue without the opt-in gives, byte for byte, up
			// to the per-run blocks.
			if !strings.HasPrefix(s.SystemPrompt, runner.SystemPrompt+"\n\n"+preflightHeading) {
				t.Error("a build run on a Wrangler queue did not get the ordinary standing prompt")
			}
			for _, bad := range []string{"Wrangler cycle", "Wrangler queue is authorized", "docs/wrangler.md", "deploy-functions.sh"} {
				if strings.Contains(s.SystemPrompt, bad) {
					t.Errorf("a build run's prompt says %q", bad)
				}
			}
			if !strings.Contains(s.SystemPrompt, "history, touching production") {
				t.Error("a build run's prompt lost the rule against touching production")
			}
			list := preflightList(t, s.SystemPrompt)
			if has(list, "pm") || has(list, "mcp:elk") {
				t.Errorf("a build run was told it holds the Wrangler's capabilities: %v", list)
			}
			if h.submitted().Arg("status") != "ready" {
				t.Errorf("the build run did not finish normally\nlog:\n%s", h.log)
			}
			// The queue still advertises the Wrangler's routing capability:
			// the opt-in gates which queues may run cycles, not which runs.
			if caps := declared(h); !has(caps, "pm") || !has(caps, "mcp:elk") {
				t.Errorf("the Wrangler queue stopped declaring pm and mcp:elk: %v", caps)
			}
		})
	}
}

func TestOneWranglerQueueGrantsTheRulePerRun(t *testing.T) {
	// One queue runner, three claims in a row — build, cycle, build — so the
	// grant is shown to follow the packet and never to carry from one run to
	// the next.
	h := wranglerHarness(t, "wrangler")
	orders := []string{order("run-1", "git"), order("run-2", "pm"), order("run-3")}
	var mu sync.Mutex
	h.elk.Handle("claim_run", func(map[string]any) elktest.Reply {
		mu.Lock()
		defer mu.Unlock()
		if len(orders) == 0 {
			return elktest.Reply{Text: `Queue "` + queue + `" is clear: no runs waiting in "` + space + `".`}
		}
		next := orders[0]
		orders = orders[1:]
		return elktest.Reply{Text: next}
	})
	deadline := time.Now().Add(10 * time.Second)
	for len(h.agent.Specs()) < 3 && time.Now().Before(deadline) {
		_ = h.runLoop(runner.Options{PollInterval: 10 * time.Millisecond, TelemetryInterval: time.Hour}, 1500*time.Millisecond)
	}
	specs := h.agent.Specs()
	if len(specs) != 3 {
		t.Fatalf("started %d runs, want 3\nlog:\n%s", len(specs), h.log)
	}
	for _, i := range []int{0, 2} {
		s := specs[i]
		if s.WranglerConnectorAccount != "" || strings.Contains(s.SystemPrompt, "docs/wrangler.md") {
			t.Errorf("build run %s got the Wrangler grant (connector %q)", s.RunID, s.WranglerConnectorAccount)
		}
		if list := preflightList(t, s.SystemPrompt); has(list, "pm") || has(list, "mcp:elk") {
			t.Errorf("build run %s was told it holds %v", s.RunID, list)
		}
	}
	c := specs[1]
	if c.RunID != "run-2" {
		t.Fatalf("second run = %s, want the cycle run-2", c.RunID)
	}
	if c.WranglerConnectorAccount != "workspace/owner" || c.MCPServers != nil {
		t.Errorf("the cycle did not get exactly the owner's connector: account %q, servers %#v",
			c.WranglerConnectorAccount, c.MCPServers)
	}
	if !strings.Contains(c.SystemPrompt, "This run is a Wrangler cycle") || !strings.Contains(c.SystemPrompt, "docs/wrangler.md") {
		t.Error("the cycle did not get the Wrangler rule")
	}
	if list := preflightList(t, c.SystemPrompt); !has(list, "pm") || !has(list, "mcp:elk") {
		t.Errorf("the cycle's preflight list lost the Wrangler's capabilities: %v", list)
	}
}

func TestAPMPacketOnAQueueWithoutTheOptInIsStillRefused(t *testing.T) {
	h := newHarness(t)
	h.elk.Text("claim_run", order("run-1", "pm"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	sub := h.submitted()
	if sub.Arg("status") != "stuck" {
		t.Errorf("status = %q, want stuck", sub.Arg("status"))
	}
	if !strings.Contains(sub.Arg("deliverable"), "Missing from this machine's environment: **pm") {
		t.Errorf("the refusal does not name pm:\n%s", sub.Arg("deliverable"))
	}
	if len(h.agent.Specs()) != 0 {
		t.Error("a Wrangler cycle started on a queue that never opted in")
	}
	if created, _ := h.wts.counts(); created != 0 {
		t.Error("a worktree was created for a run that could never be served")
	}
}

func TestABuildRunNeedingTheElkConnectorOnAWranglerQueueIsRefused(t *testing.T) {
	// On a Wrangler queue `mcp:elk` is the owner's connector. A run that is
	// not a cycle does not get it, so it must not be told it has it either:
	// refused, as on a queue without the opt-in, rather than started without
	// the server it asked for.
	h := wranglerHarness(t, "wrangler")
	h.elk.Text("claim_run", order("run-1", "mcp:elk"))
	if err := h.run(runner.Options{}); err != nil {
		t.Fatalf("%v\nlog:\n%s", err, h.log)
	}
	sub := h.submitted()
	if sub.Arg("status") != "stuck" {
		t.Errorf("status = %q, want stuck", sub.Arg("status"))
	}
	d := sub.Arg("deliverable")
	for _, want := range []string{"Missing from this machine's environment: **mcp:elk", "only a Wrangler cycle", "requires `pm`"} {
		if !strings.Contains(d, want) {
			t.Errorf("the refusal is missing %q:\n%s", want, d)
		}
	}
	if len(h.agent.Specs()) != 0 {
		t.Error("a build run started holding the owner's connector capability")
	}
}
