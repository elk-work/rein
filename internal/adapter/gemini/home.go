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
// Gemini CLI keeps everything it reads about who it is — settings, MCP
// servers, extensions, GEMINI.md, trusted folders, the files that hold a
// sign-in — under $GEMINI_CLI_HOME/.gemini, falling back to the real home when
// the variable is unset. A run gets a directory of its own, as a Codex run
// gets its own CODEX_HOME, for the same reason (ark:rein#21): the person's own
// MCP servers, the Elk connector among them, must not reach a run, because
// they act as the person.
//
// What comes in from the person's own home is their sign-in and nothing else:
//
//   - settings.json carries only their `security.auth` (or the older
//     top-level `selectedAuthType`): which sign-in Gemini CLI uses. With none
//     and a stored key, the key sign-in is chosen.
//   - The files that can hold a sign-in are linked, not copied (signInFiles).
//     On a Mac Gemini CLI keeps its Google sign-in in the keychain, which a
//     private home does not hide; the files are its fallback and older
//     storage. Rein never reads them: a link means Gemini CLI opens its own
//     file at a different path, as with Codex's auth.json.
//
// HOME itself is left alone, so git, gh and SSH inside the run are the
// person's own as with every other adapter. The home is removed when the
// session ends; [Adapter.ReapRun] removes any a crash left behind.

type home struct {
	dir string // GEMINI_CLI_HOME
}

// signInFiles are the files in a Gemini CLI home that can hold a sign-in or
// key, from Gemini CLI 0.64's own code: its encrypted-file fallback for the
// keychain, the older file it migrates from, the signed-in account, and the
// .env it reads a key from.
var signInFiles = []string{"gemini-credentials.json", "oauth_creds.json", "google_accounts.json", ".env"}

// realGeminiDir is the person's own `.gemini` folder.
func (a *Adapter) realGeminiDir() string {
	base := a.RealHome
	if base == "" {
		base = os.Getenv("GEMINI_CLI_HOME")
	}
	if base == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = h
	}
	return filepath.Join(base, ".gemini")
}

// personAuth is the sign-in choice in the person's own settings: their
// `security.auth` object, or the older top-level `selectedAuthType` as
// `{"selectedType": …}`. Nil when they have chosen none, or their settings
// cannot be read.
func personAuth(geminiDir string) map[string]any {
	if geminiDir == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(geminiDir, "settings.json"))
	if err != nil {
		return nil
	}
	var s struct {
		Security struct {
			Auth map[string]any `json:"auth"`
		} `json:"security"`
		Legacy string `json:"selectedAuthType"`
	}
	if json.Unmarshal(stripJSONComments(raw), &s) != nil {
		return nil
	}
	if t, _ := s.Security.Auth["selectedType"].(string); t != "" {
		return s.Security.Auth
	}
	if s.Legacy != "" {
		return map[string]any{"selectedType": s.Legacy}
	}
	return nil
}

// newHome writes a run's private home.
func (a *Adapter) newHome(spec adapter.RunSpec, key string) (*home, error) {
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
	real := a.realGeminiDir()
	blob, err := settingsFor(spec, personAuth(real), key != "")
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
	if real != "" {
		for _, f := range signInFiles {
			src := filepath.Join(real, f)
			if _, err := os.Stat(src); err != nil {
				continue
			}
			// A symbolic link needs a privilege on Windows that a hard link
			// does not; a file neither can link is left out, and a sign-in
			// held only there surfaces as the run's own error.
			dst := filepath.Join(conf, f)
			if os.Symlink(src, dst) != nil {
				_ = os.Link(src, dst)
			}
		}
	}
	return h, nil
}

// settingsFor is the run's settings.json: the person's sign-in choice, or the
// key sign-in when they made none and a key is stored, and the run's MCP
// servers.
func settingsFor(spec adapter.RunSpec, auth map[string]any, haveKey bool) ([]byte, error) {
	settings := map[string]any{
		// A run must not update the program under the person, nor nag.
		"general": map[string]any{"enableAutoUpdate": false, "enableAutoUpdateNotification": false},
	}
	switch {
	case auth != nil:
		settings["security"] = map[string]any{"auth": auth}
	case haveKey:
		settings["security"] = map[string]any{"auth": map[string]any{"selectedType": apiKeyAuth}}
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

// ownNames are the variables Rein sets on a Gemini CLI session itself; nothing
// inherited or from a queue may shadow them.
var ownNames = []string{"GEMINI_CLI_HOME", "GEMINI_CLI_TRUST_WORKSPACE", KeyName}

func ownName(name string) bool {
	for _, n := range ownNames {
		if strings.EqualFold(name, n) { // Windows variable names ignore case
			return true
		}
	}
	return false
}

// childEnv is the session's environment: base (the scoped inheritance), the
// spec's own variables, then the private home, trust and a stored key, which
// nothing before them can override.
func (h *home) childEnv(base []string, over map[string]string, key string) []string {
	out := make([]string, 0, len(base)+len(over)+3)
	for _, kv := range base {
		i := strings.IndexByte(kv, '=')
		if i <= 0 || ownName(kv[:i]) {
			continue
		}
		if _, shadowed := over[kv[:i]]; shadowed {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range over {
		if !ownName(k) {
			out = append(out, k+"="+v)
		}
	}
	out = append(out,
		"GEMINI_CLI_HOME="+h.dir,
		// Documented alongside --skip-trust; both, so a build that drops one
		// still trusts the worktree.
		"GEMINI_CLI_TRUST_WORKSPACE=true")
	if key != "" {
		out = append(out, KeyName+"="+key)
	}
	return out
}

// notSetUpError is a run or preflight that found the program not installed or
// not signed in on this computer. It matches [adapter.ErrLoginRequired] and
// [adapter.ErrPreflight], so Elk hears agent_login, as for an expired Claude
// Code sign-in.
type notSetUpError struct{ program, why string }

func (e *notSetUpError) Error() string {
	return NotSetUpLine(e.program) + " (" + e.why + ")"
}

func (e *notSetUpError) Is(target error) bool {
	return target == adapter.ErrLoginRequired || target == adapter.ErrPreflight
}

func notSetUp(program, why string) error { return &notSetUpError{program: program, why: why} }

var lineComment = regexp.MustCompile(`(?m)^\s*//.*$`)

// stripJSONComments drops whole-line // comments, which Gemini CLI's settings
// allow. A comment after a value on the same line is left and fails the parse.
func stripJSONComments(b []byte) []byte {
	return bytes.TrimSpace(lineComment.ReplaceAll(b, nil))
}
