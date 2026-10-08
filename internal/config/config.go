// Package config reads and writes Rein's on-disk configuration.
//
// Rein keeps everything it owns under one directory — the "Rein home" —
// resolved by [Dir]:
//
//	$REIN_HOME                if set (tests and CI use this)
//	$HOME/.rein               the documented default, every platform
//	os.UserConfigDir()/rein   only when the home directory is undiscoverable
//
// One directory rather than the platform's config location keeps the path in
// elk's docs/rein.md (§5a: worktrees under ~/.rein/work/<run>) true as written
// on macOS, Linux and Windows alike, and keeps the config beside the work it
// describes. The os.UserConfigDir fallback exists so a service account with no
// HOME still starts.
//
// The file itself is TOML at <Dir>/config.toml. It holds no secrets: run-scoped
// Elk tokens live in the OS keychain (internal/keyring), keyed by workspace and
// queue, and a queue's secrets map names keychain items rather than holding
// their values. See docs/adapters.md and elk-work/elk docs/rein.md for the wider shape.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	// The zone database, compiled in, so a `weekly_reset` naming an IANA zone
	// reads the same on a Windows runner — which has no zoneinfo of its own —
	// as on a Mac.
	_ "time/tzdata"

	"github.com/BurntSushi/toml"

	"github.com/elk-work/rein/internal/secretenv"
)

// FileName is the config file's basename inside [Dir].
const FileName = "config.toml"

// EnvHome overrides the Rein home directory.
const EnvHome = "REIN_HOME"

// QueueNameMax is the longest queue name Elk accepts. Ruling 3 in elk's
// docs/rein.md raised the cap from 10 to 12 and made truncation a rejection —
// so Rein rejects here too rather than silently shipping a name the server
// will refuse or mangle.
const QueueNameMax = 12

// Config is the whole of ~/.rein/config.toml.
type Config struct {
	// SelfRestart defaults to true; false leaves upgrades to the operator.
	SelfRestart *bool `toml:"self_restart,omitempty"`

	// HostID identifies this machine within a workspace. Short by convention —
	// three letters like "mac" or "win" — because queue names are
	// "<host>-<agent>" and must fit in QueueNameMax.
	HostID string `toml:"host_id"`

	// MachineID is the opaque, stable identifier Elk stores as an executor's
	// `host_id`. It is NOT [Config.HostID], and the two are deliberately
	// different things:
	//
	//   - HostID is a three-letter label that makes "mac-claude" readable. It
	//     is chosen by a person and can change.
	//   - MachineID is minted once by [NewMachineID] and never changes. Elk's
	//     own guidance is to send "a value that survives a hostname change (a
	//     stored UUID), not the hostname itself", because host_id + agent_kind
	//     is the durable identity that stops a laptop and a desktop whose
	//     queues share a name from being one row.
	//
	// A machine that regenerated this would split its own history in Elk, so
	// `rein enrol` mints it once and every later enrolment reuses it.
	MachineID string `toml:"machine_id"`

	// Workspace is the default Elk workspace for queues that do not name one.
	Workspace string `toml:"workspace"`

	// WorkDir is where per-run git worktrees are created. Defaults to
	// <Dir>/work.
	WorkDir string `toml:"work_dir"`

	// DefaultRepo is the repository a run is cut from when nothing more
	// specific says otherwise. Elk's work order carries no repository field —
	// `claim_run` returns the packet, the action, the context and the
	// reporting contract, and nothing that names a repo — so a runner has to
	// be told, and this is the last of the four places it looks. See
	// docs/run-loop.md.
	DefaultRepo string `toml:"default_repo"`

	// LogKeepRuns is how many runs' event logs are kept under <Dir>/log/runs.
	// Zero means [DefaultLogKeepRuns]; a negative number keeps everything.
	//
	// The logs are pruned where the worktree reaper runs, for the reason the
	// reaper exists at all: the cheapest moment to tidy up after a run is the
	// moment one finishes, and nothing else is going to remember. They are
	// small — a busy run is a few hundred kilobytes — and they outlive the
	// worktree deliberately, because once the worktree is reaped the log is
	// the only account of what happened in it.
	LogKeepRuns int `toml:"log_keep_runs"`

	// Capabilities are extra ENVIRONMENT capability names this machine holds,
	// beyond the ones Rein detects for itself: credentials, and tools no probe
	// knows to look for.
	//
	//	capabilities = ["SUPABASE_SERVICE_ROLE_KEY", "docker"]
	//
	// These are the namespace a packet's `required_capabilities` is written in
	// — NAMES only, never values. Rein is not a credential broker any more than
	// Elk is: declaring a credential here says this machine already has it in
	// its environment, and nothing more. See internal/runner/hostcap.go.
	Capabilities []string `toml:"capabilities"`

	// InheritEnv names variables from the daemon's environment that every
	// run on every queue keeps, beyond the system variables any process needs
	// (internal/secretenv). Every run is scoped now: a queue inherits only
	// these, its own [Queue.InheritEnv], the credential-shaped names in its
	// capabilities and its allowed MCP servers' keys. Names only; a variable
	// that would put an agent on metered API billing is refused.
	//
	//	inherit_env = ["SENTRY_ORG"]
	InheritEnv []string `toml:"inherit_env,omitempty"`

	// BaseRefs maps a repository name — the same keys as [Config.Repos] — to
	// the ref a run should be cut from:
	//
	//	[base_refs]
	//	scout = "origin/develop"
	//
	// Almost nothing needs this. Rein resolves the remote's own default branch
	// on its own, which is right for every repository in the org; this is the
	// escape hatch for one that releases from somewhere else.
	BaseRefs map[string]string `toml:"base_refs"`

	// Repos maps a name a work order might use to a local checkout:
	//
	//	[repos]
	//	scout            = "/Users/you/dev/elk/scout"
	//	"elk-work/rein"  = "/Users/you/dev/rein"
	//
	// A `repo:` line in a Do direction, or a github.com URL anywhere in the
	// packet, is resolved through this table. It exists because the name in a
	// work order is written by a person for a person — "scout", not a path —
	// and only this machine knows where that is checked out.
	Repos map[string]string `toml:"repos"`

	// Elk holds the connection to the Elk MCP endpoint.
	Elk Elk `toml:"elk"`

	// Telemetry configures the fleet reading this machine sends Elk on its
	// heartbeat. See internal/runner/telemetry.go.
	Telemetry Telemetry `toml:"telemetry"`

	// MCPServers defines the MCP servers a queue may allow its runs, by name.
	// A definition alone reaches no run: a queue must name it in its own
	// `mcp_servers` list (ark:rein#40).
	//
	//	[mcp_servers.posthog]
	//	url                  = "https://mcp.posthog.com/mcp"
	//	bearer_token_env_var = "POSTHOG_API_KEY"   # a NAME, never the token
	//
	//	[mcp_servers.context7]
	//	command  = "npx"
	//	args     = ["-y", "@upstash/context7-mcp"]
	//	env_vars = ["CONTEXT7_API_KEY"]             # NAMES passed through
	MCPServers map[string]MCPServer `toml:"mcp_servers"`

	// Queues are the named per-(host, agent kind) queues this machine serves.
	Queues []Queue `toml:"queues"`
}

// MCPServer is one [Config.MCPServers] definition, in Rein's vendor-neutral
// shape. Exactly one of URL and Command. It holds no secret: a token or key is
// named by the environment variable that carries it, and the vendor binary
// reads the value.
type MCPServer struct {
	URL               string   `toml:"url"`
	BearerTokenEnvVar string   `toml:"bearer_token_env_var"`
	Command           string   `toml:"command"`
	Args              []string `toml:"args"`
	EnvVars           []string `toml:"env_vars"`
}

// mcpNameRE mirrors internal/adapter's rule for a server name — duplicated
// rather than imported so config stays a leaf package, the same trade as
// permissionModes below. The adapter contract is the authority.
var mcpNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validate reports what makes a definition unusable. Refusing it here, at
// load, means a typo stops `rein run` with the line to fix rather than
// quietly leaving a server out of every run.
func (s MCPServer) validate(name string) error {
	where := "mcp_servers." + name
	switch {
	case !mcpNameRE.MatchString(name):
		return fmt.Errorf("%s: a server name is letters, digits, _ and - only, at most 64", where)
	case s.URL == "" && s.Command == "":
		return fmt.Errorf("%s: needs url (a remote server) or command (a local one)", where)
	case s.URL != "" && s.Command != "":
		return fmt.Errorf("%s: has both url and command; pick one", where)
	case s.URL != "" && (len(s.Args) > 0 || len(s.EnvVars) > 0):
		return fmt.Errorf("%s: args and env_vars belong to a command, not a url", where)
	case s.Command != "" && s.BearerTokenEnvVar != "":
		return fmt.Errorf("%s: bearer_token_env_var belongs to a url, not a command", where)
	case s.URL != "" && !strings.HasPrefix(s.URL, "https://") && !strings.HasPrefix(s.URL, "http://"):
		return fmt.Errorf("%s: url %q is not http(s)", where, s.URL)
	}
	for _, env := range append(append([]string(nil), s.EnvVars...), s.BearerTokenEnvVar) {
		if secretenv.Metered(env) {
			// The server runs inside the agent's process tree, so its key is
			// in the agent's environment too.
			return fmt.Errorf("%s: %s %s", where, env, meteredWhy)
		}
	}
	return nil
}

// Elk is the [Config] block describing the Elk endpoint.
type Elk struct {
	// MCPURL is the Elk MCP endpoint, e.g.
	// https://<project>.supabase.co/functions/v1/elk-mcp.
	MCPURL string `toml:"mcp_url"`
}

// Telemetry is the [Config] block for the fleet reading:
//
//	[telemetry]
//	interval = "60s"
//	disabled = false
type Telemetry struct {
	// Interval is how often the machine is measured, and how long a queue may
	// go without a beat before the telemetry tick sends one on its behalf.
	// Zero means the runner's default.
	Interval Duration `toml:"interval"`

	// Disabled turns the reading off: the tick does not run and no beat
	// carries a host or session object. The queue still heartbeats, so the
	// machine still shows as online — this stops Rein DESCRIBING the machine,
	// not being one.
	//
	// A negative switch rather than `enabled`, because the zero value of a
	// bool is false and the feature is meant to be on. `enabled = false` in a
	// struct with no tri-state would turn telemetry off for everybody who
	// never wrote the block.
	Disabled bool `toml:"disabled"`
}

// Duration is a [time.Duration] that reads and writes as TOML text — "60s",
// "5m" — rather than as a count of nanoseconds nobody can check by eye.
//
// TOML has no duration type and BurntSushi/toml honours
// [encoding.TextUnmarshaler], so this is the whole of what it takes. The CLI's
// duration flags already take the same spelling (`--review-timeout 20m`), and
// a config that disagreed with the flags about how long twenty minutes is
// written down would be its own small trap.
type Duration time.Duration

// Duration returns the wrapped value.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// UnmarshalText implements [encoding.TextUnmarshaler]. An empty string is
// zero, which every reader of a Duration treats as "use the default".
func (d *Duration) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration (want something like \"60s\" or \"5m\"): %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalText implements [encoding.TextMarshaler], so a config Rein writes
// round-trips through a config Rein reads.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// Queue is one named queue: a (host, agent kind) pair this machine claims runs
// for. Elk's durable identity is host_id + agent_kind; Name is a label.
type Queue struct {
	// Wrangler opts this Claude or Codex queue into running the Wrangler, Elk's PM agent
	// (elk docs/wrangler.md). Read it through [Queue.IsWrangler], which also
	// honours the old spelling below.
	Wrangler bool `toml:"wrangler,omitempty"`
	// WranglerConnectorAccount names the owner binding in keychain service
	// elk-connector-url. Empty uses <workspace>/<queue>. No secret value
	// belongs in config. Read it through [Queue.WranglerAccount].
	WranglerConnectorAccount string `toml:"wrangler_connector_account,omitempty"`

	// PM and PMConnectorAccount are the old spellings of the two fields above,
	// from before the PM driver was named the Wrangler (2026-10-05). They are
	// still read, with identical behaviour, and written back as they were
	// found when Rein rewrites the file, so a config that says `pm = true`
	// keeps working here and on the release before this one. Both spellings
	// may be set when they agree; a file where they disagree is refused at
	// load (see [checkSpellings]).
	PM                 bool   `toml:"pm,omitempty"`
	PMConnectorAccount string `toml:"pm_connector_account,omitempty"`

	// Name is the queue name as Elk knows it, at most QueueNameMax bytes.
	Name string `toml:"name"`

	// AgentKind is the coding agent driven for this queue: the key an adapter
	// registers under, e.g. "claude", "codex", "grok".
	AgentKind string `toml:"agent_kind"`

	// Workspace overrides Config.Workspace for this queue. Empty means inherit.
	Workspace string `toml:"workspace"`

	// Repo is this queue's default repository — the third place the run loop
	// looks for one, ahead of [Config.DefaultRepo] and behind whatever the
	// work order itself says.
	Repo string `toml:"repo"`

	// Capabilities are extra environment capability names available to runs on
	// this queue, added to [Config.Capabilities] rather than replacing them.
	Capabilities []string `toml:"capabilities"`

	// BaseRef is the ref runs on this queue are cut from. Empty means resolve
	// the remote default branch, which is what almost every queue wants — see
	// internal/worktree.
	BaseRef string `toml:"base_ref"`

	// PermissionMode is how sessions on this queue authorise mutations: one of
	// read_only, ask, accept_edits, full. Empty means [DefaultPermissionMode].
	//
	// There is no silent default in the adapter contract on purpose —
	// `claude -p` starts in Manual mode on every plan and hangs on its first
	// edit — so the runner resolves it here and prints the mode it is using
	// for each queue at startup. It is never a surprise.
	PermissionMode string `toml:"permission_mode"`

	// WeeklyReset is when this queue's subscription resets its weekly
	// allowance, as the person knows it: a weekday, a time, and optionally an
	// IANA zone — "fri 17:00", "Monday 09:30 America/Los_Angeles". No zone
	// means this machine's local time.
	//
	// Only an agent that cannot say when it will serve again needs it. Grok
	// exposes no window at all, so when a Grok run stops on a rate limit the
	// queue holds until the next occurrence of this time — or 24 hours with
	// none set (ark:rein#40). Claude and Codex state their own reset and do not
	// read it.
	WeeklyReset string `toml:"weekly_reset"`

	// MCPServers is this queue's per-run MCP allow-list: names from the
	// top-level [Config.MCPServers]. Every run on the queue gets exactly these
	// servers — and none of the developer's own, which ark:rein#21 and #22 cut
	// off — and the queue declares each one it can serve as `mcp:<name>`.
	// Empty is the default and means no MCP servers at all.
	MCPServers []string `toml:"mcp_servers"`

	// Land is how far a run on this queue takes its change (ark:rein#47):
	//
	//	merge   push, open a non-draft PR, wait for CI, squash-merge it
	//	pr      push, open a non-draft PR, and stop — the repository's owners merge
	//	branch  push the run branch, and stop — no pull request at all
	//
	// Empty means [DefaultLand], which is merge: elk-work's own house rule,
	// and what every queue did before the setting existed, so a config that
	// never mentions it behaves exactly as it always has. `pr` is for a
	// repository somebody else owns — a customer's — where a runner merging
	// its own work is not the agreement.
	//
	// Rein renders the policy into the run's system prompt and checks the
	// remote after the run (internal/runner/landing.go). Neither is a control:
	// an agent holding a shell can merge whatever it is told. The control is
	// the repository's own branch protection, and a `pr` queue belongs on a
	// repository that requires a review.
	Land string `toml:"land"`

	// Secrets is this queue's scoped secrets map (ark:rein#48): an environment
	// variable NAME to the keychain item that holds its value.
	//
	//	[queues.secrets]
	//	POSTHOG_API_KEY       = "acme-posthog"         # service; account "rein"
	//	AWS_SECRET_ACCESS_KEY = "acme-aws/secret-key"  # service/account
	//
	// A queue with a map is SCOPED. Each run on it gets exactly these
	// variables, read from this machine's keychain at the start of the run and
	// set in the agent process's environment — and so reaching the queue's
	// allowed MCP servers too — plus the system variables any process needs
	// (internal/secretenv). Nothing else the daemon's environment holds is
	// inherited, and the queue does not declare the machine-wide vault
	// (`supabase-vault`) whatever the machine-level capabilities say. A
	// missing item fails the run closed, naming the variable and the item.
	//
	// No value is ever in this file, in a log, in a prompt or in a
	// deliverable: the config holds names, the keychain holds values, and Rein
	// moves a value only from one to the agent process's environment.
	//
	// Empty is the default: the run's environment is scoped by name instead
	// ([Queue.InheritEnv]), and the vault stays declared.
	Secrets map[string]string `toml:"secrets"`

	// ScopedSecrets makes a queue scoped with an EMPTY secrets map: its runs
	// get no credentials from this machine at all. A non-empty [Queue.Secrets]
	// implies it, so it only needs writing for a queue that needs nothing.
	ScopedSecrets bool `toml:"scoped_secrets"`

	// InheritEnv names variables from the daemon's environment this queue's
	// runs keep, added to [Config.InheritEnv]. Not allowed on a queue with a
	// secrets map, whose credentials come from the map alone.
	InheritEnv []string `toml:"inherit_env,omitempty"`

	// Model is the vendor model id every run on this queue uses, passed to
	// the CLI verbatim: `--model` for Claude Code and Grok, the thread's and
	// turn's `model` for Codex. Empty keeps the CLI's own default.
	//
	//	model = "claude-opus-5-5"
	Model string `toml:"model,omitempty"`

	// Effort is the reasoning-effort level every run on this queue uses,
	// passed verbatim: `--effort` for Claude Code (low, medium, high, xhigh,
	// max), the turn's `effort` for Codex (minimal, low, medium, high, xhigh),
	// `--reasoning-effort` for Grok (low, medium, high, xhigh). Rein checks
	// only its shape, because each vendor adds levels on its own schedule;
	// the CLI refuses a level it does not know, and the run reports why.
	// Empty keeps the CLI's own default.
	Effort string `toml:"effort,omitempty"`
}

// modelRE is the shape of a vendor model id: `claude-opus-5-5`,
// `claude-sonnet-5-5[1m]`, `gpt-5.5-codex`, `grok-4.7`, `openai/gpt-oss`.
var modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,127}$`)

// effortRE is the shape of an effort level: one lower-case word.
var effortRE = regexp.MustCompile(`^[a-z][a-z_-]{0,31}$`)

// meteredWhy is the reason given whenever config names a metered variable.
const meteredWhy = "would move the agent off the developer's plan login onto metered API billing; Rein refuses it on every queue"

// validateInheritEnv checks one inherit_env list.
func validateInheritEnv(where string, names []string) error {
	for _, name := range names {
		switch {
		case !envNameRE.MatchString(name):
			return fmt.Errorf("%s: inherit_env: %q is not an environment variable name", where, name)
		case secretenv.Metered(name):
			return fmt.Errorf("%s: inherit_env: %s %s", where, name, meteredWhy)
		}
	}
	return nil
}

// DefaultSecretAccount is the keychain account a [Queue.Secrets] item is read
// from when the config names only its service.
const DefaultSecretAccount = "rein"

// SecretRef is one parsed [Queue.Secrets] entry.
type SecretRef struct {
	// Env is the variable the run sees.
	Env string
	// Service and Account address the keychain item holding the value.
	Service string
	Account string
}

// Item describes the keychain item for a message — never its value.
func (r SecretRef) Item() string {
	return fmt.Sprintf("service %q, account %q", r.Service, r.Account)
}

// ParseSecretItem reads a [Queue.Secrets] value: "<service>" or
// "<service>/<account>", split at the first slash so an account may contain
// one. A service with a slash in it cannot be named; that is the trade for a
// one-string form.
func ParseSecretItem(s string) (service, account string, err error) {
	s = strings.TrimSpace(s)
	service, account, hasAccount := strings.Cut(s, "/")
	switch {
	case service == "":
		return "", "", fmt.Errorf("%q names no keychain service (want \"<service>\" or \"<service>/<account>\")", s)
	case hasAccount && account == "":
		return "", "", fmt.Errorf("%q has a slash but no account after it", s)
	case !hasAccount:
		account = DefaultSecretAccount
	}
	if reservedSecretServices[service] {
		return "", "", fmt.Errorf("%q: service %q holds Rein's own credentials and cannot be handed to a run", s, service)
	}
	return service, account, nil
}

// reservedSecretServices mirrors internal/keyring's reserved services — Rein's
// own per-queue Elk tokens and the Wrangler's owner connector — duplicated so config
// stays free of the keyring dependency. The keyring refuses them as well.
var reservedSecretServices = map[string]bool{"rein": true, "elk-connector-url": true}

// Scoped reports whether runs on this queue get only their own secrets.
func (q Queue) Scoped() bool { return q.ScopedSecrets || len(q.Secrets) > 0 }

// IsWrangler reports whether this queue runs the Wrangler, under either
// spelling: `wrangler = true`, or the old `pm = true`.
func (q Queue) IsWrangler() bool { return q.Wrangler || q.PM }

// WranglerAccount is the keychain account holding this queue's owner
// connector, under either spelling. Empty means the <workspace>/<queue>
// default. [Config.Validate] refuses two different accounts, so whichever is
// set is the one.
func (q Queue) WranglerAccount() string {
	if q.WranglerConnectorAccount != "" {
		return q.WranglerConnectorAccount
	}
	return q.PMConnectorAccount
}

// wranglerKey is the opt-in as this queue's file spells it, so an error
// quotes back the line its reader actually wrote.
func (q Queue) wranglerKey() string {
	if q.PM && !q.Wrangler {
		return "pm = true"
	}
	return "wrangler = true"
}

// wranglerAccountKey is the connector-account key as this queue spells it.
func (q Queue) wranglerAccountKey() string {
	if q.PMConnectorAccount != "" && q.WranglerConnectorAccount == "" {
		return "pm_connector_account"
	}
	return "wrangler_connector_account"
}

// reservedCapability reports whether a name may not appear in the
// machine-wide `capabilities` list. `pm` is the Wrangler's routing capability
// — still its name on the wire, where Elk's api_set_pm_executor checks for it
// and declared_capabilities persists it — and only the per-queue opt-in may
// declare it. `wrangler` is not a capability at all; listing it is almost
// certainly an attempt at the opt-in, and it is refused anywhere so that it
// can never become one.
func reservedCapability(name string) bool { return name == "pm" || name == "wrangler" }

// SecretRefs returns the queue's secrets map parsed and sorted by variable
// name. An entry that does not parse is left out; [Config.Validate] refuses
// such a config at load, so a loaded config has none.
func (q Queue) SecretRefs() []SecretRef {
	out := make([]SecretRef, 0, len(q.Secrets))
	for name, item := range q.Secrets {
		service, account, err := ParseSecretItem(item)
		if err != nil {
			continue
		}
		out = append(out, SecretRef{Env: name, Service: service, Account: account})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Env < out[j].Env })
	return out
}

// envNameRE is what a secrets map may name: a portable environment variable.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// credentialNameRE is the shape of a capability that names an environment
// variable rather than a tool — SUPABASE_SERVICE_ROLE_KEY, not docker.
var credentialNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// LooksLikeEnvName reports whether a capability name is an environment
// variable name rather than a tool, by the convention capabilities follow:
// variables are upper case, tools are not.
func LooksLikeEnvName(name string) bool { return credentialNameRE.MatchString(name) }

// validateSecrets reports what is wrong with one queue's secrets map.
func (q Queue) validateSecrets(i int) error {
	where := fmt.Sprintf("queues[%d] (%s)", i, q.Name)
	if !q.Scoped() {
		return nil
	}
	if len(q.InheritEnv) > 0 {
		return fmt.Errorf("%s: inherit_env is for a queue without a secrets map; a scoped queue's runs get only their map", where)
	}
	names := make([]string, 0, len(q.Secrets))
	for name := range q.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch {
		case !envNameRE.MatchString(name):
			return fmt.Errorf("%s: secrets: %q is not an environment variable name (letters, digits and _, not starting with a digit)", where, name)
		case secretenv.Inherited(name):
			return fmt.Errorf("%s: secrets: %s is a system variable every run already inherits; it cannot come from the keychain", where, name)
		case strings.HasPrefix(strings.ToUpper(name), "REIN_"):
			return fmt.Errorf("%s: secrets: %s: the REIN_ prefix is Rein's own", where, name)
		case secretenv.Metered(name):
			// Every adapter refuses it at Start; saying so at load is kinder
			// than a stuck run.
			return fmt.Errorf("%s: secrets: %s %s", where, name, meteredWhy)
		}
		if _, _, err := ParseSecretItem(q.Secrets[name]); err != nil {
			return fmt.Errorf("%s: secrets.%s: %w", where, name, err)
		}
	}
	for _, c := range q.Capabilities {
		switch {
		case c == "supabase-vault":
			return fmt.Errorf("%s: a scoped queue cannot declare supabase-vault — its runs get only their secrets map, not the machine's vault", where)
		case LooksLikeEnvName(c):
			if _, ok := q.Secrets[c]; !ok {
				return fmt.Errorf("%s: capabilities names %s, but a scoped queue's credentials come only from its secrets map; add it there", where, c)
			}
		}
	}
	return nil
}

// The three landing policies a queue may declare in [Queue.Land].
const (
	LandMerge  = "merge"
	LandPR     = "pr"
	LandBranch = "branch"
)

// DefaultLand is the policy of a queue that does not say: merge, the house
// rule for elk-work repositories and the behaviour that predates the setting.
const DefaultLand = LandMerge

// LandOrDefault returns the queue's landing policy, or [DefaultLand].
func (q Queue) LandOrDefault() string {
	if q.Land != "" {
		return q.Land
	}
	return DefaultLand
}

func knownLand(l string) bool {
	return l == LandMerge || l == LandPR || l == LandBranch
}

// WeeklyReset is a parsed [Queue.WeeklyReset]: a weekday and a time of day
// in a zone.
type WeeklyReset struct {
	Weekday time.Weekday
	Hour    int
	Minute  int
	Loc     *time.Location
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

// ParseWeeklyReset reads "<weekday> <HH:MM> [zone]". An empty string is no
// reset stated, and returns ok false with no error.
func ParseWeeklyReset(s string) (r WeeklyReset, ok bool, err error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return WeeklyReset{}, false, nil
	}
	bad := func(why string) (WeeklyReset, bool, error) {
		return WeeklyReset{}, false, fmt.Errorf("weekly_reset %q: %s (want something like \"fri 17:00\" or \"Monday 09:30 America/Los_Angeles\")", s, why)
	}
	if len(fields) < 2 || len(fields) > 3 {
		return bad("want a weekday, a time and optionally a zone")
	}
	wd, known := weekdays[strings.ToLower(fields[0])]
	if !known {
		return bad(fmt.Sprintf("%q is not a weekday", fields[0]))
	}
	clock, err := time.Parse("15:04", fields[1])
	if err != nil {
		return bad(fmt.Sprintf("%q is not a 24-hour HH:MM time", fields[1]))
	}
	loc := time.Local
	if len(fields) == 3 {
		if loc, err = time.LoadLocation(fields[2]); err != nil {
			return bad(fmt.Sprintf("%q is not a time zone this machine knows", fields[2]))
		}
	}
	return WeeklyReset{Weekday: wd, Hour: clock.Hour(), Minute: clock.Minute(), Loc: loc}, true, nil
}

// Next is the first occurrence of the reset strictly after t.
func (r WeeklyReset) Next(t time.Time) time.Time {
	loc := r.Loc
	if loc == nil {
		loc = time.Local
	}
	local := t.In(loc)
	days := (int(r.Weekday) - int(local.Weekday()) + 7) % 7
	next := time.Date(local.Year(), local.Month(), local.Day()+days, r.Hour, r.Minute, 0, 0, loc)
	if !next.After(t) {
		next = next.AddDate(0, 0, 7)
	}
	return next
}

// DefaultPermissionMode is what a queue uses when it does not say.
//
// `full` — everything auto-approved — is the only mode a v0 runner can
// actually finish work in: `ask` and `accept_edits` both need an adapter that
// declares `approvals`, and a daemon with nobody at the keyboard has nothing
// to answer with. It is defensible for exactly the reason elk docs/rein.md §3
// gives: a run happens in a git worktree of a repository we own, cut by
// internal/worktree, and nowhere else. Narrow a queue with `permission_mode`
// when that is not true of it.
const DefaultPermissionMode = "full"

// DefaultLogKeepRuns is how many runs' event logs are kept when the config
// does not say. Fifty is roughly a fortnight for a single busy machine and a
// few megabytes on disk; the point of the bound is that nobody ever has to
// prune these by hand.
const DefaultLogKeepRuns = 50

// KeepRuns returns how many run logs to keep: the configured number, the
// default when it is zero, and 0 — meaning "keep everything" — when it is
// negative.
func (c Config) KeepRuns() int {
	switch {
	case c.LogKeepRuns > 0:
		return c.LogKeepRuns
	case c.LogKeepRuns < 0:
		return 0
	default:
		return DefaultLogKeepRuns
	}
}

// PermissionMode returns the queue's mode, or [DefaultPermissionMode].
func (q Queue) PermissionModeOrDefault() string {
	if q.PermissionMode != "" {
		return q.PermissionMode
	}
	return DefaultPermissionMode
}

// ErrNotExist reports that no config file exists yet. [Load] returns it wrapped
// so callers can tell "not enrolled" from "config is broken".
var ErrNotExist = errors.New("config: no config file")

// nameRE constrains queue names to what reads well in a queue listing and
// survives a URL, a shell word and a keychain key unquoted.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// hostIDRE is the same shape, minus the hyphen, because "<host>-<agent>" splits
// on it.
var hostIDRE = regexp.MustCompile(`^[a-z0-9]+$`)

// Dir returns the Rein home directory, creating nothing. See the package doc
// for the resolution order.
func Dir() (string, error) {
	if h := strings.TrimSpace(os.Getenv(EnvHome)); h != "" {
		return h, nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".rein"), nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: no home or config directory: %w", err)
	}
	return filepath.Join(cfg, "rein"), nil
}

// Path returns the full path of the config file.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// DefaultHostID suggests a host_id from the machine's hostname: the first label,
// lowercased, stripped of anything outside [a-z0-9], and cut to three
// characters — "Sams-MacBook-Pro.local" becomes "sam". It is a suggestion for
// `rein enrol` to offer, never applied silently, because the host id is half of
// a durable identity in Elk and a machine that guesses it twice differently
// splits its own history.
func DefaultHostID() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(h, ".")
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 3 {
		s = s[:3]
	}
	return s
}

// NewMachineID mints a fresh [Config.MachineID]: 32 hex characters from
// crypto/rand, well inside Elk's 128-character cap on `host_id`.
//
// It is called once, by `rein enrol`, and the result is written to the config.
// Nothing else calls it: a machine that minted a second id would appear to Elk
// as a second machine, which is the failure the durable identity exists to
// prevent.
func NewMachineID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("config: mint machine id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Default returns a Config with every defaultable field filled in: the host id
// suggested from the hostname and the work directory under the Rein home. It
// carries no queues, no machine id and no Elk URL — those come from
// `rein enrol`.
func Default() Config {
	c := Config{HostID: DefaultHostID()}
	if dir, err := Dir(); err == nil {
		c.WorkDir = filepath.Join(dir, "work")
	}
	return c
}

// Load reads the config from [Path]. A missing file returns a wrapped
// [ErrNotExist] along with [Default], so a caller that only wants defaults can
// ignore that one error.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	return LoadFile(path)
}

// LoadFile reads the config from an explicit path. Defaults are applied to
// fields the file leaves empty, and the result is validated.
func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), fmt.Errorf("%w at %s", ErrNotExist, path)
	}
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	var c Config
	if _, err := toml.Decode(string(data), &c); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := checkSpellings(string(data)); err != nil {
		return c, fmt.Errorf("config: %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("config: %s: %w", path, err)
	}
	return c, nil
}

// checkSpellings refuses a queue whose file sets both `wrangler` and its old
// spelling `pm` to different values.
//
// It reads the file a second time, into pointers, because a plain bool cannot
// tell `pm = false` written down from `pm` left out — and the case that
// matters is someone turning the Wrangler off with `wrangler = false` beside a
// `pm = true` they forgot. Reading the two as "either one" would leave
// production authority on; reading either one as winning is a guess about
// which line the person meant. So neither: the file is refused and the error
// says which line to delete.
func checkSpellings(data string) error {
	var file struct {
		Queues []struct {
			Name     string `toml:"name"`
			Wrangler *bool  `toml:"wrangler"`
			PM       *bool  `toml:"pm"`
		} `toml:"queues"`
	}
	if _, err := toml.Decode(data, &file); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	for i, q := range file.Queues {
		if q.Wrangler != nil && q.PM != nil && *q.Wrangler != *q.PM {
			return fmt.Errorf("queues[%d] (%s): wrangler = %t and pm = %t disagree; pm is the old spelling of wrangler, so delete the pm line and set wrangler to what you mean",
				i, q.Name, *q.Wrangler, *q.PM)
		}
	}
	return nil
}

// applyDefaults fills empty fields from [Default]. It never overwrites a value
// the file set.
func (c *Config) applyDefaults() {
	d := Default()
	if c.HostID == "" {
		c.HostID = d.HostID
	}
	if c.WorkDir == "" {
		c.WorkDir = d.WorkDir
	}
}

// Save writes the config to [Path], creating the Rein home if needed. The file
// is written 0600 — it holds no secrets today, but it names workspaces and
// endpoints, and a future field should not have to remember to tighten it.
func (c Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	return c.SaveFile(path)
}

// SaveFile writes the config to an explicit path.
func (c Config) SaveFile(path string) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("config: refusing to write invalid config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: create %s: %w", filepath.Dir(path), err)
	}
	var b strings.Builder
	b.WriteString("# Rein — the Elk runner. Generated by `rein enrol`; safe to edit.\n")
	b.WriteString("# Tokens are NOT here: they live in the OS keychain, keyed by workspace\n")
	b.WriteString("# and queue. See `rein status`.\n\n")
	enc := toml.NewEncoder(&b)
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

// Validate reports the first problem that would make this config unusable.
// An empty config is valid — a machine that has not enrolled yet has nothing to
// be wrong about — but any queue present must be complete and well formed.
func (c Config) Validate() error {
	if c.HostID != "" && !hostIDRE.MatchString(c.HostID) {
		return fmt.Errorf("host_id %q: must be lowercase letters and digits, no hyphen", c.HostID)
	}
	// A negative interval is a typo, not an intention, and a runner that
	// clamped it silently would leave the typo in the file for the next
	// person. Zero means the default; the runner clamps a too-small positive
	// one up to its own floor rather than refusing a config over it.
	if c.Telemetry.Interval < 0 {
		return fmt.Errorf("telemetry.interval %s: must not be negative", c.Telemetry.Interval.Duration())
	}
	for _, name := range c.Capabilities {
		if reservedCapability(name) {
			return fmt.Errorf("capabilities: %s cannot be listed; enable the Wrangler per queue with wrangler = true", name)
		}
		if secretenv.Metered(name) {
			return fmt.Errorf("capabilities: %s %s", name, meteredWhy)
		}
	}
	if err := validateInheritEnv("config", c.InheritEnv); err != nil {
		return err
	}
	for name, s := range c.MCPServers {
		if err := s.validate(name); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(c.Queues))
	for i, q := range c.Queues {
		switch {
		case q.IsWrangler() && q.AgentKind != "claude" && q.AgentKind != "codex":
			return fmt.Errorf("queues[%d]: %s requires agent_kind = claude or codex", i, q.wranglerKey())
		case q.IsWrangler() && len(q.MCPServers) > 0:
			return fmt.Errorf("queues[%d]: Wrangler queues get only their owner Elk connector; remove mcp_servers", i)
		case q.WranglerConnectorAccount != "" && q.PMConnectorAccount != "" && q.WranglerConnectorAccount != q.PMConnectorAccount:
			return fmt.Errorf("queues[%d]: wrangler_connector_account and pm_connector_account name different keychain accounts; pm_connector_account is the old spelling, so delete it and set wrangler_connector_account to the one you mean", i)
		case !q.IsWrangler() && q.WranglerAccount() != "":
			return fmt.Errorf("queues[%d]: %s requires wrangler = true", i, q.wranglerAccountKey())
		case q.Name == "":
			return fmt.Errorf("queues[%d]: name is required", i)
		case len(q.Name) > QueueNameMax:
			return fmt.Errorf("queues[%d]: name %q is %d bytes, over the %d-byte cap Elk enforces",
				i, q.Name, len(q.Name), QueueNameMax)
		case !nameRE.MatchString(q.Name):
			return fmt.Errorf("queues[%d]: name %q: must be lowercase letters, digits and hyphens, starting with a letter or digit", i, q.Name)
		case q.AgentKind == "":
			return fmt.Errorf("queues[%d] (%s): agent_kind is required", i, q.Name)
		case seen[q.Name]:
			return fmt.Errorf("queues[%d]: name %q appears twice", i, q.Name)
		case q.PermissionMode != "" && !knownPermissionMode(q.PermissionMode):
			return fmt.Errorf("queues[%d] (%s): permission_mode %q: want read_only, ask, accept_edits or full",
				i, q.Name, q.PermissionMode)
		case q.Land != "" && !knownLand(q.Land):
			// Refused rather than read as the default: a typo of `pr` that
			// quietly became `merge` would merge into a repository whose
			// owners asked for a pull request, which is the one outcome the
			// setting exists to prevent.
			return fmt.Errorf("queues[%d] (%s): land %q: want merge, pr or branch", i, q.Name, q.Land)
		case q.Model != "" && !modelRE.MatchString(q.Model):
			return fmt.Errorf("queues[%d] (%s): model %q is not a model id", i, q.Name, q.Model)
		case q.Effort != "" && !effortRE.MatchString(q.Effort):
			return fmt.Errorf("queues[%d] (%s): effort %q: want one lower-case word such as low, medium or high", i, q.Name, q.Effort)
		}
		if err := validateInheritEnv(fmt.Sprintf("queues[%d] (%s)", i, q.Name), q.InheritEnv); err != nil {
			return err
		}
		for _, name := range q.Capabilities {
			switch {
			case name == "wrangler":
				return fmt.Errorf("queues[%d]: wrangler is not a capability; enable the Wrangler with wrangler = true", i)
			case name == "pm" && !q.IsWrangler():
				return fmt.Errorf("queues[%d]: capability pm requires wrangler = true", i)
			case secretenv.Metered(name):
				return fmt.Errorf("queues[%d] (%s): capabilities: %s %s", i, q.Name, name, meteredWhy)
			}
		}
		if _, _, err := ParseWeeklyReset(q.WeeklyReset); err != nil {
			return fmt.Errorf("queues[%d] (%s): %w", i, q.Name, err)
		}
		for _, name := range q.MCPServers {
			if _, ok := c.MCPServers[name]; !ok {
				return fmt.Errorf("queues[%d] (%s): mcp_servers names %q, which has no [mcp_servers.%s] definition",
					i, q.Name, name, name)
			}
		}
		if err := q.validateSecrets(i); err != nil {
			return err
		}
		seen[q.Name] = true
	}
	return nil
}

// permissionModes mirrors internal/adapter's PermissionMode values. It is
// duplicated rather than imported so that config stays a leaf package; the
// adapter contract is the authority and a divergence is a bug here.
var permissionModes = []string{"read_only", "ask", "accept_edits", "full"}

func knownPermissionMode(m string) bool {
	for _, v := range permissionModes {
		if v == m {
			return true
		}
	}
	return false
}

// Queue returns the queue with the given name.
func (c Config) Queue(name string) (Queue, bool) {
	for _, q := range c.Queues {
		if q.Name == name {
			return q, true
		}
	}
	return Queue{}, false
}

// SetQueue adds a queue, or replaces the one with the same name. It reports
// whether an existing queue was replaced, so `rein enrol` can say "attached"
// rather than "created" without a second lookup.
func (c *Config) SetQueue(q Queue) (replaced bool) {
	for i := range c.Queues {
		if c.Queues[i].Name == q.Name {
			c.Queues[i] = q
			return true
		}
	}
	c.Queues = append(c.Queues, q)
	return false
}

// RemoveQueue drops a queue by name, reporting whether one was there.
//
// The caller must delete that queue's keychain entry BEFORE calling this. The
// OS keychain APIs Rein uses have no enumeration, so the config is the only
// index of what tokens exist: a queue removed from the config first leaves its
// token in the keychain with nothing left that knows to ask for it.
func (c *Config) RemoveQueue(name string) bool {
	for i, q := range c.Queues {
		if q.Name == name {
			c.Queues = append(c.Queues[:i], c.Queues[i+1:]...)
			return true
		}
	}
	return false
}

// WorkspaceFor returns the workspace a queue belongs to, falling back to the
// config-level default.
func (c Config) WorkspaceFor(q Queue) string {
	if q.Workspace != "" {
		return q.Workspace
	}
	return c.Workspace
}

// SuggestQueueName builds the conventional queue name for an agent kind on this
// host — "mac-claude", "win-codex". It reports false when the result would
// exceed [QueueNameMax] rather than truncating, per ruling 3.
func (c Config) SuggestQueueName(agentKind string) (string, bool) {
	if c.HostID == "" || agentKind == "" {
		return "", false
	}
	name := c.HostID + "-" + agentKind
	if len(name) > QueueNameMax {
		return "", false
	}
	return name, true
}
