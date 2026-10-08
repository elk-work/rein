package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Exit code 2 is part of the published contract — a service manager and a
// shell script both read it, and it has to keep meaning "this command does not
// exist yet" rather than "it ran and failed".
//
// Nothing in the tree is a stub any more, so the mechanism is exercised
// against a synthetic one. Deleting it along with the last command that used
// it would have retired a documented contract by accident.
func TestNotImplementedExitsTwo(t *testing.T) {
	root := NewRootCommand("v0")
	root.AddCommand(&cobra.Command{
		Use:  "stubbed",
		RunE: notImplemented("stubbed", "ark:rein#999"),
	})

	var out, errb bytes.Buffer
	code := execute(root, Options{Args: []string{"stubbed"}, Stdout: &out, Stderr: &errb})
	if code != ExitNotImplemented {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, ExitNotImplemented, errb.String())
	}
	if !strings.Contains(errb.String(), "not implemented in v0") {
		t.Errorf("stderr does not say what happened: %q", errb.String())
	}
	if !strings.Contains(errb.String(), "ark:rein#999") {
		t.Errorf("stderr does not name the task that implements it: %q", errb.String())
	}
}
