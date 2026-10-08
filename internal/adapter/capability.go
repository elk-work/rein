package adapter

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// Capability is a named ability an adapter declares in its [Manifest] and a run
// requires in its packet.
//
// The names are Rein's, not any vendor's: an adapter translates them into
// whatever its own CLI calls the thing. They are deliberately coarse. A
// capability exists so a run can be refused *before* a worktree is created,
// which means it has to be something Elk can put in a packet without knowing
// which agent will pick it up.
type Capability string

// The capability vocabulary. This list is closed: [Satisfies] treats a required
// name outside it as missing, so a typo in a packet fails the run rather than
// silently satisfying nothing. Adding one is a change here plus a line in
// docs/adapters.md plus a re-declaration in every adapter that has it.
const (
	// CapGit — the session can run git operations in its worktree: status,
	// diff, add, commit, branch.
	CapGit Capability = "git"

	// CapWorktree — the session stays inside the directory it was given.
	// Distinct from CapGit: Loom's Codex adapter declares worktree isolation
	// "partial" precisely because the binary can reach outside, which is why
	// partial fails closed here (see [Satisfies]).
	CapWorktree Capability = "worktree"

	// CapFileEdit — the session can create, modify and delete files.
	CapFileEdit Capability = "file_edit"

	// CapShell — the session can run arbitrary shell commands.
	CapShell Capability = "shell"

	// CapMCP — the session can be given MCP servers to call, which is how a
	// run reaches Elk from inside the agent.
	CapMCP Capability = "mcp"

	// CapResume — a session can be resumed by id after the process exits, so a
	// restarted daemon continues rather than starting over.
	CapResume Capability = "resume"

	// CapStructuredEvents — the adapter reports through a machine-readable
	// event stream, not scrollback. Everything in Rein's watch loop depends on
	// this; an adapter without it is a scraper, and Rein does not scrape.
	CapStructuredEvents Capability = "structured_events"

	// CapApprovals — the session raises [EventPermissionRequest] and honours
	// [Session.Respond]. Without it, [PermissionAsk] cannot be served and the
	// only usable modes are the ones that decide up front.
	CapApprovals Capability = "approvals"

	// CapSteer — the session takes text after it has started, through
	// [Session.Send]: the answer to an [EventQuestion], or a mid-run
	// correction. Without it the only way to say anything to an agent is to
	// start it again.
	//
	// It is what makes `rein attach` a conversation rather than a window.
	// Grok declares it `no` — ACP has no mid-turn steer, and a follow-up
	// there is a new session against the same session id — so attaching to a
	// grok run is read-only, and says so before the person types anything
	// rather than after.
	CapSteer Capability = "steer"
)

// allCapabilities is the closed vocabulary, in the order docs/adapters.md
// lists them.
var allCapabilities = []Capability{
	CapGit, CapWorktree, CapFileEdit, CapShell,
	CapMCP, CapResume, CapStructuredEvents, CapApprovals, CapSteer,
}

// Capabilities returns the closed capability vocabulary.
func Capabilities() []Capability {
	out := make([]Capability, len(allCapabilities))
	copy(out, allCapabilities)
	return out
}

// KnownCapability reports whether c is in the vocabulary.
func KnownCapability(c Capability) bool {
	for _, k := range allCapabilities {
		if k == c {
			return true
		}
	}
	return false
}

// Support is how completely an adapter provides a capability. The tri-state is
// borrowed from Loom's defaults/runtimes/*.json, and so is the rule that only
// [SupportYes] satisfies a requirement — see [Satisfies].
type Support string

const (
	// SupportYes — provided, and proven. The only value that satisfies a
	// requirement.
	SupportYes Support = "yes"

	// SupportPartial — a real mechanism exists but is incomplete or unverified.
	// Fails closed, identically to [SupportNo]. Loom's Codex manifest carries
	// worktreeIsolation: "partial" for exactly this reason.
	SupportPartial Support = "partial"

	// SupportNo — not provided. Declared explicitly rather than omitted, so a
	// manifest reads as a statement instead of a gap.
	SupportNo Support = "no"
)

// Valid reports whether s is one of the three defined values. An adapter that
// invents a fourth is rejected by [Manifest.Validate] rather than being
// interpreted generously.
func (s Support) Valid() bool {
	switch s {
	case SupportYes, SupportPartial, SupportNo:
		return true
	}
	return false
}

// Platform is an OS/architecture pair an adapter runs on. Arch empty means
// every architecture on that OS.
type Platform struct {
	OS   string `json:"os"`   // a GOOS value: darwin, linux, windows
	Arch string `json:"arch"` // a GOARCH value, or "" for all
}

// String renders the platform as "darwin/arm64" or "windows".
func (p Platform) String() string {
	if p.Arch == "" {
		return p.OS
	}
	return p.OS + "/" + p.Arch
}

// Matches reports whether p covers a given GOOS/GOARCH.
func (p Platform) Matches(goos, goarch string) bool {
	return p.OS == goos && (p.Arch == "" || p.Arch == goarch)
}

// AnyArch returns a [Platform] covering every architecture on an OS.
func AnyArch(goos string) Platform { return Platform{OS: goos} }

// Manifest is what an adapter declares about itself: which agent kind it
// drives, what it can do, and where it runs.
//
// The shape is Loom's defaults/docs/runtime-adapters.md §7 (fetched
// 2026-08-29 from rjwalters/loom), with two changes. Loom keys its
// declaration on a fixed struct of five fields; Rein keys on the open-ended
// [Capability] vocabulary above, because Rein's requirements arrive from Elk
// as `required_capabilities`, a names-only list (elk docs/rein.md §2).
// And Rein adds [Manifest.Platforms], because Loom is Unix-only by
// construction and Rein's Phase 1 is a Windows box.
//
// What is borrowed unchanged is the part that matters: the tri-state, and
// **fail-closed** matching — only "yes" satisfies, so "partial" and "no" and
// "not declared at all" are all equally a refusal.
type Manifest struct {
	// Kind is the agent kind this adapter drives — "claude", "codex", "grok".
	// It is the registry key and the value of `agent_kind` in config.toml.
	Kind string `json:"kind"`

	// Binary is the executable the adapter shells out to, for diagnostics and
	// for a preflight message a human can act on.
	Binary string `json:"binary,omitempty"`

	// Capabilities is the declaration. Every capability the adapter has an
	// opinion about should appear, including the ones it declares "no": an
	// omission and a "no" fail identically, but only one of them reads as a
	// decision.
	Capabilities map[Capability]Support `json:"capabilities"`

	// Platforms lists where this adapter is supported. Empty means nowhere —
	// fail closed here too.
	Platforms []Platform `json:"platforms"`

	// Notes records residual gaps in prose, the way Loom's parity documents
	// do. The manifest is what is enforced; this is what a human reads when
	// they want to know why something is "partial".
	Notes string `json:"notes,omitempty"`
}

// Supports returns the declared [Support] for a capability. An undeclared
// capability reads as [SupportNo].
func (m Manifest) Supports(c Capability) Support {
	if s, ok := m.Capabilities[c]; ok {
		return s
	}
	return SupportNo
}

// Has reports whether the capability is declared [SupportYes].
func (m Manifest) Has(c Capability) bool { return m.Supports(c) == SupportYes }

// Satisfies checks a run's required capabilities against this manifest and
// returns the ones it does not meet. An empty return means the adapter may
// take the run.
//
// It takes []string rather than []Capability because that is how the
// requirement arrives: Elk's packet carries `required_capabilities` as names,
// and the whole point is to catch a name this build has never heard of.
//
// **Fail closed.** A requirement is a miss when it is any of:
//
//   - outside the capability vocabulary — an unknown name cannot be satisfied
//     by anything, so a typo in a packet stops the run instead of passing
//     vacuously;
//   - undeclared in this manifest;
//   - declared [SupportPartial] or [SupportNo], which fail identically —
//     Loom's rule, and the reason a tier-3 manifest can honestly say "no"
//     without the matcher treating it differently from "close but gapped";
//   - declared with a value outside the tri-state.
//
// The result preserves the order the requirements were given in and drops
// duplicates, so an operator reading the error sees their own list back.
func (m Manifest) Satisfies(required []string) (missing []string) {
	seen := make(map[string]bool, len(required))
	for _, r := range required {
		name := strings.TrimSpace(r)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		c := Capability(name)
		if !KnownCapability(c) || m.Supports(c) != SupportYes {
			missing = append(missing, name)
		}
	}
	return missing
}

// SupportsPlatform reports whether the adapter runs on a GOOS/GOARCH. An empty
// [Manifest.Platforms] supports nothing.
func (m Manifest) SupportsPlatform(goos, goarch string) bool {
	for _, p := range m.Platforms {
		if p.Matches(goos, goarch) {
			return true
		}
	}
	return false
}

// SupportsHost reports whether the adapter runs on the machine executing this
// code.
func (m Manifest) SupportsHost() bool {
	return m.SupportsPlatform(runtime.GOOS, runtime.GOARCH)
}

// Declared returns the capabilities declared [SupportYes], sorted, for
// reporting to Elk as this executor's `declared_capabilities`
// (elk docs/rein.md §5b).
func (m Manifest) Declared() []string {
	out := make([]string, 0, len(m.Capabilities))
	for c, s := range m.Capabilities {
		if s == SupportYes {
			out = append(out, string(c))
		}
	}
	sort.Strings(out)
	return out
}

// Validate reports the first thing wrong with the manifest itself. [Register]
// calls it, so a malformed manifest fails at startup rather than at dispatch.
//
// An undeclared capability is legal — it reads as "no". A capability declared
// under a name outside the vocabulary is not: nothing can ever require it, so
// it is dead weight at best and a misspelling of a real one at worst.
func (m Manifest) Validate() error {
	if m.Kind == "" {
		return fmt.Errorf("adapter: manifest has no kind")
	}
	if strings.ContainsAny(m.Kind, " \t/") {
		return fmt.Errorf("adapter: kind %q: no spaces or slashes", m.Kind)
	}
	names := make([]Capability, 0, len(m.Capabilities))
	for c := range m.Capabilities {
		names = append(names, c)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	for _, c := range names {
		if !KnownCapability(c) {
			return fmt.Errorf("adapter %s: declares unknown capability %q; the vocabulary is %s",
				m.Kind, c, joinCapabilities(allCapabilities))
		}
		if s := m.Capabilities[c]; !s.Valid() {
			return fmt.Errorf("adapter %s: capability %q declared %q; want yes, partial or no",
				m.Kind, c, s)
		}
	}
	if len(m.Platforms) == 0 {
		return fmt.Errorf("adapter %s: declares no platforms", m.Kind)
	}
	for _, p := range m.Platforms {
		if p.OS == "" {
			return fmt.Errorf("adapter %s: platform with no OS", m.Kind)
		}
	}
	return nil
}

func joinCapabilities(cs []Capability) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = string(c)
	}
	return strings.Join(parts, ", ")
}
