package check

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Check is a check packet's `check` object: which job of which workflow to
// run, at which commit of which repository, and what to record it as.
//
// The names are the packet contract shared with Elk (scout harness/packet.mjs
// and elk-mcp/queue.ts, ark:scout#4033 (01M5RKEFGX)). Rein does not invent
// any of them, and ignores fields it does not know, so Elk can add one
// without a Rein release.
type Check struct {
	// Repo is the repository under test, `owner/name`.
	Repo string `json:"repo"`
	// SHA is the commit the job runs at, a full object name.
	SHA string `json:"sha"`
	// Ref is the ref the commit was dispatched from, when Elk knows it —
	// `refs/heads/main`, `main`, `refs/pull/7/head`. It decides only whether
	// the record may name a branch, and only after the remote confirms it.
	Ref string `json:"ref"`
	// Suite is the suite the result is recorded as (ci/suites.yml).
	Suite string `json:"suite"`
	// Workflow is the workflow file, relative to the repository root.
	Workflow string `json:"workflow"`
	// Job is the job's id in that file. A job's name is accepted too, and a
	// workflow with one job runs that job — see [resolveJob].
	Job string `json:"job"`
	// RunsOn is the job's runs-on labels as Elk read them.
	RunsOn []string `json:"runs_on"`
	// Provider is the provider category the manifest chose: linux-lab,
	// mac-lab, win-lab, hosted or rein.
	Provider string `json:"provider"`
	// ChangedPaths is the diff the check was dispatched for.
	ChangedPaths []string `json:"changed_paths"`
	// Event is the GitHub event the job runs as. Not part of the contract
	// yet; empty means workflow_dispatch.
	Event string `json:"event"`
}

// UnmarshalJSON reads a check tolerantly: runs_on and changed_paths may be a
// string or an array, and anything unknown is ignored.
func (c *Check) UnmarshalJSON(b []byte) error {
	var raw struct {
		Repo         string          `json:"repo"`
		SHA          string          `json:"sha"`
		Ref          string          `json:"ref"`
		Suite        string          `json:"suite"`
		Workflow     string          `json:"workflow"`
		Job          string          `json:"job"`
		RunsOn       json.RawMessage `json:"runs_on"`
		Provider     string          `json:"provider"`
		ChangedPaths json.RawMessage `json:"changed_paths"`
		Event        string          `json:"event"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*c = Check{Repo: raw.Repo, SHA: raw.SHA, Ref: raw.Ref, Suite: raw.Suite, Workflow: raw.Workflow,
		Job: raw.Job, Provider: raw.Provider, Event: raw.Event}
	var err error
	if c.RunsOn, err = stringList(raw.RunsOn); err != nil {
		return fmt.Errorf("runs_on: %w", err)
	}
	if c.ChangedPaths, err = stringList(raw.ChangedPaths); err != nil {
		return fmt.Errorf("changed_paths: %w", err)
	}
	return nil
}

func stringList(b json.RawMessage) ([]string, error) {
	if len(b) == 0 || string(b) == "null" {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		return splitList(one), nil
	}
	var many []any
	if err := json.Unmarshal(b, &many); err != nil {
		return nil, errors.New("want a string or an array of strings")
	}
	out := make([]string, 0, len(many))
	for _, v := range many {
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("want an array of strings")
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// splitList reads "a, b" or "[a, b]" as a list.
func splitList(s string) []string {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "["), "]"))
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.Trim(strings.TrimSpace(part), `"'`)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// ErrNoPacket reports a work order with no check packet in it.
var ErrNoPacket = errors.New("check: the work order carries no check packet")

var (
	fenceRE     = regexp.MustCompile("(?s)```[A-Za-z]*[ \\t]*\\r?\\n(.*?)```")
	checkLineRE = regexp.MustCompile(`(?m)^[ \t>*-]*check:[ \t]*(\{.*\})[ \t]*$`)
	fieldLineRE = regexp.MustCompile(`(?m)^[ \t>*-]*check\.([a-z_]+):[ \t]*(.+?)[ \t]*$`)
	headingRE   = regexp.MustCompile(`(?m)^#{2,4}[ \t]+Check[ \t]*$`)
	kvLineRE    = regexp.MustCompile(`^[ \t>*-]*([a-z_]+):[ \t]*(.+?)[ \t]*$`)
)

// ParsePacket finds the check packet in a work order.
//
// A work order is markdown: `claim_run` has no structured content, so Elk
// renders the packet into the claim text and Rein reads it back. Three
// shapes are accepted, first match wins, because the rendering lives in
// another repository and a reader that insists on one spelling turns a
// cosmetic change there into every check going stuck:
//
//  1. a fenced code block holding the check object, or `{"check": {…}}`, or
//     the whole packet `{"kind": "check", "check": {…}}`;
//  2. a line `check: {…}` holding the object as one line of JSON;
//  3. lines `check.sha: …`, `check.job: …` and so on, or `sha: …` lines under
//     a `### Check` heading.
//
// Nothing is interpreted beyond the fields of [Check], and every value is
// checked by [Check.Validate] before it reaches a command line: the work
// order is data, and its woven half comes from captures nobody vouched for.
func ParsePacket(text string) (Check, error) {
	for _, m := range fenceRE.FindAllStringSubmatch(text, -1) {
		if c, ok := fromJSON(m[1]); ok {
			return c, nil
		}
	}
	for _, m := range checkLineRE.FindAllStringSubmatch(text, -1) {
		if c, ok := fromJSON(m[1]); ok {
			return c, nil
		}
	}
	fields := map[string]string{}
	for _, m := range fieldLineRE.FindAllStringSubmatch(text, -1) {
		if _, seen := fields[m[1]]; !seen {
			fields[m[1]] = m[2]
		}
	}
	if loc := headingRE.FindStringIndex(text); loc != nil && len(fields) == 0 {
		for _, line := range strings.Split(text[loc[1]:], "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				break
			}
			if m := kvLineRE.FindStringSubmatch(line); m != nil {
				if _, seen := fields[m[1]]; !seen {
					fields[m[1]] = m[2]
				}
			}
		}
	}
	if fields["sha"] == "" {
		return Check{}, ErrNoPacket
	}
	unquote := func(s string) string { return strings.Trim(strings.TrimSpace(s), "`\"'") }
	return Check{
		Repo: unquote(fields["repo"]), SHA: unquote(fields["sha"]), Ref: unquote(fields["ref"]),
		Suite: unquote(fields["suite"]), Workflow: unquote(fields["workflow"]), Job: unquote(fields["job"]),
		RunsOn: splitList(fields["runs_on"]), Provider: unquote(fields["provider"]),
		ChangedPaths: splitList(fields["changed_paths"]), Event: unquote(fields["event"]),
	}, nil
}

// fromJSON reads a check object, bare or wrapped, from a JSON document.
func fromJSON(doc string) (Check, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(doc)), &top); err != nil {
		return Check{}, false
	}
	body := []byte(strings.TrimSpace(doc))
	if inner, ok := top["check"]; ok {
		body = inner
	} else if _, ok := top["sha"]; !ok {
		return Check{}, false
	}
	var c Check
	if err := json.Unmarshal(body, &c); err != nil || c.SHA == "" {
		return Check{}, false
	}
	return c, true
}

var (
	shaRE      = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	repoRE     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,39}/[A-Za-z0-9_.-]{1,100}$`) // GitHub's own limits
	workflowRE = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml$`)
	labelRE    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	tokenRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
)

// events a check may run as. A workflow_dispatch is the default: a check is
// Elk dispatching a job, which is the nearest thing GitHub has.
var events = map[string]bool{"workflow_dispatch": true, "push": true, "pull_request": true, "schedule": true}

// Validate normalises the check and refuses anything that should not reach a
// git or act command line. It returns the first problem, phrased for the
// person reading a stuck run.
func (c *Check) Validate() error {
	c.SHA = strings.ToLower(strings.TrimSpace(c.SHA))
	c.Repo = strings.TrimSpace(c.Repo)
	c.Workflow = strings.TrimPrefix(strings.TrimSpace(c.Workflow), "./")
	c.Job = strings.TrimSpace(c.Job)
	c.Suite = strings.TrimSpace(c.Suite)
	c.Provider = strings.TrimSpace(c.Provider)
	c.Ref = strings.TrimSpace(c.Ref)
	c.Event = strings.TrimSpace(c.Event)
	switch {
	case !shaRE.MatchString(c.SHA):
		return fmt.Errorf("check.sha %q is not a full commit id (40 or 64 hex characters)", c.SHA)
	case c.Repo != "" && !repoRE.MatchString(c.Repo):
		return fmt.Errorf("check.repo %q is not owner/name", c.Repo)
	case !workflowRE.MatchString(c.Workflow) || path.Clean(c.Workflow) != c.Workflow:
		return fmt.Errorf("check.workflow %q is not a file under .github/workflows/", c.Workflow)
	case c.Job == "" || len(c.Job) > 100 || strings.HasPrefix(c.Job, "-") || strings.ContainsAny(c.Job, "\r\n\t"):
		return fmt.Errorf("check.job %q is not a job id or name", c.Job)
	case c.Provider != "" && !tokenRE.MatchString(c.Provider):
		return fmt.Errorf("check.provider %q is not a provider name", c.Provider)
	case strings.ContainsAny(c.Ref, " \t\r\n") || len(c.Ref) > 255:
		return fmt.Errorf("check.ref %q is not a ref", c.Ref)
	case c.Event != "" && !events[c.Event]:
		return fmt.Errorf("check.event %q: want workflow_dispatch, push, pull_request or schedule", c.Event)
	case len(c.RunsOn) > 16:
		return fmt.Errorf("check.runs_on has %d labels; at most 16", len(c.RunsOn))
	case len(c.ChangedPaths) > 10000:
		return fmt.Errorf("check.changed_paths has %d paths; at most 10000", len(c.ChangedPaths))
	}
	for _, l := range c.RunsOn {
		if !labelRE.MatchString(l) {
			return fmt.Errorf("check.runs_on label %q is not a runner label", l)
		}
	}
	if c.Suite != "" && (len(c.Suite) > 200 || strings.ContainsAny(c.Suite, "\r\n")) {
		return fmt.Errorf("check.suite %q is not a suite name", c.Suite)
	}
	if c.Suite == "" {
		c.Suite = strings.TrimSuffix(strings.TrimSuffix(path.Base(c.Workflow), ".yml"), ".yaml") + "/" + c.Job
	}
	if c.Provider == "" {
		c.Provider = "rein"
	}
	if c.Event == "" {
		c.Event = "workflow_dispatch"
	}
	if c.ChangedPaths == nil {
		c.ChangedPaths = []string{}
	}
	return nil
}

// Family is the platform a set of runs-on labels asks for: "linux", "macos",
// "windows", or "" when no label says. The first decisive label wins, so
// `[self-hosted, linux-lab]` is linux.
func Family(labels []string) string {
	for _, l := range labels {
		l = strings.ToLower(l)
		switch {
		case strings.HasPrefix(l, "ubuntu") || l == "linux" || l == "linux-lab":
			return "linux"
		case strings.HasPrefix(l, "macos") || l == "mac-01" || l == "mac-lab" || l == "macos":
			return "macos"
		case strings.HasPrefix(l, "windows") || strings.HasPrefix(l, "win-") || l == "win11" || l == "win-lab":
			return "windows"
		}
	}
	return ""
}

// RequiredCapabilities is what a check whose job runs on labels requires of
// the machine that takes it — the mapping elk docs/milestones/elk-ci.md §5.2
// rules: `ubuntu-*` and `linux-lab` need `os:linux tool:docker`, `macos-*`
// and `mac-01` need `os:macos tool:xcode`, and a Windows label needs
// `os:windows desktop`. Every check needs `agent:runner`.
//
// Elk computes the same list when it writes the packet; Rein keeps its own
// copy so a check can say what it needed when it is refused, and so the two
// can be compared in a test rather than in production.
func RequiredCapabilities(labels []string) []string {
	out := []string{"agent:" + Kind}
	switch Family(labels) {
	case "linux":
		out = append(out, "os:linux", "tool:docker")
	case "macos":
		out = append(out, "os:macos", "tool:xcode")
	case "windows":
		out = append(out, "os:windows", "desktop")
	}
	return out
}
