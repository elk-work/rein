package grok

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
)

// sandboxCommand wraps the entire agent, so tools and credential helpers inherit
// the same kernel boundary. sandbox-exec fails closed, before executing Grok.
// We cannot stack Grok's Seatbelt profile: it denies the Keychain IPC that this
// profile deliberately preserves, and a child cannot relax an inherited sandbox.
func (h *privateHome) sandboxCommand(ctx context.Context, binary string, args []string, spec adapter.RunSpec) (*exec.Cmd, error) {
	if spec.Env[sandboxEnv] != sandboxMacOS {
		return exec.Command(binary, args...), nil
	}
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("grok: macOS sandbox requested on %s", runtime.GOOS)
	}
	profile, err := h.writeMacSandbox(ctx, spec.WorktreeDir)
	if err != nil {
		return nil, err
	}
	return exec.Command("/usr/bin/sandbox-exec", append([]string{"-f", profile, binary}, args...)...), nil
}

func (h *privateHome) commandEnv(spec adapter.RunSpec) []string {
	extra := spec.Env
	if extra[sandboxEnv] == sandboxMacOS {
		// Only this native-sandbox launch disables the vendor's inner sandbox.
		// Rein's logical mode remains rein-macos, so callers cannot request off.
		extra = mergeMaps(extra, map[string]string{sandboxEnv: "off", "TMPDIR": filepath.Join(h.dir, "tmp"), "TMP": filepath.Join(h.dir, "tmp"), "TEMP": filepath.Join(h.dir, "tmp")})
	}
	return h.runEnv(spec.InheritedEnv(), extra)
}

func (h *privateHome) writeMacSandbox(ctx context.Context, work string) (string, error) {
	work, err := filepath.EvalSymlinks(work)
	if err != nil {
		return "", err
	}
	home, err := filepath.EvalSymlinks(h.dir)
	if err != nil {
		return "", err
	}
	paths, err := gitWritePaths(ctx, work)
	if err != nil {
		return "", err
	}
	paths = append(paths, work, home)
	if sessions, err := filepath.EvalSymlinks(filepath.Join(home, "sessions")); err == nil {
		paths = append(paths, sessions)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(home, "tmp"), 0700); err != nil {
		return "", err
	}
	// This is a write boundary, not a read/network/IPC boundary. In particular,
	// securityd and the credential helper retain their normal IPC permissions.
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n")
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("grok: sandbox path is not absolute")
		}
		fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", strconv.Quote(path))
	}
	b.WriteString("(allow file-write* (literal \"/dev/null\") (literal \"/dev/tty\"))\n")
	// Keep global configuration and hook sources immutable, like Grok's
	// workspace profile. Pin the home itself against rename as well.
	fmt.Fprintf(&b, "(deny file-write* (literal %s))\n", strconv.Quote(home))
	for _, name := range []string{"hooks", "hooks-paths", "config.toml", "sandbox.toml", "sandbox.sb", "trusted_folders.toml", "managed_config.toml", "requirements.toml", "auth.json"} {
		fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", strconv.Quote(filepath.Join(home, name)))
	}
	profile := filepath.Join(home, "sandbox.sb")
	if err := os.WriteFile(profile, []byte(b.String()), 0600); err != nil {
		return "", err
	}
	return profile, nil
}
