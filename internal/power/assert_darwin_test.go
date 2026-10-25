package power

import (
	"os/exec"
	"testing"
)

// TestCaffeinateHoldsAndLetsGo takes the real assertion and lets it go. It is
// the one test here that spawns anything, and it cleans up after itself; on a
// Mac without caffeinate it is skipped.
func TestCaffeinateHoldsAndLetsGo(t *testing.T) {
	if _, err := exec.LookPath(caffeinate); err != nil {
		t.Skip("no caffeinate on this machine")
	}
	stop, err := startCaffeinate()
	if err != nil {
		t.Fatalf("startCaffeinate: %v", err)
	}
	stop() // returns only once the process has exited
}
