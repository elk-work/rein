package check

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testSHA = "bfb8f963f4a487ebc76425891199e6af3dd283ce"

// order is a claim text in Elk's shape with the packet rendered as ark:scout#4033
// was asked to render it: a `kind: check` line and a fenced block under ### Check.
func order(block string) string {
	return "Claimed Elk run `run-1` (contract v2).\n\n**Direction:** Run quick-checks\n\n" +
		"## Handoff packet\n\n### Work order\nrepo: elk-work/scout\nkind: check\n" +
		"requires: agent:runner, os:linux, tool:docker\n\n### Check\n\n" + block + "\n\n" +
		"### Required capabilities — preflight BEFORE starting\n- docker\n"
}

const fullCheck = `{"repo":"elk-work/scout","sha":"` + testSHA + `","ref":"refs/heads/main","suite":"migration-numbers",` +
	`"workflow":".github/workflows/quick-checks.yml","job":"checks","runs_on":["ubuntu-latest"],"provider":"linux-lab",` +
	`"changed_paths":["supabase/migrations/0550_x.sql"],"some_future_field":{"x":1}}`

func TestParsePacketShapes(t *testing.T) {
	want := Check{Repo: "elk-work/scout", SHA: testSHA, Ref: "refs/heads/main", Suite: "migration-numbers",
		Workflow: ".github/workflows/quick-checks.yml", Job: "checks", RunsOn: []string{"ubuntu-latest"},
		Provider: "linux-lab", ChangedPaths: []string{"supabase/migrations/0550_x.sql"}}
	cases := map[string]string{
		"fenced bare object":      order("```json\n" + fullCheck + "\n```"),
		"fenced wrapped object":   order("```json\n{\"check\":" + fullCheck + "}\n```"),
		"fenced whole packet":     order("```\n{\"kind\":\"check\",\"description\":\"x\",\"check\":" + fullCheck + "}\n```"),
		"single check: line":      "**Direction:** x\n\ncheck: " + fullCheck + "\n",
		"check.field lines":       "check.repo: elk-work/scout\ncheck.sha: `" + testSHA + "`\ncheck.ref: refs/heads/main\ncheck.suite: migration-numbers\ncheck.workflow: .github/workflows/quick-checks.yml\ncheck.job: checks\ncheck.runs_on: [ubuntu-latest]\ncheck.provider: linux-lab\ncheck.changed_paths: supabase/migrations/0550_x.sql\n",
		"lines under ### Check":   "### Check\nrepo: elk-work/scout\nsha: " + testSHA + "\nref: refs/heads/main\nsuite: migration-numbers\nworkflow: .github/workflows/quick-checks.yml\njob: checks\nruns_on: ubuntu-latest\nprovider: linux-lab\nchanged_paths: supabase/migrations/0550_x.sql\n### Next\n",
		"other fences come first": "```text\nnot json\n```\n" + order("```json\n"+fullCheck+"\n```"),
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParsePacket(text)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got  %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestParsePacketRunsOnAsAString(t *testing.T) {
	c, err := ParsePacket("```json\n{\"sha\":\"" + testSHA + "\",\"workflow\":\".github/workflows/a.yml\",\"job\":\"j\",\"runs_on\":\"macos-26\"}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.RunsOn, []string{"macos-26"}) {
		t.Fatalf("runs_on = %v", c.RunsOn)
	}
}

func TestParsePacketWithoutOne(t *testing.T) {
	for _, text := range []string{
		"Claimed Elk run `r` (contract v2).\n\n**Direction:** Port the sheet\nrepo: scout\n",
		"```json\n{\"description\":\"a task, not a check\"}\n```",
	} {
		if _, err := ParsePacket(text); !errors.Is(err, ErrNoPacket) {
			t.Errorf("ParsePacket(%q) = %v, want ErrNoPacket", text, err)
		}
	}
}

func TestValidateRefusesWhatShouldNotReachACommandLine(t *testing.T) {
	ok := func() Check {
		return Check{Repo: "elk-work/scout", SHA: testSHA, Workflow: ".github/workflows/quick-checks.yml", Job: "checks"}
	}
	cases := map[string]func(*Check){
		"short sha":           func(c *Check) { c.SHA = "bfb8f96" },
		"sha with a flag":     func(c *Check) { c.SHA = "--upload-pack=x" },
		"workflow escapes":    func(c *Check) { c.Workflow = ".github/workflows/../../etc/passwd.yml" },
		"workflow elsewhere":  func(c *Check) { c.Workflow = "scripts/ci.yml" },
		"job is a flag":       func(c *Check) { c.Job = "--privileged" },
		"job has a newline":   func(c *Check) { c.Job = "a\nb" },
		"label with =":        func(c *Check) { c.RunsOn = []string{"ubuntu-latest=evil/image"} },
		"repo not owner/name": func(c *Check) { c.Repo = "scout" },
		"unknown event":       func(c *Check) { c.Event = "release" },
		"provider with space": func(c *Check) { c.Provider = "linux lab" },
	}
	for name, mutate := range cases {
		c := ok()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, c)
		}
	}
	c := ok()
	c.SHA = strings.ToUpper(testSHA)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.SHA != testSHA || c.Suite != "quick-checks/checks" || c.Provider != "rein" || c.Event != "workflow_dispatch" || c.ChangedPaths == nil {
		t.Errorf("defaults not applied: %+v", c)
	}
}

// The mapping elk docs/milestones/elk-ci.md §5.2 rules, which Elk's packet
// writer applies too.
func TestRequiredCapabilities(t *testing.T) {
	cases := []struct {
		labels []string
		want   []string
	}{
		{[]string{"ubuntu-latest"}, []string{"agent:runner", "os:linux", "tool:docker"}},
		{[]string{"ubuntu-slim"}, []string{"agent:runner", "os:linux", "tool:docker"}},
		{[]string{"self-hosted", "linux-lab"}, []string{"agent:runner", "os:linux", "tool:docker"}},
		{[]string{"macos-26"}, []string{"agent:runner", "os:macos", "tool:xcode"}},
		{[]string{"mac-01"}, []string{"agent:runner", "os:macos", "tool:xcode"}},
		{[]string{"self-hosted", "windows", "win11", "ui-lab"}, []string{"agent:runner", "os:windows", "desktop"}},
		{[]string{"windows-latest"}, []string{"agent:runner", "os:windows", "desktop"}},
		{nil, []string{"agent:runner"}},
	}
	for _, c := range cases {
		if got := RequiredCapabilities(c.labels); !reflect.DeepEqual(got, c.want) {
			t.Errorf("RequiredCapabilities(%v) = %v, want %v", c.labels, got, c.want)
		}
	}
}

func TestParseSettings(t *testing.T) {
	s, err := ParseSettings(nil)
	if err != nil || s.Image != DefaultImage || s.Executor != "" {
		t.Fatalf("defaults: %+v, %v", s, err)
	}
	s, err = ParseSettings(map[string]string{"executor": "host", "image": "ghcr.io/x/y:1", "labels": "a, b", "timeout": "30m", "event": "push"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Executor != "host" || s.Image != "ghcr.io/x/y:1" || !reflect.DeepEqual(s.Labels, []string{"a", "b"}) ||
		s.Timeout != 30*time.Minute || s.Event != "push" {
		t.Fatalf("parsed %+v", s)
	}
	for _, bad := range []map[string]string{
		{"executor": "docker"}, {"timeout": "soon"}, {"image": "a b"}, {"labels": "a=b"}, {"exeuctor": "host"},
	} {
		if _, err := ParseSettings(bad); err == nil {
			t.Errorf("ParseSettings(%v) accepted it", bad)
		}
	}
}

func TestChooseExecutor(t *testing.T) {
	if got := ChooseExecutor(Settings{}, []string{"ubuntu-latest"}); got != ExecutorContainer {
		t.Errorf("linux job: %s", got)
	}
	if got := ChooseExecutor(Settings{}, []string{"macos-26"}); got != ExecutorHost {
		t.Errorf("mac job: %s", got)
	}
	if got := ChooseExecutor(Settings{Executor: ExecutorHost}, []string{"ubuntu-latest"}); got != ExecutorHost {
		t.Errorf("the queue's setting must win: %s", got)
	}
}

func TestPlatformArgsMapEveryLabelOnce(t *testing.T) {
	args := platformArgs(ExecutorContainer, "img:1", []string{"ubuntu-latest", "custom"}, []string{"extra"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"-P ubuntu-latest=img:1", "-P custom=img:1", "-P extra=img:1", "-P ubuntu-slim=img:1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Count(joined, "ubuntu-latest=") != 1 {
		t.Errorf("ubuntu-latest mapped twice: %s", joined)
	}
	if !strings.HasPrefix(joined, "-P ubuntu-latest=img:1 -P custom=img:1") {
		t.Errorf("the packet's labels must come first: %s", joined)
	}
	host := strings.Join(platformArgs(ExecutorHost, "img:1", []string{"mac-01"}, nil), " ")
	if !strings.Contains(host, "-P mac-01=-self-hosted") || strings.Contains(host, "img:1") {
		t.Errorf("host mapping: %s", host)
	}
}

func TestParseListAndResolveJob(t *testing.T) {
	b, err := os.ReadFile("testdata/act-list.txt")
	if err != nil {
		t.Fatal(err)
	}
	jobs := parseList(string(b))
	if !reflect.DeepEqual(jobs, []listedJob{{ID: "floor", Name: "Floor check"}}) {
		t.Fatalf("parsed %+v", jobs)
	}
	// scout's quick-checks, as act --list printed it in the spike.
	scout := parseList("Stage  Job ID  Job name      Workflow name  Workflow file     Events                             \n" +
		"0      checks  Quick checks  quick-checks   quick-checks.yml  pull_request,push,workflow_dispatch\n")
	for want, wantNote := range map[string]bool{"checks": false, "Quick checks": true, "quick-checks": true, "CHECKS": true} {
		id, note, err := resolveJob(want, scout)
		if err != nil || id != "checks" || (note != "") != wantNote {
			t.Errorf("resolveJob(%q) = %q, %q, %v", want, id, note, err)
		}
	}
	two := []listedJob{{ID: "build", Name: "Build"}, {ID: "test", Name: "Test"}}
	if _, _, err := resolveJob("lint", two); err == nil || !strings.Contains(err.Error(), "build, test") {
		t.Errorf("an unknown job among several must be refused, naming them: %v", err)
	}
}

func tallyOf(t *testing.T, fixture string) (tally, []string) {
	t.Helper()
	b, err := os.ReadFile("testdata/" + fixture)
	if err != nil {
		t.Fatal(err)
	}
	var tl tally
	var lines []string
	for _, line := range strings.Split(string(b), "\n") {
		lines = append(lines, tl.render(line)...)
	}
	return tl, lines
}

func TestTallyFromRealActOutput(t *testing.T) {
	pass, lines := tallyOf(t, "act-pass.jsonl")
	if pass.jobResult != "success" || pass.setupFailed || pass.steps != 3 || pass.failed != 0 {
		t.Errorf("pass: %+v", pass)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "  | line 2") || !strings.Contains(joined, "🏁  Job succeeded") {
		t.Errorf("rendered lines:\n%s", joined)
	}
	if strings.Contains(joined, "\x1b") || strings.Contains(joined, "\r") {
		t.Errorf("control sequences survived rendering:\n%q", joined)
	}
	if r, _ := classify(pass, nil, false, false, time.Hour); r != ResultPass {
		t.Errorf("pass classified %s", r)
	}

	fail, _ := tallyOf(t, "act-fail.jsonl")
	if fail.jobResult != "failure" || fail.failed != 1 || fail.failedStep != "the failing step" || fail.setupFailed {
		t.Errorf("fail: %+v", fail)
	}
	if r, why := classify(fail, errors.New("exit status 1"), false, false, time.Hour); r != ResultFail || !strings.Contains(why, "the failing step") {
		t.Errorf("fail classified %s (%s)", r, why)
	}

	// An unmapped label: act fell back to its own image, could not reach
	// Docker, and failed "Set up job". That is the harness, not the code.
	setup, _ := tallyOf(t, "act-setup-failed.jsonl")
	if !setup.setupFailed || setup.steps != 0 {
		t.Errorf("setup: %+v", setup)
	}
	if r, _ := classify(setup, errors.New("exit status 1"), false, false, time.Hour); r != ResultError {
		t.Errorf("setup failure classified %s, want error", r)
	}
	if r, _ := classify(tally{}, errors.New("exit status 1"), false, false, time.Hour); r != ResultError {
		t.Errorf("no job line classified %s, want error", r)
	}
	if r, _ := classify(pass, nil, true, false, time.Hour); r != ResultError {
		t.Errorf("a timeout classified %s, want error", r)
	}
	if r, _ := classify(pass, nil, false, true, time.Hour); r != ResultCancelled {
		t.Errorf("an interrupt classified %s, want cancelled", r)
	}
}

func TestRecordShape(t *testing.T) {
	c := Check{Repo: "elk-work/scout", SHA: testSHA, Workflow: ".github/workflows/q.yml", Job: "checks"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC)
	rec := newRecord(c, "lab-checks", "", ResultFail, &tally{}, start, start.Add(1500*time.Millisecond))
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"counts", "failing_test_ids", "changed_paths"} {
		if got[k] == nil {
			t.Errorf("%s is null; pulse refuses a record without it: %s", k, b)
		}
	}
	if _, ok := got["branch"]; ok {
		t.Errorf("an unverified branch must be omitted: %s", b)
	}
	if got["schema_version"].(float64) != 1 || got["retention_class"] != "ci-history" || got["runner"] != "lab-checks" ||
		got["pr"].(float64) != 0 || got["duration_ms"].(float64) != 1500 || got["provider"] != "rein" || got["result"] != "fail" {
		t.Errorf("record: %s", b)
	}
}

func TestCandidateBranch(t *testing.T) {
	for ref, want := range map[string]string{
		"refs/heads/main": "main", "main": "main", "release/1.2": "release/1.2",
		"refs/pull/7/head": "", "7/merge": "", "refs/tags/v1": "", "HEAD": "", "": "", "-x": "",
	} {
		if got := candidateBranch(ref); got != want {
			t.Errorf("candidateBranch(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestOriginRepo(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:elk-work/scout.git":  "elk-work/scout",
		"https://github.com/elk-work/scout":  "elk-work/scout",
		"https://github.com/elk-work/scout/": "elk-work/scout",
		"/tmp/origin.git":                    "",
		"https://gitlab.com/a/b":             "",
	} {
		if got := originRepo(in); got != want {
			t.Errorf("originRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactEnv(t *testing.T) {
	env := []string{"STRIDE_TOKEN=supersecretvalue", "STRIDE_INGEST_URL=https://ingest.example/abc", "PATH=/usr/bin"}
	got := redactEnv("posting to https://ingest.example/abc with supersecretvalue from /usr/bin", env)
	if strings.Contains(got, "supersecretvalue") || strings.Contains(got, "ingest.example") || !strings.Contains(got, "/usr/bin") {
		t.Errorf("redacted: %q", got)
	}
}

const sharedCheck = `{"repo":"elk-work/scout","sha":"` + testSHA + `","ref":"refs/heads/main","suite":"migration-numbers",` +
	`"suites":["migration-numbers","no-schema-fallbacks"],"workflow":".github/workflows/quick-checks.yml","job":"checks",` +
	`"runs_on":["self-hosted","linux","linux-lab"],"provider":"linux-lab",` +
	`"changed_paths":["supabase/migrations/0559_check_runs_per_job.sql"]}`

func TestParsePacketReadsSuites(t *testing.T) {
	want := []string{"migration-numbers", "no-schema-fallbacks"}
	cases := map[string]string{
		"fenced whole packet":   order("```json\n{\"kind\":\"check\",\"repo\":\"elk-work/scout\",\"check\":" + sharedCheck + "}\n```"),
		"single check: line":    "check: " + sharedCheck + "\n",
		"check.field lines":     "check.sha: " + testSHA + "\ncheck.suite: migration-numbers\ncheck.suites: [migration-numbers, no-schema-fallbacks]\ncheck.workflow: .github/workflows/quick-checks.yml\ncheck.job: checks\n",
		"lines under ### Check": "### Check\nsha: " + testSHA + "\nsuites: migration-numbers, no-schema-fallbacks\nworkflow: .github/workflows/quick-checks.yml\njob: checks\n",
		"suites as one string":  "```json\n{\"sha\":\"" + testSHA + "\",\"suites\":\"migration-numbers, no-schema-fallbacks\",\"workflow\":\".github/workflows/quick-checks.yml\",\"job\":\"checks\"}\n```",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := ParsePacket(text)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c.Suites, want) || c.Suite != want[0] {
				t.Fatalf("suite %q, suites %v; want %v", c.Suite, c.Suites, want)
			}
		})
	}
	if _, err := ParsePacket("```json\n{\"sha\":\"" + testSHA + "\",\"suites\":[1,2]}\n```"); err == nil {
		// A fence that does not parse as a check is skipped, so this is no
		// packet at all rather than a packet with no suites.
		t.Error("suites of numbers were read as a check")
	}
}

func TestValidateSuites(t *testing.T) {
	cases := []struct {
		name      string
		suite     string
		suites    []string
		wantSuite string
		want      []string
	}{
		{"neither: the workflow and job", "", nil, "quick-checks/checks", []string{"quick-checks/checks"}},
		{"suite alone", "floor", nil, "floor", []string{"floor"}},
		{"suites empty", "floor", []string{}, "floor", []string{"floor"}},
		{"suites wins and suite is its first", "floor", []string{"a", "b"}, "a", []string{"a", "b"}},
		{"a name listed twice is recorded once", "a", []string{"a", " b ", "a", "b"}, "a", []string{"a", "b"}},
		{"blank entries are skipped", "", []string{"", "  ", "c"}, "c", []string{"c"}},
	}
	for _, tc := range cases {
		c := Check{SHA: testSHA, Workflow: ".github/workflows/quick-checks.yml", Job: "checks", Suite: tc.suite, Suites: tc.suites}
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if c.Suite != tc.wantSuite || !reflect.DeepEqual(c.Suites, tc.want) {
			t.Errorf("%s: suite %q, suites %q; want %q, %q", tc.name, c.Suite, c.Suites, tc.wantSuite, tc.want)
		}
	}

	many := make([]string, maxSuites+1)
	for i := range many {
		many[i] = fmt.Sprintf("s%d", i)
	}
	for name, suites := range map[string][]string{
		"a newline":         {"ok", "a\nb"},
		"a name over 200":   {"ok", strings.Repeat("s", 201)},
		"more than allowed": many,
	} {
		c := Check{SHA: testSHA, Workflow: ".github/workflows/quick-checks.yml", Job: "checks", Suites: suites}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "check.suites") {
			t.Errorf("%s: Validate = %v, want a check.suites refusal", name, err)
		}
	}
	bad := Check{SHA: testSHA, Workflow: ".github/workflows/quick-checks.yml", Job: "checks", Suite: "a\nb", Suites: []string{"ok"}}
	if err := bad.Validate(); err == nil {
		t.Error("an invalid suite was accepted because suites was present")
	}
}

// scout's quick-checks job covers six suites; one run for all of them must
// fit, with room for more. Names far longer than any real suite's are
// refused before the job runs, with a reason a person can act on.
func TestASharedRunThatCannotBeRecordedIsRefused(t *testing.T) {
	scout := func(suites []string) Check {
		return Check{Repo: "elk-work/scout", SHA: testSHA, Ref: "refs/heads/main", Workflow: ".github/workflows/quick-checks.yml",
			Job: "checks", Provider: "linux-lab", Suites: suites}
	}
	six := []string{"ia-parity", "parity-declare", "parity-matrix", "no-schema-fallbacks", "elk-type", "migration-numbers"}
	c := scout(six)
	if err := c.Validate(); err != nil {
		t.Fatalf("scout's quick-checks suites: %v", err)
	}
	fit := 0
	for n := 1; n <= maxSuites; n++ {
		var suites []string
		for i := 0; i < n; i++ {
			suites = append(suites, fmt.Sprintf("no-schema-fallbacks-%02d", i))
		}
		c := scout(suites)
		if c.Validate() != nil {
			break
		}
		fit = n
	}
	if fit < 8 {
		t.Errorf("only %d suites of 22-character names fit in one run", fit)
	}
	t.Logf("one run records at most %d suites of 22-character names", fit)

	var long []string
	for i := 0; i < 8; i++ {
		long = append(long, strings.Repeat("<", 199)+string(rune('a'+i)))
	}
	c = scout(long)
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "separate runs") || !strings.Contains(err.Error(), "8 suites") {
		t.Errorf("Validate = %v, want a refusal naming the count and saying to split the run", err)
	}
}
