// Package secretenv is the run-side half of a queue's scoped secrets
// (ark:rein#48): what a scoped run inherits from the daemon's environment, and
// the redactor that keeps the values Rein handed it out of everything Rein
// writes down.
//
// A queue that declares a secrets map in config.toml is SCOPED. Its runs get
// exactly the variables the map names, read from this machine's keychain at
// the start of each run, plus the small set of system variables a process
// needs to run at all ([Inherited]) — and nothing else the daemon holds. The
// shell environment of a developer who started `rein run` by hand, a
// credential somebody put in the service's environment, and the machine-wide
// vault broker's declaration are all left behind.
//
// A queue with no map is scoped too, more loosely: its runs get the system
// variables plus the ones the queue declares it needs by name — inherit_env,
// a credential-shaped capability, an allowed MCP server's key — and nothing
// else the daemon holds. Until the plan-login change they inherited the
// daemon's whole environment, which is how an API key exported in the shell
// that started `rein run` could silently move a run onto metered billing.
// [Metered] variables are never inherited by any run.
//
// It is a leaf package — no other internal import — so config can validate a
// map against [Inherited] without depending on the adapter contract, and the
// adapters, the run loop and the run log can all share one redactor.
package secretenv

import (
	"bytes"
	"encoding/json"
	"runtime"
	"sort"
	"strings"
)

// inherited is every variable a scoped run takes from the daemon's
// environment. It is the plumbing a process needs to find its binaries, its
// home, its temp directory, its locale, its certificates and its network — and
// deliberately nothing that is a credential in its own right.
//
// SSH_AUTH_SOCK is here because pushing the run's branch is the run's job: it
// is the machine's own git identity, in the same class as `gh`'s login, which a
// run also keeps. The proxy variables are the machine's network configuration;
// without them a vendor binary behind a proxy cannot reach its own API at all.
var inherited = map[string]bool{
	// Every platform.
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"TMPDIR": true, "TMP": true, "TEMP": true, "TERM": true, "TZ": true,
	"LANG": true, "LANGUAGE": true,
	"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_CACHE_HOME": true,
	"XDG_STATE_HOME": true, "XDG_RUNTIME_DIR": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
	"SSH_AUTH_SOCK": true,
	// Toolchain locations: where a compiler, an SDK or a package cache lives.
	// Configuration, never a credential, and a build that loses them fails in
	// ways that look nothing like a missing variable.
	"DEVELOPER_DIR": true, "JAVA_HOME": true, "ANDROID_HOME": true,
	"ANDROID_SDK_ROOT": true, "GOPATH": true, "GOROOT": true, "GOCACHE": true,
	"GOMODCACHE": true, "CARGO_HOME": true, "RUSTUP_HOME": true, "NVM_DIR": true,
	"PNPM_HOME": true, "PYENV_ROOT": true, "DOTNET_ROOT": true,
	// macOS.
	"__CF_USER_TEXT_ENCODING": true,
	// Windows. Compared without case there, as Windows does.
	"SYSTEMROOT": true, "SYSTEMDRIVE": true, "WINDIR": true, "COMSPEC": true,
	"PATHEXT": true, "USERPROFILE": true, "USERNAME": true, "USERDOMAIN": true,
	"APPDATA": true, "LOCALAPPDATA": true, "PROGRAMDATA": true,
	"PROGRAMFILES": true, "PROGRAMFILES(X86)": true, "PROGRAMW6432": true,
	"COMMONPROGRAMFILES": true, "COMMONPROGRAMFILES(X86)": true,
	"HOMEDRIVE": true, "HOMEPATH": true, "COMPUTERNAME": true, "OS": true,
	"PROCESSOR_ARCHITECTURE": true, "NUMBER_OF_PROCESSORS": true,
}

// Inherited reports whether a scoped run takes this variable from the daemon's
// environment. The locale's LC_* family is matched by prefix.
func Inherited(name string) bool {
	if strings.HasPrefix(name, "LC_") {
		return true
	}
	if inherited[name] {
		return true
	}
	if runtime.GOOS == "windows" {
		return inherited[strings.ToUpper(name)]
	}
	return false
}

// metered are the variables that move a coding agent off the developer's plan
// login and onto metered API billing. Each one
// either is an API credential the vendor CLI prefers over the subscription
// login when it is present — `claude -p` always uses ANTHROPIC_API_KEY when it
// is set — or switches the CLI to a cloud provider's billing outright.
//
// A run never inherits one, whatever the queue says; config refuses a queue
// that names one; and every adapter refuses a RunSpec.Env that carries one.
var metered = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"OPENAI_API_KEY",
	"CODEX_API_KEY",
	"XAI_API_KEY",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
}

// Metered reports whether name is a variable that would move an agent off the
// plan login onto metered billing. See [MeteredNames].
func Metered(name string) bool {
	for _, m := range metered {
		if name == m || runtime.GOOS == "windows" && strings.EqualFold(name, m) {
			return true
		}
	}
	return false
}

// MeteredNames is every variable [Metered] matches, for messages and docs.
func MeteredNames() []string { return append([]string(nil), metered...) }

// MeteredIn returns the names in env that [Metered] matches, sorted.
func MeteredIn(env map[string]string) []string {
	var out []string
	for name := range env {
		if Metered(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Filter returns the entries of a KEY=VALUE environment a run inherits: the
// system variables ([Inherited]) plus any named in pass, which is how a queue
// keeps a variable it declared (inherit_env, a credential-shaped capability, an
// MCP server's key). A [Metered] variable is dropped even when pass names it.
// Everything else is dropped, including entries with no `=` at all.
func Filter(environ []string, pass ...string) []string {
	keep := make(map[string]bool, len(pass))
	for _, name := range pass {
		keep[name] = true
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		i := strings.IndexByte(kv, '=')
		// i == 0 is Windows' "=C:=C:\dir" drive-cwd entries: not a variable
		// anything is configured with, and not something a run needs.
		if i <= 0 {
			continue
		}
		name := kv[:i]
		if Metered(name) {
			continue
		}
		if Inherited(name) || keep[name] {
			out = append(out, kv)
		}
	}
	return out
}

// MinValueLen is the shortest secret value Rein will hand a run.
//
// Redaction is a literal match, so a short value would be replaced everywhere
// it happens to occur in ordinary text — and a value too short to redact
// without mangling every deliverable is too short to be kept out of them.
// Every real credential is far longer; a value this short in a secrets map is
// a mistake (a region, a username) and is refused rather than redacted badly.
const MinValueLen = 8

// Redactor replaces the values of a run's secrets with a marker naming the
// variable, in anything Rein is about to write down: the run log, the daemon's
// log, a progress report, a deliverable.
//
// It is a backstop, not the guarantee. The guarantee is that Rein never puts a
// value anywhere itself — names only, in the prompt and in every message about
// a missing item. What the redactor catches is the agent's own output, which
// Rein relays: an agent that echoes a variable it was given has its echo
// replaced before the line reaches disk or Elk. It matches literally, so a
// value the agent transforms (encodes, splits, reverses) is not caught; the
// system prompt tells it not to and the docs say so.
//
// A nil *Redactor is a working no-op, so call sites need no guard.
type Redactor struct {
	pairs []pair
}

type pair struct {
	value  string
	marker string
}

// NewRedactor builds a redactor over name → value. Empty values are ignored.
// Longer values are replaced first, so a value that contains another is
// replaced whole rather than leaving its remainder behind.
func NewRedactor(values map[string]string) *Redactor {
	r := &Redactor{}
	seen := map[string]bool{}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	add := func(v, marker string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		r.pairs = append(r.pairs, pair{value: v, marker: marker})
	}
	for _, name := range names {
		v := values[name]
		marker := Marker(name)
		add(v, marker)
		// The same value as it appears inside a JSON string, which is how a
		// run log line and a vendor's raw event carry it. Identical for the
		// usual credential alphabet; different for a value holding a quote,
		// a backslash, a control character or <, > and &.
		if b, err := json.Marshal(v); err == nil && len(b) >= 2 {
			add(string(b[1:len(b)-1]), marker)
		}
	}
	sort.SliceStable(r.pairs, func(i, j int) bool { return len(r.pairs[i].value) > len(r.pairs[j].value) })
	return r
}

// Marker is what a value is replaced with: the variable's NAME, which is not a
// secret and tells a reader exactly which credential leaked into the text.
func Marker(name string) string { return "[redacted:" + name + "]" }

// Empty reports whether the redactor has nothing to replace.
func (r *Redactor) Empty() bool { return r == nil || len(r.pairs) == 0 }

// String redacts s.
func (r *Redactor) String(s string) string {
	if r.Empty() || s == "" {
		return s
	}
	for _, p := range r.pairs {
		if strings.Contains(s, p.value) {
			s = strings.ReplaceAll(s, p.value, p.marker)
		}
	}
	return s
}

// Bytes redacts b, returning b itself when there is nothing to replace.
func (r *Redactor) Bytes(b []byte) []byte {
	if r.Empty() || len(b) == 0 {
		return b
	}
	for _, p := range r.pairs {
		v := []byte(p.value)
		if bytes.Contains(b, v) {
			b = bytes.ReplaceAll(b, v, []byte(p.marker))
		}
	}
	return b
}
