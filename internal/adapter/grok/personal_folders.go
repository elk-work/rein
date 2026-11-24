package grok

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
)

// Keeping a run out of the person's own folders (Elk #1832, ark:rein#162).
// Grok offers two layers, and Rein uses both where they exist (read from
// xai-org/grok-build, 2026-11-25; not yet run against a released Grok):
//
//   - Permission rules: `[permission] deny` in $GROK_HOME/config.toml, Read
//     and Edit rules that `grok agent` checks before yoloMode's approve-all
//     (xai-grok-workspace permission manager). They cover Grok's file tools
//     and the file paths a shell command names, so, like the Claude Code
//     rules, a default and not a sandbox. Every platform, Windows included.
//     Patterns are absolute, forward-slashed and case-sensitive; `~` is not
//     expanded.
//   - The OS sandbox, which denies reads and writes in the kernel: on macOS
//     Rein's own Seatbelt profile (sandbox.go), on Linux the `deny` list of
//     the `rein-worktree` profile, which Grok enforces with bubblewrap and
//     refuses to start without. Grok has no sandbox on Windows at all, so
//     there the permission rules are the only layer.
//
// `grok agent` takes no `--deny` flag, so the rules can only come through the
// private home.

// personalFolders is where the folders this run stays out of are; tests
// replace it.
var personalFolders = adapter.DeniedPersonalFoldersForRun

// deniedFolders is the folders a run stays out of, as absolute paths: none
// when the queue allows them, and never one holding the run's private home.
// Unknown home on macOS or Linux leaves nothing to name: Grok expands no `~`.
func (h *privateHome) deniedFolders(spec adapter.RunSpec) []string {
	if spec.PersonalFolders {
		return nil
	}
	home, _ := os.UserHomeDir()
	var out []string
	for _, f := range personalFolders(home, spec.WorktreeDir) {
		if !adapter.HoldsAny(f.Path, []string{h.dir}) {
			out = append(out, f.Path)
		}
	}
	return out
}

// permissionDenies is the `[permission] deny` list: each folder itself (a
// listing is a Read of the folder's own path) and everything in it.
func permissionDenies(folders []string) []string {
	var deny []string
	for _, f := range folders {
		p := filepath.ToSlash(filepath.Clean(f))
		deny = append(deny, "Read("+p+")", "Read("+p+"/**)", "Edit("+p+")", "Edit("+p+"/**)")
	}
	return deny
}

// sandboxDenies is the folders as the kernel sees them: as written and with
// symlinks resolved, without repeats.
func sandboxDenies(folders []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range folders {
		for _, p := range []string{filepath.Clean(f), adapter.Resolved(f)} {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// profileDenies is the `deny` list of Grok's own sandbox profile, for the
// folders that exist: Grok mounts a placeholder over each denied path, and
// one over a folder that is not there could leave an empty Documents in the
// person's home. Grok reads `*`, `?` and `[` in the list as a glob and
// refuses `{`, so a folder named with one cannot be denied literally, and
// the run does not start rather than run without it.
func profileDenies(folders []string) ([]string, error) {
	var out []string
	for _, p := range sandboxDenies(folders) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if strings.ContainsAny(p, "*?[{") {
			return nil, fmt.Errorf("grok: personal folder %q cannot be named in Grok's sandbox", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// seatbeltDenies are the Seatbelt rules that close each folder to reads and
// writes. They go last in the profile: the last matching rule wins.
func seatbeltDenies(folders []string) (string, error) {
	var b strings.Builder
	for _, p := range sandboxDenies(folders) {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("grok: sandbox path is not absolute")
		}
		fmt.Fprintf(&b, "(deny file-read* file-write* (subpath %s))\n", strconv.Quote(p))
	}
	return b.String(), nil
}
