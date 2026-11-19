package runner

import (
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
)

// TestWithheldNoteNamesWhatIsWithheld: a run refused for a name this machine
// withholds says so, rather than only suggesting the config line that would
// not help.
func TestWithheldNoteNamesWhatIsWithheld(t *testing.T) {
	qr := &queueRunner{
		r: &Runner{opts: Options{Config: config.Config{WithholdCapabilities: []string{"xcode"}}}},
		q: config.Queue{Name: "mac-claude", WithholdCapabilities: []string{"docker"}},
	}
	note := qr.withheldNote([]string{"tool:xcode", "docker", "node"})
	if !strings.Contains(note, `"tool:xcode", "docker"`) || strings.Contains(note, "node") {
		t.Errorf("note = %q — want the withheld names and only those", note)
	}
	if got := qr.withheldNote([]string{"node"}); got != "" {
		t.Errorf("a name nobody withholds got a note: %q", got)
	}
}
