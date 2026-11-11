//go:build windows

package gemini

import (
	"strings"
	"testing"
)

// Before Windows 11 24H2 Antigravity has no terminal sandbox and headless runs
// would refuse every command, so Rein says why in one line instead.
func TestOldWindowsIsRefusedInOneLine(t *testing.T) {
	if why := sandboxUnsupportedOn(22631); !strings.Contains(why, "24H2") || strings.Contains(why, "\n") {
		t.Fatalf("23H2: %q", why)
	}
	if why := sandboxUnsupportedOn(26100); why != "" {
		t.Fatalf("24H2: %q", why)
	}
}
