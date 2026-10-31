//go:build darwin

package capture

import (
	"os"
	"testing"
)

// The process table parser against this test's own pid. It does not print the
// table: a line that did would be a list of whatever else is running.
func TestSystemTreeSeesThisProcess(t *testing.T) {
	tree, err := systemTree()
	if err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	if _, ok := tree.PPID[self]; !ok {
		t.Fatalf("the process table did not include pid %d", self)
	}
	if tree.PPID[self] <= 0 {
		t.Fatalf("pid %d has no parent in the table", self)
	}
	// pid 1 is launchd. Whatever launched this test, launchd is not a process
	// the test started, and the capture process is not one either.
	if tree.Launched(1, self) {
		t.Fatal("pid 1 was treated as a process this run launched")
	}
	if tree.Launched(self, self) {
		t.Fatal("the capture process counted itself as a window it may photograph")
	}
}
