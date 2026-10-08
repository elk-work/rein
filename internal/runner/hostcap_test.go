package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runner"
)

func TestSplitRequirementsRoutesEachNameToTheListThatCanAnswerIt(t *testing.T) {
	// The bug this exists to prevent: every name went to the manifest, whose
	// fail-closed rule correctly refuses a name it has never heard of — so
	// every packet carrying an environment name went stuck at preflight.
	rt, host := runner.SplitRequirements([]string{
		"git", "elk-connector", "shell", "github-cli", "", "  xcode  ",
	})
	if got := strings.Join(rt, ","); got != "git,shell" {
		t.Errorf("runtime names = %q, want the adapter vocabulary only", got)
	}
	if got := strings.Join(host, ","); got != "elk-connector,github-cli,xcode" {
		t.Errorf("host names = %q, want everything else", got)
	}
}

func TestHostCapabilitiesFailClosed(t *testing.T) {
	h := runner.NewHostCapabilities("a test", "elk-connector", "github-cli")
	if got := h.Missing([]string{"elk-connector", "github-cli"}); len(got) != 0 {
		t.Errorf("missing = %v, want none", got)
	}
	// A name nothing declared is missing, whether the machine genuinely lacks
	// it or the packet has a typo. Same rule as the manifest matcher, asked of
	// a different list.
	got := h.Missing([]string{"github-cli", "xcode", "xcode", "definitely-not-a-thing"})
	if strings.Join(got, ",") != "xcode,definitely-not-a-thing" {
		t.Errorf("missing = %v — want the order they were required, deduplicated", got)
	}
}

func TestDetectHostCapabilitiesBuiltInsAndConfig(t *testing.T) {
	t.Setenv(runner.EnvCapabilities, "github-cli, docker")
	cfg := config.Config{Capabilities: []string{"SUPABASE_SERVICE_ROLE_KEY"}}
	h := runner.DetectHostCapabilities(context.Background(), cfg, "xcode")

	for _, want := range []string{
		// Rein IS the connector: this is the name every real packet carried,
		// and the one that made the dogfood fail.
		"elk-connector",
		"git", "worktree", // cut by internal/worktree before the agent starts
		"github-cli", "docker", // the env override
		"SUPABASE_SERVICE_ROLE_KEY", // config.toml
		"xcode",                     // this queue's addition
	} {
		if !h.Has(want) {
			t.Errorf("this machine does not claim %q; it has %v", want, h.Names())
		}
	}
	// Provenance is what makes a stuck deliverable useful.
	var described string
	for _, d := range h.Describe() {
		if strings.HasPrefix(d, "elk-connector ") {
			described = d
		}
	}
	if described != "elk-connector (built in)" {
		t.Errorf("elk-connector described as %q", described)
	}
}

func TestSetButEmptyCapabilitiesDeclaresNothingRatherThanProbing(t *testing.T) {
	// `REIN_HOST_CAPABILITIES=` is how .github/workflows/ci.yml says "this box
	// holds nothing beyond the built-ins". Reading it with os.Getenv and
	// testing for "" would turn that declaration back into a full probe, which
	// on a hosted runner shells out to `gh auth status` and answers a question
	// nobody asked.
	t.Setenv(runner.EnvCapabilities, "")
	h := runner.DetectHostCapabilities(context.Background(), config.Config{})

	want := []string{"elk-connector", "git", "worktree"}
	if got := strings.Join(h.Names(), ","); got != strings.Join(want, ",") {
		t.Fatalf("capabilities = %q, want exactly the built-ins %q", got, strings.Join(want, ","))
	}
	// Specifically: no probe ran, so nothing on this machine's PATH leaked in.
	// `go` is the sharpest of these — it is on PATH by definition wherever
	// this test runs.
	for _, probed := range []string{"go", "github-cli", "node", "claude"} {
		if h.Has(probed) {
			t.Errorf("%q was probed for despite an explicit empty declaration", probed)
		}
	}
}

func TestDeclaredCapabilitiesIsTheUnionOfBothNamespaces(t *testing.T) {
	m := adapter.Manifest{
		Kind: "claude",
		Capabilities: map[adapter.Capability]adapter.Support{
			adapter.CapGit:       adapter.SupportYes,
			adapter.CapShell:     adapter.SupportYes,
			adapter.CapApprovals: adapter.SupportNo,
		},
		Platforms: []adapter.Platform{adapter.AnyArch("darwin")},
	}
	host := runner.NewHostCapabilities("a test", "elk-connector", "github-cli", "git")

	got := runner.DeclaredCapabilities(m, host)
	if strings.Join(got, ",") != "agent:claude,elk-connector,git,github-cli,os:"+testOS()+",runtime:git,runtime:shell,shell,tool:git,tool:github-cli" {
		t.Errorf("declared = %v — want both namespaces, sorted and deduplicated", got)
	}
	// Only what the adapter declares `yes`; `no` is a decision, not a claim.
	for _, n := range got {
		if n == "approvals" {
			t.Error("a capability declared `no` was reported to Elk as held")
		}
	}
}

func TestDeclaredCapabilitiesRespectsElksLimits(t *testing.T) {
	// Elk refuses the whole list if it is over 64 names or any name is over 64
	// characters, so a truncated declaration beats no declaration at all.
	names := make([]string, 0, 80)
	for i := 0; i < 80; i++ {
		names = append(names, "cap-"+strings.Repeat("x", 2)+"-"+string(rune('a'+i%26))+strings.Repeat("y", i%3))
	}
	names = append(names, strings.Repeat("z", runner.MaxCapabilityChars+1))
	host := runner.NewHostCapabilities("a test", names...)

	got := runner.DeclaredCapabilities(adapter.Manifest{Kind: "claude"}, host)
	if len(got) > runner.MaxDeclaredCapabilities {
		t.Errorf("declared %d names, over Elk's cap of %d", len(got), runner.MaxDeclaredCapabilities)
	}
	for _, n := range got {
		if len(n) > runner.MaxCapabilityChars {
			t.Errorf("declared %q, over Elk's %d-character cap", n, runner.MaxCapabilityChars)
		}
	}
}

func testOS() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

func TestArkOnPathDeclaresArkCLIAndAcceptsAlias(t *testing.T) {
	t.Setenv(runner.EnvCapabilities, "")
	if err := os.Unsetenv(runner.EnvCapabilities); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binary := "ark"
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, binary), []byte("placeholder"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	h := runner.DetectHostCapabilities(context.Background(), config.Config{})
	if !h.Has("ark-cli") || !h.Has("ark") {
		t.Fatal(h.Names())
	}
	_, names := runner.SplitRequirements([]string{"ark", "tool:ark"})
	if strings.Join(names, ",") != "ark-cli,tool:ark-cli" {
		t.Fatal(names)
	}
	if missing := runner.MissingQueueRequirements(names, adapter.Manifest{}, h, nil); len(missing) != 0 {
		t.Fatal(missing)
	}
}

func TestWranglerRequirementIsNotAdvisory(t *testing.T) {
	// The Wrangler's capability is still `pm` on the wire.
	host := runner.NewHostCapabilities("test")
	if got := runner.AdvisoryRequirements([]string{"pm"}, host); len(got) != 0 {
		t.Fatal("Wrangler requirement ignored on ordinary queues")
	}
}
