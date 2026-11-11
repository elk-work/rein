package runner

import "testing"

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
