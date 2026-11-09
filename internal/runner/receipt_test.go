package runner

import (
	"encoding/json"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

func TestDidLine(t *testing.T) {
	if got := didLine(nil, []string{"go test ./..."}, []string{"a.ts", "b.ts", "a.ts"}); got != "Edited a.ts, b.ts · ran the tests" {
		t.Errorf("didLine = %q", got)
	}
	if got := didLine(nil, nil, nil); got != "" {
		t.Errorf("nothing = %q", got)
	}
	if got := didLine([]string{"Read", "Read"}, []string{"git status"}, nil); got != "Ran git · ran Read" {
		t.Errorf("other tools = %q", got)
	}
	in, _ := json.Marshal(map[string]string{"file_path": "/w/src/a.ts"})
	if got := editedFile(&adapter.ToolUse{Name: "Edit", Input: in}); got != "a.ts" {
		t.Errorf("editedFile(Edit) = %q", got)
	}
	if got := editedFile(&adapter.ToolUse{Name: "Read", Input: in}); got != "" {
		t.Errorf("editedFile(Read) = %q", got)
	}
}

func TestWhyNext(t *testing.T) {
	why, next := whyNext("Done.\nWhy: the Taps list skips private rows\nnext: run the Mac tests\n")
	if why != "the Taps list skips private rows" || next != "run the Mac tests" {
		t.Errorf("whyNext = %q, %q", why, next)
	}
	if why, next := whyNext("nothing here\nNextly"); why != "" || next != "" {
		t.Errorf("whyNext without = %q, %q", why, next)
	}
}

// Why and Next are read from what the agent wrote since the last receipt,
// so a turn-one Why does not ride every later one.
func TestWhyNextSince(t *testing.T) {
	off := 0
	if why, next := whyNextSince("Why: a\nNext: b\n", &off); why != "a" || next != "b" {
		t.Errorf("first = %q, %q", why, next)
	}
	if why, next := whyNextSince("Why: a\nNext: b\nmore text\n", &off); why != "" || next != "" {
		t.Errorf("second rode the first's lines: %q, %q", why, next)
	}
}
