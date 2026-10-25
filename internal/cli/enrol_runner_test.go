package cli_test

import (
	"strings"
	"testing"

	_ "github.com/elk-work/rein/internal/adapter/check"
	"github.com/elk-work/rein/internal/config"
)

// `rein enrol --agent-kind runner` enrols a check runner (internal/adapter/
// check): Elk is told the kind, and the declaration carries agent:runner.
// (tool:docker and tool:act come from the host probes; internal/runner tests
// those, since this package's harness declares the host list itself.)
func TestEnrolARunner(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	s := enrolServer(t)
	s.Text("connect_executor", `Created named queue "lab-checks" in workspace "Elk Scout" (executor `+
		"`exec-2`). Pull its work with `claim_run(queue: \"lab-checks\")`.")

	out, errb, code := run(t, "enrol",
		"--workspace", "Elk Scout", "--agent-kind", "runner", "--host-id", "lab", "--queue", "lab-checks",
		"--token", enrolToken, "--mcp-url", s.URL)
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	args := s.CallsTo("connect_executor")[0].Args
	if args["agent_kind"] != "runner" {
		t.Errorf("agent_kind = %v", args["agent_kind"])
	}
	var caps []string
	for _, c := range args["declared_capabilities"].([]any) {
		caps = append(caps, c.(string))
	}
	joined := " " + strings.Join(caps, " ") + " "
	for _, want := range []string{"agent:runner", "runtime:structured_events", "tool:git"} {
		if !strings.Contains(joined, " "+want+" ") {
			t.Errorf("declared capabilities lack %s: %v", want, caps)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if q, ok := cfg.Queue("lab-checks"); !ok || q.AgentKind != "runner" {
		t.Errorf("queue in config: %+v (found %v)", q, ok)
	}
}
