package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The Claude adapter turns stream-json it has no kind for into progress text
// (claude/stream.go "claude event <type>", "claude system/<subtype>"); the
// hook lines in claude/testdata/tools.jsonl decode to the second shape.
func TestVendorNoise(t *testing.T) {
	for text, want := range map[string]bool{
		"claude system/hook_started": true,
		"claude event stream_event":  true,
		"reading the code":           false,
		"permission denied: no":      false,
	} {
		if got := vendorNoise(text); got != want {
			t.Errorf("vendorNoise(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestReplyWindow(t *testing.T) {
	opened := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		opened, now time.Time
		want        bool
	}{
		{time.Time{}, opened, false},
		{opened, opened.Add(time.Second), true},
		{opened, opened.Add(3*time.Minute - time.Second), true},
		{opened, opened.Add(3 * time.Minute), false},
	} {
		if got := inReplyWindow(c.opened, c.now); got != c.want {
			t.Errorf("inReplyWindow(%v, %v) = %v, want %v", c.opened, c.now, got, c.want)
		}
	}
}

func TestSayPartsSplitOnRunesUnderTheCap(t *testing.T) {
	long := strings.Repeat("é", 2500) // 5000 bytes
	parts := sayParts(long)
	if len(parts) != 2 || strings.Join(parts, "") != long {
		t.Fatalf("parts = %d, rejoined equal = %v", len(parts), strings.Join(parts, "") == long)
	}
	for _, p := range parts {
		if len(p) > sayMax || !utf8.ValidString(p) {
			t.Errorf("part of %d bytes, valid UTF-8 %v", len(p), utf8.ValidString(p))
		}
	}
	if got := sayParts("  \n"); len(got) != 0 {
		t.Errorf("blank text made parts: %q", got)
	}
}

func TestRepoPath(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		filepath.Join(dir, "a", "b.go"):  "a/b.go",
		filepath.Join(real, "c.go"):      "c.go",
		"docs/x.md":                      "docs/x.md",
		"../escape.go":                   "",
		"/etc/hosts":                     "",
		filepath.Join(dir, "..", "x.go"): "",
	} {
		if got := repoPath(dir, in); got != want {
			t.Errorf("repoPath(%q) = %q, want %q", in, got, want)
		}
	}
	// The worktree named through a symlink, the file through its real path.
	if got := repoPath(link, filepath.Join(real, "d.go")); got != "d.go" {
		t.Errorf("through a symlinked worktree: %q", got)
	}
}
