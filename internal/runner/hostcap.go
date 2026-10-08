package runner

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
)

// A packet's `required_capabilities` and an adapter's manifest are two
// different namespaces, and conflating them is what made every real packet go
// stuck at preflight during the Phase 0 dogfood
// (`ark:rein#17 (01M17ZRR8QHBAK9TKCZYBS22PZ)`).
//
//   - The **adapter vocabulary** is nine closed RUNTIME names — git, worktree,
//     file_edit, shell, mcp, resume, structured_events, approvals, steer —
//     describing what the agent PROCESS can do. It is checked against
//     [adapter.Manifest], and [adapter.Capabilities] is the list.
//   - A packet's `required_capabilities` are ENVIRONMENT names: tools and
//     credentials this MACHINE must already hold. Elk's own harness emits
//     "elk-connector" and "github-cli" (scout/harness/handshake.test.mjs), and
//     Elk never passes the values — only the names.
//
// The two overlap by accident ("git" is in both) and are otherwise disjoint.
// Sending an environment name to `Manifest.Satisfies` means asking a question
// the manifest cannot answer, and its fail-closed rule — an unknown name is
// satisfied by nothing — correctly refuses it. The rule was right; the list was
// wrong.
//
// So [SplitRequirements] routes each name to the list that can actually answer
// it, and this file is the other list.

// builtinHostCapabilities are the environment names Rein satisfies by
// construction on every machine it runs on.
//
//   - elk-connector: Rein IS the connector. It holds the workspace token, and
//     claim/report/submit is the loop it exists to run.
//   - git, worktree: internal/worktree cuts a git worktree per run before the
//     agent starts. (Both are also adapter-vocabulary names, so a packet
//     naming them is answered by the manifest rather than here — they are
//     listed so that what Rein DECLARES to Elk is complete.)
var builtinHostCapabilities = []string{"elk-connector", "git", "worktree"}

// hostProbe establishes one environment capability.
type hostProbe struct {
	// name is the capability as a packet would name it.
	name string
	// binary must be on PATH.
	binary string
	// verify, when set, must also exit 0 — for capabilities where being
	// installed is not the same as being usable.
	verify []string
	// goos, when set, restricts the probe to one platform.
	goos string
}

// hostProbes is the startup auto-detection. Each is cheap: a PATH lookup, and
// at most one short subprocess.
//
// `gh` on PATH is not the same as `gh` logged in, and an unauthenticated `gh`
// fails at the first API call inside a run rather than at preflight — which is
// the whole thing preflight exists to prevent. Same argument for `xcodebuild`,
// which is present as a stub on a Mac with no Xcode selected and only answers
// `-version` when there is a real one.
var hostProbes = []hostProbe{
	{name: "github-cli", binary: "gh", verify: []string{"gh", "auth", "status"}},
	{name: "xcode", binary: "xcodebuild", verify: []string{"xcodebuild", "-version"}, goos: "darwin"},
	{name: "node", binary: "node"},
	{name: "go", binary: "go"},
	{name: "ark-cli", binary: "ark"},
	{name: "supabase-cli", binary: "supabase"},
	{name: "codex", binary: "codex"},
	{name: "claude", binary: "claude"},
	{name: "grok", binary: "grok"},
}

// EnvCapabilities replaces auto-detection with an explicit comma-separated
// list: REIN_HOST_CAPABILITIES="github-cli,xcode,docker".
//
// The built-ins and the config are still added, so this overrides only the
// probing. It exists for the two places probing is the wrong answer — a
// sandbox or CI box where shelling out to `gh auth status` is slow and
// pointless, and a test that must not depend on whether the machine running it
// happens to be logged in.
//
// **Set-but-empty means an empty list, not "unset".** `REIN_HOST_CAPABILITIES=`
// is how a CI job says "this box holds nothing beyond the built-ins", and it is
// what .github/workflows/ci.yml sets on all three legs. Reading it with
// os.Getenv and testing for "" would have silently turned that declaration back
// into a full probe — on a hosted runner, the one place the probe is most
// certainly wrong.
const EnvCapabilities = "REIN_HOST_CAPABILITIES"

// probeTimeout bounds one verification subprocess. `gh auth status` reaches the
// network, and a runner must not hang at startup because GitHub is slow.
const probeTimeout = 10 * time.Second

// Elk's limits on what an executor may declare
// (MAX_DECLARED_CAPABILITIES / MAX_CAPABILITY_CHARS in
// scout/supabase/functions/_shared/connected_executors.ts). Over either and the
// whole list is refused, so Rein trims rather than losing the declaration.
const (
	MaxDeclaredCapabilities = 64
	MaxCapabilityChars      = 64
)

// HostCapabilities is what this MACHINE holds: the environment half of a
// packet's `required_capabilities`.
//
// It records how each name was established, because the single most useful
// thing a `stuck` deliverable can say is not "you do not have xcode" but "this
// is the list I checked, this is where each entry came from, and this is how to
// add yours".
type HostCapabilities struct {
	how map[string]string
}

// NewHostCapabilities builds a set from explicit names, each recorded with the
// same provenance. It exists for tests and for anything that already knows the
// answer; [DetectHostCapabilities] is what a runner uses.
func NewHostCapabilities(how string, names ...string) *HostCapabilities {
	h := &HostCapabilities{how: map[string]string{}}
	for _, n := range names {
		h.add(n, how)
	}
	return h
}

func (h *HostCapabilities) add(name, how string) {
	name = canonicalCapability(name)
	if name == "" {
		return
	}
	if h.how == nil {
		h.how = map[string]string{}
	}
	if _, seen := h.how[name]; seen {
		return // first provenance wins; a built-in should not be relabelled
	}
	h.how[name] = how
}

// withExtra returns a copy carrying additional names, so a per-queue addition
// does not re-run the probes or leak into another queue's list.
func (h *HostCapabilities) withExtra(how string, names ...string) *HostCapabilities {
	out := &HostCapabilities{how: make(map[string]string, len(h.how)+len(names))}
	for k, v := range h.how {
		out.how[k] = v
	}
	for _, n := range names {
		out.add(n, how)
	}
	return out
}

// withoutProvenance returns a copy without the names established by how — one
// run's view of a queue that holds something only some of its runs may use.
func (h *HostCapabilities) withoutProvenance(how string) *HostCapabilities {
	out := &HostCapabilities{how: make(map[string]string, len(h.how))}
	for k, v := range h.how {
		if v != how {
			out.how[k] = v
		}
	}
	return out
}

// Has reports whether this machine holds a capability.
func (h *HostCapabilities) Has(name string) bool {
	if h == nil {
		return false
	}
	_, ok := h.how[canonicalCapability(name)]
	return ok
}

// Names returns every capability, sorted.
func (h *HostCapabilities) Names() []string {
	if h == nil {
		return nil
	}
	out := make([]string, 0, len(h.how))
	for n := range h.how {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Describe renders the list as "name (how it was established)", for a status
// table and for a stuck deliverable.
func (h *HostCapabilities) Describe() []string {
	names := h.Names()
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n + " (" + h.how[n] + ")"
	}
	return out
}

// Missing returns the required environment names this machine does not hold,
// in the order they were required and without duplicates.
//
// It fails closed, exactly as the manifest matcher does: a name nothing has
// declared is missing, whether it is a capability this machine genuinely lacks
// or a typo in a packet. The difference from the manifest is only which
// question is being asked.
func (h *HostCapabilities) Missing(required []string) []string {
	return h.missingWhere(required, h.Has)
}

// DetectHostCapabilities works out what this machine holds: the built-ins, then
// whatever the probes find, then whatever the config declares.
//
// Config comes last and is additive. Auto-detection can only ever find what it
// knows to look for, so the config is how a person says "this machine also has
// SUPABASE_SERVICE_ROLE_KEY" — a name no probe could establish without holding
// the credential, which Rein will not do.
func DetectHostCapabilities(ctx context.Context, cfg config.Config, extra ...string) *HostCapabilities {
	h := &HostCapabilities{how: map[string]string{}}
	for _, n := range builtinHostCapabilities {
		h.add(n, "built in")
	}
	// LookupEnv, not Getenv: setting the variable to nothing is a declaration
	// that this machine holds nothing, and it has to be distinguishable from
	// not setting it at all. See the constant's doc.
	if declared, set := os.LookupEnv(EnvCapabilities); set {
		for _, n := range strings.Split(declared, ",") {
			h.add(strings.TrimSpace(n), EnvCapabilities)
		}
		return withConfig(h, cfg, extra)
	}
	for _, p := range hostProbes {
		if p.goos != "" && p.goos != runtime.GOOS {
			continue
		}
		if _, err := exec.LookPath(p.binary); err != nil {
			continue
		}
		if len(p.verify) == 0 {
			h.add(p.name, p.binary+" on PATH")
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := exec.CommandContext(probeCtx, p.verify[0], p.verify[1:]...).Run()
		cancel()
		if err == nil {
			h.add(p.name, strings.Join(p.verify, " "))
		}
	}
	return withConfig(h, cfg, extra)
}

func withConfig(h *HostCapabilities, cfg config.Config, extra []string) *HostCapabilities {
	for _, n := range RepoCapabilities(context.Background(), cfg.Repos) {
		h.add(n, "configured checkout")
	}
	for _, n := range cfg.Capabilities {
		h.add(n, "config.toml")
	}
	for _, n := range extra {
		h.add(n, "config.toml, this queue")
	}
	return h
}

// SplitRequirements routes a packet's `required_capabilities` to the list that
// can answer each one: names in the adapter's closed vocabulary to the manifest,
// everything else to this machine.
//
// Only the runtime half may go into [adapter.RunSpec.RequiredCapabilities].
// Adapters re-check that field inside Start — the contract asks them to — so
// passing the whole packet list there would reintroduce the same refusal one
// layer down, after the worktree exists.
func SplitRequirements(required []string) (runtimeNames, hostNames []string) {
	for _, r := range required {
		name := canonicalCapability(r)
		if repo := pushRepo(name); repo != "" {
			hostNames = append(hostNames, "github-cli", "repo:"+repo)
			continue
		}
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, "runtime:") && strings.TrimSpace(strings.TrimPrefix(name, "runtime:")) != "" {
			runtimeNames = append(runtimeNames, strings.TrimPrefix(name, "runtime:"))
		} else if adapter.KnownCapability(adapter.Capability(name)) {
			runtimeNames = append(runtimeNames, name)
		} else {
			hostNames = append(hostNames, name)
		}
	}
	return runtimeNames, hostNames
}

// DeclaredCapabilities is what Rein tells Elk this executor can do: the
// adapter's runtime declaration union this machine's environment list.
//
// Both halves, because Elk's `declared_capabilities` is advisory input to Pace
// — "so Elk avoids handing you work you cannot do" — and a packet's
// requirements are drawn from both namespaces. Declaring only the runtime half
// would tell Pace nothing about the half that actually varies between machines.
func DeclaredCapabilities(m adapter.Manifest, host *HostCapabilities) []string {
	return declaredCapabilities(m, host, runtime.GOOS)
}

func declaredCapabilities(m adapter.Manifest, host *HostCapabilities, goos string) []string {
	// Reserve room for the legacy declaration first. New names must never
	// displace names older servers already consumed when Elk's limit is hit.
	legacy := m.Declared()
	var additions []string
	for _, name := range host.Names() {
		if host.how[name] == "configured checkout" {
			additions = append(additions, name)
		} else {
			legacy = append(legacy, name)
		}
	}
	sort.Strings(legacy)
	additions = append(additions, "agent:"+string(m.Kind), "os:"+hostOS(goos))
	for _, n := range m.Declared() {
		additions = append(additions, "runtime:"+n)
	}
	for _, n := range toolCapabilities {
		if host.verifiedTool(n) {
			additions = append(additions, "tool:"+n)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range append(legacy, additions...) {
		if n == "" || seen[n] || len(n) > MaxCapabilityChars {
			continue
		}
		seen[n] = true
		if len(out) < MaxDeclaredCapabilities {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Mirrored by hand from Elk's public.capability_vocabulary (scout migration
// 0295). Reserved namespaces are checked against their actual source.
var capabilityNamespaces = map[string]bool{
	"agent": true, "tool": true, "runtime": true, "repo": true,
	"mcp": true, "os": true, "does": true, "tier": true,
}
var toolCapabilities = []string{"git", "github-cli", "xcode", "node", "go", "supabase-cli", "ark-cli"}

func hostOS(goos string) string {
	if goos == "darwin" {
		return "macos"
	}
	return goos
}

func (h *HostCapabilities) verifiedTool(name string) bool {
	if !h.Has(name) {
		return false
	}
	how := h.how[name]
	return how != "config.toml" && how != "config.toml, this queue"
}

// canonicalCapability preserves the public Ark alias while declaring one name.
func canonicalCapability(name string) string {
	name = strings.TrimSpace(name)
	if name == "ark" {
		return "ark-cli"
	}
	if name == "tool:ark" {
		return "tool:ark-cli"
	}
	return name
}

var pushAccessRE = regexp.MustCompile(`^git push access to ([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)$`)

func pushRepo(name string) string {
	if match := pushAccessRE.FindStringSubmatch(name); match != nil {
		return match[1]
	}
	return ""
}

// AdvisoryRequirements identifies names outside Rein's vocabulary. Explicitly
// declared custom names remain requirements; reserved namespaces remain checked.
func AdvisoryRequirements(required []string, h *HostCapabilities) []string {
	var out []string
	for _, name := range required {
		name = canonicalCapability(name)
		if name == "" || h.Has(name) || strings.Contains(name, ":") && capabilityNamespaces[strings.SplitN(name, ":", 2)[0]] {
			continue
		}
		// pm is the Wrangler's routing capability, still `pm` on the wire.
		known := name == "supabase-vault" || name == "gcloud" || name == "pm"
		for _, n := range builtinHostCapabilities {
			known = known || name == n
		}
		for _, p := range hostProbes {
			known = known || name == p.name
		}
		if !known {
			out = append(out, name)
		}
	}
	return out
}

// MissingQueueRequirements checks the environment half against its actual
// source, rather than trusting arbitrary config declarations of reserved names.
func MissingQueueRequirements(required []string, m adapter.Manifest, h *HostCapabilities, mcps []string) []string {
	ignored := map[string]bool{}
	for _, name := range AdvisoryRequirements(required, h) {
		ignored[name] = true
	}
	return h.missingWhere(required, func(name string) bool {
		name = canonicalCapability(name)
		if ignored[name] {
			return true
		}
		ns, value, qualified := strings.Cut(name, ":")
		if !qualified {
			return h.Has(name)
		}
		if !capabilityNamespaces[ns] {
			return h.Has(name)
		}
		switch ns {
		case "agent":
			return value == string(m.Kind)
		case "os":
			return value == hostOS(runtime.GOOS)
		case "tool":
			for _, tool := range builtinHostCapabilities {
				if value == tool {
					return h.verifiedTool(value)
				}
			}
			for _, probe := range hostProbes {
				if value == probe.name {
					return h.verifiedTool(value)
				}
			}
			return false
		case "repo":
			return h.Has(name) && h.how[name] == "configured checkout"
		case "mcp":
			for _, allowed := range mcps {
				if name == allowed {
					return true
				}
			}
			return false
		case "does", "tier":
			return true
		}
		return false
	})
}

func (h *HostCapabilities) missingWhere(required []string, has func(string) bool) []string {
	var missing []string
	seen := map[string]bool{}
	for _, r := range required {
		name := strings.TrimSpace(r)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if !has(name) {
			missing = append(missing, name)
		}
	}
	return missing
}

// RepoCapabilities declares existing configured checkouts. Bare aliases require
// a GitHub origin; slash keys already supply the canonical identity.
func RepoCapabilities(ctx context.Context, repos map[string]string) []string {
	seen := map[string]bool{}
	for key, path := range repos {
		path = expand(path)
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		identity := key
		if !strings.Contains(key, "/") {
			probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			remote, err := exec.CommandContext(probeCtx, "git", "-C", path, "remote", "get-url", "origin").Output()
			cancel()
			if err != nil {
				continue
			}
			identity = githubOriginRepo(strings.TrimSpace(string(remote)))
		}
		parts := strings.Split(identity, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		seen["repo:"+identity] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func githubOriginRepo(remote string) string {
	var path string
	if strings.HasPrefix(remote, "git@github.com:") {
		path = strings.TrimPrefix(remote, "git@github.com:")
	} else {
		u, err := url.Parse(remote)
		if err != nil || u.Hostname() != "github.com" || (u.Scheme != "https" && u.Scheme != "ssh") {
			return ""
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return path
}
