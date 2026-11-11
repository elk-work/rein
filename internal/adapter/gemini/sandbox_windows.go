//go:build windows

package gemini

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// minSandboxBuild is Windows 11 version 24H2. Antigravity's terminal sandbox
// runs on it and later; before it, agy turns the sandbox off and asks before
// every command, which headless means refusing every command
// (antigravity.google/docs/permissions; agy changelog).
const minSandboxBuild = 26100

// sandboxUnsupported says, in one line a person reads, why Antigravity cannot
// keep a run inside its folder on this computer, or "" when it can.
func sandboxUnsupported() string { return sandboxUnsupportedOn(windows.RtlGetVersion().BuildNumber) }

func sandboxUnsupportedOn(build uint32) string {
	if build >= minSandboxBuild {
		return ""
	}
	return fmt.Sprintf("Antigravity needs Windows 11 version 24H2 or later to keep a run inside its folder, and this computer runs Windows build %d. Update Windows, or use Gemini CLI.", build)
}
