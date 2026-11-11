package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
// What comes in from the person's own home is their key setup and nothing
// else (keyAuth):
//
//   - settings.json carries their sign-in choice (`security.auth.selectedType`,
//     or the older top-level `selectedAuthType`) only when it is a key: the
//     Gemini API key or Vertex AI. A choice of a Google sign-in is never
//     carried: the run is put on the key sign-in instead when there is a key,
//     and refused when there is none. The run's settings also enforce that
//     choice (`enforcedType`), so Gemini CLI refuses to start on any other,
//     whatever else would choose one. Rein never drives a personal Google
//     sign-in.
//   - Their `.env`, where Gemini CLI reads a key, is linked, not copied
//     (keyFiles). Rein never opens it. The files that hold a Google sign-in
//     are never linked.
//
// HOME itself is left alone, so git, gh and SSH inside the run are the
// person's own as with every other adapter. The home is removed when the
// session ends; [Adapter.ReapRun] removes any a crash left behind.

type home struct {
	dir string // GEMINI_CLI_HOME
}

// keyFiles are the files in a person's Gemini CLI home a run may use: the
// .env Gemini CLI reads a key from (Gemini CLI 0.64). Never
// gemini-credentials.json, oauth_creds.json or google_accounts.json, which
// hold a Google sign-in.
var keyFiles = []string{".env"}

// keyAuthTypes are Gemini CLI's sign-in choices that run on the person's own
// key: Google AI Studio's Gemini API key and Google Cloud's Vertex AI.
var keyAuthTypes = map[string]bool{apiKeyAuth: true, "vertex-ai": true}

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

// keyAuth is the sign-in choice a run's settings carry, and enforce: the
// person's own when it is a key (keyAuthTypes), and nothing else of their
// `security.auth`; otherwise the Gemini API key sign-in, when a key is stored
// for Rein or their `.env` can give one (only its presence is looked at; Rein
// never opens it). With neither it is the not-set-up refusal.
func keyAuth(geminiDir string, storedKey bool) (map[string]any, error) {
	auth := personAuth(geminiDir)
	if t, _ := auth["selectedType"].(string); keyAuthTypes[t] {
		return map[string]any{"selectedType": t, "enforcedType": t}, nil
	}
	if storedKey || hasEnvFile(geminiDir) {
		return map[string]any{"selectedType": apiKeyAuth, "enforcedType": apiKeyAuth}, nil
	}
	if auth != nil {
		return nil, notSetUp("its settings choose a Google sign-in, which Rein never drives, and it has no key")
	}
	return nil, notSetUp("it has no Google AI Studio or Google Cloud key")
}

func hasEnvFile(geminiDir string) bool {
	if geminiDir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(geminiDir, ".env"))
	return err == nil && !info.IsDir()
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
	auth, err := keyAuth(real, key != "")
	if err != nil {
		h.remove()
		return nil, err
	}
	blob, err := settingsFor(spec, auth)
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
		for _, f := range keyFiles {
			src := filepath.Join(real, f)
			if _, err := os.Stat(src); err != nil {
				continue
			}
			// A symbolic link needs a privilege on Windows that a hard link
			// does not; a file neither can link is left out, and a key
			// held only there surfaces as the run's own error.
			dst := filepath.Join(conf, f)
			if os.Symlink(src, dst) != nil {
				_ = os.Link(src, dst)
			}
		}
	}
	return h, nil
}

// settingsFor is the run's settings.json: the key sign-in choice keyAuth
// made, and the run's MCP servers.
func settingsFor(spec adapter.RunSpec, auth map[string]any) ([]byte, error) {
	settings := map[string]any{
		// A run must not update the program under the person, nor nag.
		"general":  map[string]any{"enableAutoUpdate": false, "enableAutoUpdateNotification": false},
		"security": map[string]any{"auth": auth},
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

// ownNames are the variables Rein sets on a Gemini CLI session itself, and
// those that could move a run off the key it checked: GOOGLE_GENAI_USE_GCA
// chooses a Google sign-in, and the system settings paths and the auth
// override would let a run read settings Rein never looked at. Nothing
// inherited or from a queue may set them.
var ownNames = []string{"GEMINI_CLI_HOME", "GEMINI_CLI_TRUST_WORKSPACE", KeyName, "GOOGLE_GENAI_USE_GCA",
	"GEMINI_CLI_SYSTEM_SETTINGS_PATH", "GEMINI_CLI_SYSTEM_DEFAULTS_PATH", "GEMINI_CLI_AUTH_OVERRIDE"}

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

// checkWorkspaceAuth refuses a worktree whose checked-in Gemini settings
// choose anything but the Gemini API key. Gemini CLI layers the workspace's
// .gemini/settings.json over the private home's (measured: a checked-in
// `enforcedType: oauth-personal` opened Google's sign-in page), so a
// repository could otherwise move a run onto a Google sign-in, which Rein
// never drives. A file Rein cannot read is refused too: failing closed is the
// point.
func checkWorkspaceAuth(worktree string) error {
	return checkAuthFile(filepath.Join(worktree, ".gemini", "settings.json"),
		"this repository would choose Gemini CLI's sign-in", "its .gemini/settings.json")
}

// systemSettingsPath is where Gemini CLI reads this computer's system
// settings, which it applies over every other layer. Rein never passes
// GEMINI_CLI_SYSTEM_SETTINGS_PATH to a run (see ownNames), so this default is
// the file a run sees. A variable so tests can point it elsewhere.
var systemSettingsPath = defaultSystemSettingsPath(runtime.GOOS)

func defaultSystemSettingsPath(goos string) string {
	switch goos {
	case "darwin":
		return "/Library/Application Support/GeminiCli/settings.json"
	case "windows":
		return `C:\ProgramData\gemini-cli\settings.json`
	default:
		return "/etc/gemini-cli/settings.json"
	}
}

// checkSystemAuth refuses a run when this computer's Gemini CLI system
// settings choose anything but the Gemini API key: they outrank the private
// home's, as a repository's do.
func checkSystemAuth() error {
	return checkAuthFile(systemSettingsPath,
		"this computer's Gemini CLI system settings would choose its sign-in", "the file "+systemSettingsPath)
}

func checkAuthFile(path, who, what string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return refusal(who, what+" could not be read: "+err.Error())
	}
	var s struct {
		Security struct {
			Auth map[string]any `json:"auth"`
		} `json:"security"`
		Legacy any `json:"selectedAuthType"`
	}
	if err := json.Unmarshal(stripJSONComments(raw), &s); err != nil {
		return refusal(who, what+" is not JSON Rein can read, so its sign-in cannot be checked")
	}
	for _, k := range []string{"selectedType", "enforcedType"} {
		if v, ok := s.Security.Auth[k]; ok {
			if v != apiKeyAuth {
				return refusal(who, fmt.Sprintf("%s sets security.auth.%s to %v", what, k, v))
			}
		}
	}
	if s.Legacy != nil && s.Legacy != apiKeyAuth {
		return refusal(who, fmt.Sprintf("%s sets selectedAuthType to %v", what, s.Legacy))
	}
	if v, ok := s.Security.Auth["useExternal"]; ok && v != false {
		return refusal(who, what+" sets security.auth.useExternal")
	}
	return nil
}

// signInError is a refusal to let a repository move a run onto a Google
// sign-in. It matches [adapter.ErrNotPlanAuth], so the run loop reports it as
// a session that was not on the auth Rein requires.
type signInError struct{ why string }

func (e *signInError) Error() string {
	return "Gemini runs on the person's own Google AI Studio or Google Cloud key only, and " + e.why + "; nothing was started"
}

func (e *signInError) Is(target error) bool { return target == adapter.ErrNotPlanAuth }

func refusal(who, why string) error {
	return &signInError{why: who + ": " + why}
}

// notSetUpError is a run or preflight that found Gemini CLI not set up on a
// key on this computer. It matches [adapter.ErrLoginRequired] and
// [adapter.ErrPreflight], so Elk hears agent_login, as for an expired Claude
// Code sign-in.
type notSetUpError struct{ why string }

func (e *notSetUpError) Error() string { return NotSetUpLine + " (" + e.why + ")" }

func (e *notSetUpError) Is(target error) bool {
	return target == adapter.ErrLoginRequired || target == adapter.ErrPreflight
}

func notSetUp(why string) error { return &notSetUpError{why: why} }

var lineComment = regexp.MustCompile(`(?m)^\s*//.*$`)

// stripJSONComments drops whole-line // comments, which Gemini CLI's settings
// allow. A comment after a value on the same line is left and fails the parse.
func stripJSONComments(b []byte) []byte {
	return bytes.TrimSpace(lineComment.ReplaceAll(b, nil))
}
