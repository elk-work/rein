//go:build !windows

package claude

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

// Preflight runs the version check on what `claude --version` prints
// (version_test.go has the cases on every platform).
func TestPreflightRefusesAnOldClaudeCode(t *testing.T) {
	for _, tc := range []struct {
		out     string
		refused bool
	}{
		{"2.1.227 (Claude Code)", true},
		{"2.0.999 (Claude Code)", true},
		{"1.9.300", true},
		{"2.1.228 (Claude Code)", false},
		{"2.1.251 (Claude Code)", false},
		{"2.2.0 (Claude Code)", false},
		{"3.0.0", false},
		{"Claude test", false},
	} {
		path := stubBinary(t, `case "$1" in
 --version) echo '`+tc.out+`'; exit 0;;
 auth) echo '{"loggedIn":true}'; exit 0;;
 *) echo '{"type":"result","subtype":"success","is_error":false}'; exit 0;;
 esac`)
		err := (&Adapter{Binary: path}).Preflight(context.Background())
		if !tc.refused {
			if err != nil {
				t.Errorf("%q: refused: %v", tc.out, err)
			}
			continue
		}
		if !errors.Is(err, adapter.ErrPreflight) {
			t.Fatalf("%q: not refused as a preflight failure: %v", tc.out, err)
		}
		installed := strings.Fields(tc.out)[0]
		for _, want := range []string{"Claude Code " + installed + " is installed", "2.1.228 or later", "claude update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: message %q does not say %q", tc.out, err, want)
			}
		}
	}
}
