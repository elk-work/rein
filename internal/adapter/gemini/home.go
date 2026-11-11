package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
)

// Each run's private Gemini CLI home.
//
// Gemini CLI keeps everything it reads about who it is — settings, the cached
// Google sign-in, MCP servers, extensions, GEMINI.md, trusted folders — under
// $GEMINI_CLI_HOME/.gemini, falling back to the real home when the variable is
// unset. Pointing it at a directory of the run's own does three things:
//
//   - The session cannot reach the person's Google sign-in, their own MCP
//     servers (the Elk connector Elk's installer registers at user scope
//     among them — capability as the person, the ark:rein#21 incident), or
//     their extensions.
//   - Its settings enforce the API-key sign-in: `security.auth.selectedType`
//     and `enforcedType` are both `gemini-api-key`, so with no key Gemini CLI
//     stops ("you must specify the GEMINI_API_KEY environment variable")
//     instead of signing in.
//   - It is where the run's own MCP servers are written, as Gemini CLI reads
//     them nowhere else Rein can reach.
//
// HOME itself is left alone, so git, gh and SSH inside the run are the
// person's own as with every other adapter. The home is removed when the
// session ends; [Adapter.ReapRun] removes any a crash left behind.

type home struct {
	dir string // GEMINI_CLI_HOME
}

// newHome writes a run's private home.
func (a *Adapter) newHome(spec adapter.RunSpec) (*home, error) {
	root, err := a.root()
	if err != nil {
		return nil, err
	}
	runs := filepath.Join(root, "runs")
	if err := os.MkdirAll(runs, 0o700); err != nil {
		return nil, fmt.Errorf("creating the private Gemini CLI home: %w", err)
	}
	dir, err := os.MkdirTemp(runs, safeName(spec.RunID)+"-")
	if err != nil {
		return nil, fmt.Errorf("creating the private Gemini CLI home: %w", err)
	}
	h := &home{dir: dir}
	blob, err := settingsFor(spec)
	if err != nil {
		h.remove()
		return nil, err
	}
	conf := filepath.Join(dir, ".gemini")
	if err := os.MkdirAll(conf, 0o700); err != nil {
		h.remove()
		return nil, fmt.Errorf("creating the private Gemini CLI home: %w", err)
	}
	if err := os.WriteFile(filepath.Join(conf, "settings.json"), blob, 0o600); err != nil {
		h.remove()
		return nil, fmt.Errorf("writing the private Gemini CLI settings: %w", err)
	}
	return h, nil
}

// settingsFor is the run's settings.json.
func settingsFor(spec adapter.RunSpec) ([]byte, error) {
	settings := map[string]any{
		"security": map[string]any{
			"auth": map[string]any{"selectedType": apiKeyAuth, "enforcedType": apiKeyAuth},
		},
		// A run must not update the program under the person, nor nag.
		"general": map[string]any{"enableAutoUpdate": false, "enableAutoUpdateNotification": false},
	}
	if len(spec.MCPServers) > 0 {
		servers := map[string]any{}
		for name, v := range spec.MCPServers {
			servers[name] = v
		}
		settings["mcpServers"] = servers
	}
	blob, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the private Gemini CLI settings: %w", err)
	}
	return append(blob, '\n'), nil
}

// apiKeyAuth is Gemini CLI's name for the Gemini API key sign-in.
const apiKeyAuth = "gemini-api-key"

func (h *home) remove() {
	if h != nil && h.dir != "" {
		_ = os.RemoveAll(h.dir)
	}
}

// stripped are variables the child never inherits, whatever a queue says:
// Gemini CLI's own switches and Google's, any of which could move the session
// onto a Google sign-in or Google Cloud billing, relocate its settings or
// trust, or send the key to another host (GOOGLE_GEMINI_BASE_URL).
var strippedPrefixes = []string{"GEMINI_", "GOOGLE_GEMINI_", "GOOGLE_GENAI_", "GOOGLE_CLOUD_", "GOOGLE_API_KEY",
	"GOOGLE_APPLICATION_CREDENTIALS", "CLOUD_SHELL"}

func strippedName(name string) bool {
	up := strings.ToUpper(name) // Windows variable names ignore case
	for _, p := range strippedPrefixes {
		if strings.HasPrefix(up, p) {
			return true
		}
	}
	return false
}

// childEnv is the session's environment: base (the scoped inheritance), the
// spec's own variables, then the private home, trust and the key, which
// nothing before them can override.
func (h *home) childEnv(base []string, over map[string]string, key string) []string {
	out := make([]string, 0, len(base)+len(over)+3)
	for _, kv := range base {
		i := strings.IndexByte(kv, '=')
		if i <= 0 || strippedName(kv[:i]) {
			continue
		}
		if _, shadowed := over[kv[:i]]; shadowed {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range over {
		if !strippedName(k) {
			out = append(out, k+"="+v)
		}
	}
	return append(out,
		"GEMINI_CLI_HOME="+h.dir,
		// Documented alongside --skip-trust; both, so a build that drops one
		// still trusts the worktree.
		"GEMINI_CLI_TRUST_WORKSPACE=true",
		KeyName+"="+key)
}

// checkWorkspaceAuth refuses a worktree whose checked-in Gemini settings
// choose a sign-in. Gemini CLI layers the workspace's .gemini/settings.json
// over the user's, so a repository that sets `security.auth` to anything but
// the API key would move the run onto a Google sign-in — exactly what Rein
// must never do. A file Rein cannot read is refused too: failing closed is the
// point.
func checkWorkspaceAuth(worktree string) error {
	path := filepath.Join(worktree, ".gemini", "settings.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return refusal("its .gemini/settings.json could not be read: " + err.Error())
	}
	var s struct {
		Security struct {
			Auth map[string]any `json:"auth"`
		} `json:"security"`
	}
	if err := json.Unmarshal(stripJSONComments(raw), &s); err != nil {
		return refusal("its .gemini/settings.json is not JSON Rein can read, so its sign-in cannot be checked")
	}
	for _, k := range []string{"selectedType", "enforcedType"} {
		if v, ok := s.Security.Auth[k]; ok && v != apiKeyAuth {
			return refusal(fmt.Sprintf("its .gemini/settings.json sets security.auth.%s to %v", k, v))
		}
	}
	if v, ok := s.Security.Auth["useExternal"]; ok && v != false {
		return refusal("its .gemini/settings.json sets security.auth.useExternal")
	}
	return nil
}

// signInError is a refusal to let a run reach a Google sign-in. It matches
// [adapter.ErrNotPlanAuth], so the run loop reports it as it reports a session
// that was not on the auth Rein requires.
type signInError struct{ why string }

func (e *signInError) Error() string {
	return "Gemini runs on the person's own Google key only, and " + e.why + "; nothing was started"
}

func (e *signInError) Is(target error) bool { return target == adapter.ErrNotPlanAuth }

func refusal(why string) error {
	return &signInError{why: "this repository would choose Gemini CLI's sign-in: " + why}
}

var lineComment = regexp.MustCompile(`(?m)^\s*//.*$`)

// stripJSONComments drops whole-line // comments, which Gemini CLI's settings
// allow. A comment after a value on the same line is left and fails the parse,
// which refuses the run: fail closed rather than guess.
func stripJSONComments(b []byte) []byte {
	return bytes.TrimSpace(lineComment.ReplaceAll(b, nil))
}
