package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
)

// incidentConfig is the shape ark:rein#67 found on Issac's Mac: several
// workspaces, a top-level [repos] map, default_repo, and no repos lists.
const incidentConfig = `workspace = "Elk Scout"
default_repo = "/dev/elk"
[elk]
mcp_url = "https://elk.invalid/mcp"
[repos]
"elk-work/scout" = "/dev/scout"
signal = "/dev/signal"
[[queues]]
name = "mac-codex"
agent_kind = "codex"
[[queues]]
name = "mac-claude"
agent_kind = "claude"
workspace = "Signal"
`

// v084Reading is how v0.8.4 read incidentConfig: no repositories anywhere and
// no default — every run stuck.
func v084Reading(version string) ConfigReport {
	return ConfigReport{Schema: 1, Version: version, OK: false, RepositoryMode: "scoped", Queues: []QueueReport{
		{Queue: "Elk Scout/mac-codex", Workspace: "Elk Scout", Name: "mac-codex", Repositories: []string{}},
		{Queue: "Signal/mac-claude", Workspace: "Signal", Name: "mac-claude", Repositories: []string{}},
	}}
}

// guardRunner is a service-mode runner on v1 whose installed binary has just
// become v2, reading the incident config from a real file.
func guardRunner(t *testing.T, check func(context.Context, string, string) (ConfigReport, error)) (*Runner, string, *strings.Builder) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(incidentConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "rein")
	if err := os.WriteFile(bin, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := &strings.Builder{}
	r := &Runner{opts: Options{
		Version: "v1", UnderService: true, Out: log, Config: cfg, ConfigPath: cfgPath,
		UpgradeStat:    os.Stat,
		UpgradeVersion: func(context.Context, string) (string, error) { return "v2", nil },
		UpgradeCheck:   check,
	}, upgradeDone: make(chan struct{})}
	return r, bin, log
}

// install replaces the binary and runs one upgrade check against it.
func install(t *testing.T, r *Runner, bin string) {
	t.Helper()
	baseline, _ := os.Stat(bin)
	if err := os.WriteFile(bin, []byte("v2, a different size"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.checkUpgrade(context.Background(), bin, &baseline)
}

func TestUpgradeGuardRefusesARestartThatStrandsQueues(t *testing.T) {
	var asked []string
	r, bin, log := guardRunner(t, func(_ context.Context, binary, cfg string) (ConfigReport, error) {
		asked = append(asked, binary, cfg)
		return v084Reading("v2"), nil
	})
	install(t, r, bin)

	if r.isDraining() {
		t.Fatal("drained into a version that strands every queue")
	}
	if len(asked) != 2 || asked[0] != bin || asked[1] != r.opts.ConfigPath {
		t.Errorf("checked %v, want the installed binary against the live config", asked)
	}
	for _, want := range []string{"NOT restarting into v2", "staying on v1", "Elk Scout/mac-codex would have no repositories",
		"would have no default repository", "Signal/mac-claude"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log does not say %q:\n%s", want, log)
		}
	}

	// The beat says so, on a queue with nothing more pressing to report.
	s := &elk.SessionReading{State: string(StateIdle)}
	r.putRefusal(s)
	if !strings.Contains(s.WaitingOn, "Rein v2 is installed but this machine stays on v1") || s.WaitingSince == "" {
		t.Errorf("waiting_on = %q since %q", s.WaitingOn, s.WaitingSince)
	}
	if strings.Contains(s.WaitingOn, "/dev/") {
		t.Errorf("waiting_on carries a local path: %q", s.WaitingOn)
	}
	busy := &elk.SessionReading{State: string(StateIdle), WaitingOn: "no free slot"}
	if r.putRefusal(busy); busy.WaitingOn != "no free slot" {
		t.Errorf("the refusal overwrote the queue's own reason: %q", busy.WaitingOn)
	}

	// Asked again with nothing changed: no second log line, no drain.
	before := log.Len()
	baseline, _ := os.Stat(bin)
	r.checkUpgrade(context.Background(), bin, &baseline)
	if r.isDraining() || log.Len() != before {
		t.Errorf("an unchanged refusal drained or logged again:\n%s", log.String()[before:])
	}
}

func TestUpgradeGuardProceedsWhenNothingIsStranded(t *testing.T) {
	r, bin, _ := guardRunner(t, func(_ context.Context, _, path string) (ConfigReport, error) {
		return CheckConfigFile(path, "v2"), nil
	})
	install(t, r, bin)
	if !r.isDraining() {
		t.Fatal("a passing check did not drain")
	}
}

func TestUpgradeGuardFailsOpen(t *testing.T) {
	for name, check := range map[string]func(context.Context, string, string) (ConfigReport, error){
		"no config check": func(context.Context, string, string) (ConfigReport, error) {
			return ConfigReport{}, errors.New(`unknown command "config"`)
		},
		"newer schema": func(context.Context, string, string) (ConfigReport, error) {
			rep := v084Reading("v2")
			rep.Schema = ConfigCheckSchema + 1
			return rep, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, bin, log := guardRunner(t, check)
			install(t, r, bin)
			if !r.isDraining() {
				t.Fatalf("an unanswerable check held the upgrade:\n%s", log)
			}
			if !strings.Contains(log.String(), "unchecked") {
				t.Errorf("the log does not say the restart is unchecked:\n%s", log)
			}
		})
	}
}

func TestUpgradeGuardRetriesOnceTheConfigIsFixed(t *testing.T) {
	fixed := false
	r, bin, log := guardRunner(t, func(_ context.Context, _, path string) (ConfigReport, error) {
		if fixed {
			return CheckConfigFile(path, "v2"), nil
		}
		return v084Reading("v2"), nil
	})
	install(t, r, bin)
	if r.isDraining() {
		t.Fatal("drained before the fix")
	}
	// The person adds the lists; the next tick sees the file change.
	fixed = true
	text := strings.Replace(incidentConfig, `agent_kind = "codex"`, "agent_kind = \"codex\"\nrepos = [\"elk-work/scout\"]\nrepo = \"elk-work/scout\"", 1)
	text = strings.Replace(text, `workspace = "Signal"`, "workspace = \"Signal\"\nrepos = [\"signal\"]\nrepo = \"signal\"", 1)
	if err := os.WriteFile(r.opts.ConfigPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	os.Chtimes(r.opts.ConfigPath, later, later)
	baseline, _ := os.Stat(bin)
	r.checkUpgrade(context.Background(), bin, &baseline)
	if !r.isDraining() {
		t.Fatalf("the fixed config did not release the upgrade:\n%s", log)
	}
	if s := (&elk.SessionReading{}); func() bool { r.putRefusal(s); return s.WaitingOn != "" }() {
		t.Errorf("the refusal outlived the fix: %q", s.WaitingOn)
	}
}

func TestStrandedBy(t *testing.T) {
	cfg, err := loadText(t, incidentConfig)
	if err != nil {
		t.Fatal(err)
	}
	cur := CheckConfig(cfg, "v1")
	if got := strandedBy(cur, cur); len(got) != 0 {
		t.Errorf("the same reading strands %v", got)
	}
	if got := strandedBy(cur, ConfigReport{Schema: 1, Error: "config: /home/x/.rein/config.toml: queues[0]: boom"}); len(got) != 1 || strings.Contains(got[0], "/home/x") {
		t.Errorf("an unloadable config = %v, want one sentence without the path", got)
	}
	lost := CheckConfig(cfg, "v2")
	lost.Queues[0].Repositories = []string{"elk-work/scout"}
	lost.Queues = lost.Queues[:1]
	got := strings.Join(strandedBy(cur, lost), "\n")
	for _, want := range []string{"Elk Scout/mac-codex would lose [signal]", "Signal/mac-claude would not be served"} {
		if !strings.Contains(got, want) {
			t.Errorf("stranded = %q, want %q", got, want)
		}
	}
	// A queue already broken on the running build does not hold the upgrade.
	broken := v084Reading("v1")
	if got := strandedBy(broken, v084Reading("v2")); len(got) != 0 {
		t.Errorf("an already-stranded config held the upgrade: %v", got)
	}
}

func TestCheckConfigNamesWhatStrands(t *testing.T) {
	cfg, err := loadText(t, incidentConfig)
	if err != nil {
		t.Fatal(err)
	}
	if rep := CheckConfig(cfg, "v1"); !rep.OK || rep.RepositoryMode != "compatibility" || len(rep.Warnings) != 1 {
		t.Errorf("the incident config under compatibility = %+v", rep)
	}
	// One list, added to one queue: Signal has nothing, and a Wrangler
	// queue in Elk Scout has no default for its cycles.
	text := strings.Replace(incidentConfig, `agent_kind = "codex"`, "agent_kind = \"codex\"\nwrangler = true\nrepos = [\"elk-work/scout\"]", 1)
	if cfg, err = loadText(t, text); err != nil {
		t.Fatal(err)
	}
	rep := CheckConfig(cfg, "v1")
	if rep.OK {
		t.Fatal("a half-scoped config checked OK")
	}
	problems := map[string]string{}
	for _, q := range rep.Queues {
		problems[q.Queue] = strings.Join(q.Problems, "; ")
	}
	if !strings.Contains(problems["Elk Scout/mac-codex"], "Wrangler") || !strings.Contains(problems["Signal/mac-claude"], "no repository") {
		t.Errorf("problems = %v", problems)
	}
	if rep := CheckConfigFile(filepath.Join(t.TempDir(), "missing.toml"), "v1"); rep.OK || rep.Error == "" || rep.Schema != ConfigCheckSchema {
		t.Errorf("a missing file = %+v", rep)
	}
}

func TestExecConfigCheckReadsTheReportWhateverTheExitStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-ins for a binary")
	}
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	failing := script("failing", `echo '{"schema":1,"ok":false,"queues":[{"queue":"q","workspace":"w","name":"q","repositories":[]}]}'; exit 1`)
	rep, err := ExecConfigCheck(context.Background(), failing, "/x/config.toml")
	if err != nil || rep.OK || len(rep.Queues) != 1 {
		t.Errorf("a failing check = %+v, %v", rep, err)
	}
	older := script("older", `echo 'rein: unknown command "config" for "rein"' >&2; exit 1`)
	if _, err := ExecConfigCheck(context.Background(), older, "/x/config.toml"); err == nil {
		t.Error("a binary with no config check produced a report")
	}
	args := script("args", `echo "{\"schema\":1,\"ok\":true,\"config\":\"$*\",\"queues\":[]}"`)
	if rep, err := ExecConfigCheck(context.Background(), args, "/x/config.toml"); err != nil || rep.Config != "config check --json --config /x/config.toml" {
		t.Errorf("args = %q, %v", rep.Config, err)
	}
}

func loadText(t *testing.T, text string) (config.Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.LoadFile(p)
}
