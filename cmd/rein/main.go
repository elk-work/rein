// Command rein is the Elk runner: it enrols a developer machine into an Elk
// workspace as named queues and drives coding-agent sessions against the runs
// dispatched to them. See docs/rein.md in elk-work/elk.
package main

import (
	"os"

	"github.com/elk-work/rein/internal/cli"

	// Registering an adapter is one blank import: each package's init calls
	// adapter.MustRegister, so this list is the whole answer to "which agents
	// can this build drive" (docs/adapters.md).
	_ "github.com/elk-work/rein/internal/adapter/claude"
	_ "github.com/elk-work/rein/internal/adapter/codex"
	_ "github.com/elk-work/rein/internal/adapter/grok"
)

// Version is set at release time by -ldflags "-X main.Version=v0.1.0".
var Version = "dev"

func main() {
	os.Exit(cli.Execute(cli.Options{
		Version: Version,
		Args:    os.Args[1:],
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	}))
}
