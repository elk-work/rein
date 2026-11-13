package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/worktree"
)

// A run-private `$GROK_HOME`, which is how a Rein session is kept from
// inheriting the developer's own agent configuration.
//
// # Why this exists
//
// On 2026-08-29 a dogfood run on this Mac (`arun-18298891`) **submitted its own
// deliverable**. Grok had picked up the Elk MCP connector out of
// `~/.grok/config.toml` — the developer's personal one — and called
// `submit_deliverable` under his identity mid-run. Elk reviewed that
// submission, Rein then submitted again, and the run ended in a state the
// review sweep treats as already reviewed, so it was never re-reviewed and
// could not be accepted (`ark:rein#21`).
//
// The inherited server is not merely extra capability. It is capability *as
// somebody else*, and a runner that hands an agent the operator's own
// credentials has no story about who did what.
//
// # Why a private home rather than a flag
//
// There is no flag. What was checked, against `grok 1.0.13`:
//
//   - `grok agent --help` has no MCP switch at all.
//   - ACP's `session/new` `mcpServers` is **additive** here: a probe passing one
//     server saw the agent's own tally go from two to three, not to one.
//   - `GROK_CONFIG` is allowlisted to `models`, `features`, a narrowed
//     `toolset` and `shell_environment_policy`; the documentation says outright
//     that it cannot add a discovery source, and `mcp_servers` is not on the
//     list either way.
//   - `disabled_mcp_servers` and `[mcp_servers.<name>].enabled` both need the
//     names, which means reading the developer's config and hoping nothing is
//     added mid-run.
//
// `GROK_HOME` is the switch that exists: Grok reads its configuration from
// there, so a home with no `config.toml` has no MCP servers, no hooks and no
// personal model pin. Verified — `_x.ai/mcp/servers_updated` arrives empty and
// the Elk connector is gone.
//
// # What is carried across, and why each one
//
//   - `auth.json` — the login. Rein does not read, mint, refresh or replay a
//     credential (elk `docs/rein.md` §3); a symlink means the *vendor binary*
//     opens its own credential file at a different path, which is a different
//     act from Rein handling the token. Verified: `grok models` under a private
//     home with only this link still reports the account logged in, and without
//     it reports "You are not authenticated."
//   - `sessions/` — session state. `session/load` reads it, and `resume` is a
//     declared capability, so it has to survive a home that does not.
//
// Plugins are enumerated and disabled before the agent starts; see configure.
type privateHome struct {
	dir  string
	real string

	// stopFailureLog is where Rein's StopFailure hook appends, and "" on a
	// platform that gets no hook. See ratelimit.go.
	stopFailureLog string
}

// carried names what a private home links back to the real one. Adding to this
// list is a decision about what a run may inherit; nothing that *configures*
// the agent belongs here.
var carried = []string{
	"auth.json", // the login
	"sessions",  // session state, for session/load
}

// realGrokHome is where Grok keeps the developer's configuration and login.
func realGrokHome() (string, error) {
	if h := os.Getenv("GROK_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("grok: cannot locate the home directory: %w", err)
	}
	return filepath.Join(home, ".grok"), nil
}

// newPrivateHome builds one. It fails rather than falling back to the real
// home: a session that cannot be isolated must not start, because the failure
// mode is a run acting as the developer.
func newPrivateHome(runID string) (*privateHome, error) {
	real, err := realGrokHome()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(real, "auth.json")); err != nil {
		return nil, fmt.Errorf("%w: no Grok login at %s/auth.json; run `grok login`",
			adapter.ErrPreflight, real)
	}

	dir, err := os.MkdirTemp("", "rein-grok-home-"+sanitizeRunID(runID))
	if err != nil {
		return nil, fmt.Errorf("grok: creating a private GROK_HOME: %w", err)
	}
	// The resolved path, not the one MkdirTemp returned. On macOS the temp
	// directory sits under /var, which is a symlink to /private/var, and a
	// hook source path with a symlink component is exactly what Grok's sandbox
	// refuses to start with (ark:rein#16) — which matters now that the home
	// carries a hook of Rein's own.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	h := &privateHome{dir: dir, real: real}

	// Real, empty, and not links. Grok's sandbox refuses to start when a hook
	// source path has a symlink component — that is what it is protecting
	// against, since a symlink can be retargeted — and the developer's own
	// ~/.grok/hooks is full of them on this machine (ark:rein#16). Giving the
	// private home its own empty pair removes that cause entirely, and removes
	// the developer's hooks from the run as a side effect, which is the same
	// inheritance this file is about.
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o700); err != nil {
		h.remove()
		return nil, fmt.Errorf("grok: preparing the private GROK_HOME: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "hooks-paths"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		h.remove()
		return nil, fmt.Errorf("grok: preparing the private GROK_HOME: %w", err)
	}
	_ = f.Close()

	// The one hook a run does get: Rein's own, which records a turn that ended
	// on an API error so a rate limit can be recognised (ratelimit.go). A home
	// that cannot take it still isolates the run, so this is not fatal.
	if hookSupported() {
		if logPath, err := installStopFailureHook(dir); err == nil {
			h.stopFailureLog = logPath
		}
	}

	for _, name := range carried {
		target := filepath.Join(real, name)
		if _, err := os.Lstat(target); err != nil {
			continue // a fresh install has no sessions yet
		}
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil && !os.IsExist(err) {
			h.remove()
			return nil, fmt.Errorf("grok: linking %s into the private home: %w", target, err)
		}
	}
	return h, nil
}

// env returns the variables that isolate one session. These compatibility
// switches are documented in Grok's own config reference, compat section:
// https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/26-config-reference.md
// They disable foreign configuration scanners, not Grok's native hooks; Rein's
// StopFailure hook in the private home stays enabled.
func (h *privateHome) env() map[string]string {
	env := map[string]string{"GROK_HOME": h.dir}
	for _, tool := range []string{"CLAUDE", "CURSOR"} {
		for _, surface := range []string{"MCPS", "HOOKS", "AGENTS", "RULES", "SKILLS"} {
			env["GROK_"+tool+"_"+surface+"_ENABLED"] = "0"
		}
	}
	return env
}

// runEnv keeps isolation authoritative even when the caller supplies an env.
func (h *privateHome) runEnv(base []string, extra map[string]string) []string {
	return mergeEnv(base, mergeMaps(extra, h.env()))
}

// remove deletes the private home — unless the login stopped being a symlink.
//
// Grok may rotate its own token, and an atomic write (temp file, rename) would
// replace the link with a real file holding a **newer** credential than the one
// in the real home. Deleting it would log the developer out; copying it back
// would mean this package handling a token, which it must never do. So it does
// neither: it leaves the directory and reports where it is.
func (h *privateHome) remove() {
	if h == nil || h.dir == "" {
		return
	}
	if h.rotatedLogin() != "" {
		return
	}
	_ = os.RemoveAll(h.dir)
}

// rotatedLogin reports a warning when the private home holds a login that is no
// longer a link to the real one, and "" when there is nothing wrong.
func (h *privateHome) rotatedLogin() string {
	if h == nil || h.dir == "" {
		return ""
	}
	link := filepath.Join(h.dir, "auth.json")
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	return fmt.Sprintf("grok rotated its login inside the run-private home; "+
		"%s holds a newer credential than %s/auth.json and was left in place — "+
		"move it across by hand (rein never reads a credential)", link, h.real)
}

// sanitizeRunID keeps a run id usable as part of a directory name, so an
// abandoned private home says which run left it behind.
func sanitizeRunID(runID string) string {
	var b strings.Builder
	for _, r := range runID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
		if b.Len() >= 40 {
			break
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String() + "-"
}

// configure runs discovery with the same private home, cwd and environment as
// the agent. A failed or unrecognisable inventory must not start a session.
func (h *privateHome) configure(ctx context.Context, binary string, spec adapter.RunSpec) error {
	cmd := exec.CommandContext(ctx, binary, "plugin", "list", "--json")
	cmd.Dir = spec.WorktreeDir
	cmd.Env = h.runEnv(spec.InheritedEnv(), mergeMaps(spec.Env, map[string]string{sandboxEnv: sandboxReadOnly}))
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("grok: enumerate plugins: %w", err)
	}
	names, err := pluginNames(out)
	if err != nil {
		return err
	}
	if err := h.writeTOML("config.toml", map[string]any{"plugins": map[string]any{"disabled": names}}); err != nil {
		return err
	}
	if spec.Env[sandboxEnv] != sandboxWorktree {
		return nil
	}
	paths, err := gitWritePaths(ctx, spec.WorktreeDir)
	if err != nil {
		return err
	}
	// Session state is intentionally carried across homes for ACP resume.
	// Grant only that directory, never the real home's configuration or login.
	if sessions, err := filepath.EvalSymlinks(filepath.Join(h.dir, "sessions")); err == nil {
		if strings.ContainsAny(sessions, "*?[") || strings.TrimSpace(sessions) != sessions {
			return fmt.Errorf("grok: session path cannot be represented by sandbox")
		}
		paths = append(paths, sessions)
	} else if !os.IsNotExist(err) {
		return err
	}
	return h.writeTOML("sandbox.toml", map[string]any{"profiles": map[string]any{sandboxWorktree: map[string]any{"extends": "workspace", "read_write": paths}}})
}

func pluginNames(data []byte) ([]string, error) {
	var plugins []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &plugins); err != nil || plugins == nil {
		return nil, fmt.Errorf("grok: invalid plugin inventory")
	}
	names := make([]string, 0, len(plugins))
	seen := map[string]bool{}
	for _, p := range plugins {
		if strings.TrimSpace(p.Name) == "" {
			return nil, fmt.Errorf("grok: plugin inventory has an unnamed plugin")
		}
		if !seen[p.Name] {
			names = append(names, p.Name)
			seen[p.Name] = true
		}
	}
	sort.Strings(names)
	return names, nil
}

func (h *privateHome) writeTOML(name string, value any) error {
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(value); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(h.dir, name), b.Bytes(), 0o600)
}

// Git keeps a linked checkout's index/HEAD apart from its shared object and
// reference storage. Never grant the entire common directory: that would also
// expose other worktrees' indexes and the repository's configuration/hooks.
func gitWritePaths(ctx context.Context, dir string) ([]string, error) {
	resolve := func(flag string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", worktree.SafeGit("rev-parse", "--path-format=absolute", flag)...)
		cmd.Dir = dir
		cmd.Env = mergeEnv(os.Environ(), map[string]string{"LC_ALL": "C"})
		out, err := cmd.Output()
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok && strings.Contains(string(e.Stderr), "not a git repository") {
				return "", nil
			}
			return "", fmt.Errorf("grok: resolve worktree git metadata: %w", err)
		}
		path, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
		if err != nil {
			return "", err
		}
		if strings.ContainsAny(path, "*?[") || strings.TrimSpace(path) != path {
			return "", fmt.Errorf("grok: git path cannot be represented by sandbox: %q", path)
		}
		return path, nil
	}
	common, err := resolve("--git-common-dir")
	if err != nil {
		return nil, err
	}
	if common == "" {
		return []string{}, nil
	}
	gitDir, err := resolve("--git-dir")
	if err != nil {
		return nil, err
	}
	paths := []string{filepath.Join(common, "objects"), filepath.Join(common, "refs"), filepath.Join(common, "logs", "refs")}
	if gitDir != common {
		paths = append(paths, gitDir)
	}
	return paths, nil
}
