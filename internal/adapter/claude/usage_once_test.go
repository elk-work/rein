package claude

import (
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

// ark:rein#62: Claude Code writes one assistant line per content block, each
// carrying the whole message's usage. tools.jsonl has one such message — a text
// block and a tool call, two lines, the same id and the same counts — and its
// usage must be recorded once.
func TestUsageIsRecordedOncePerMessageNotPerBlock(t *testing.T) {
	got := replay(t, "tools.jsonl")

	var sum adapter.Usage
	n := 0
	for _, ev := range got.events {
		if ev.Kind == adapter.EventUsage && ev.Usage != nil {
			sum.Add(*ev.Usage)
			n++
		}
	}
	// Three distinct messages in the fixture, four assistant lines.
	if n != 3 {
		t.Fatalf("usage events = %d, want 3 (one per message, not one per line)", n)
	}
	// The per-message counts from testdata/tools.jsonl, each message once.
	want := adapter.Usage{
		InputTokens:      2 + 56 + 56,
		OutputTokens:     2 + 17 + 2,
		CacheReadTokens:  10138 + 28093 + 28237,
		CacheWriteTokens: 17955 + 144 + 259,
	}
	sum.Model = ""
	if sum != want {
		t.Fatalf("summed usage = %+v, want %+v", sum, want)
	}
}

// A later line of the same message that carries a grown count records only
// what it adds; one with no id is recorded as it stands, as before.
func TestUsageIncrementRecordsOnlyWhatALaterLineAdds(t *testing.T) {
	d := &decoder{}
	first := tokenUsage{InputTokens: 5, OutputTokens: 2, CacheReadInputTokens: 1000}
	if inc, ok := d.usageIncrement("msg_a", first); !ok || inc != first {
		t.Fatalf("first line = %+v, %v; want it whole", inc, ok)
	}
	if _, ok := d.usageIncrement("msg_a", first); ok {
		t.Fatal("an identical repeat was recorded again")
	}
	grown := first
	grown.OutputTokens = 40
	inc, ok := d.usageIncrement("msg_a", grown)
	if !ok || inc != (tokenUsage{OutputTokens: 38}) {
		t.Fatalf("grown line = %+v, %v; want only the 38 output tokens it adds", inc, ok)
	}
	if _, ok := d.usageIncrement("msg_a", first); ok {
		t.Fatal("a line with smaller counts than already recorded was recorded")
	}
	if inc, ok := d.usageIncrement("msg_b", first); !ok || inc != first {
		t.Fatalf("a new message = %+v, %v; want it whole", inc, ok)
	}
	for i := 0; i < 2; i++ {
		if inc, ok := d.usageIncrement("", first); !ok || inc != first {
			t.Fatalf("a line with no id = %+v, %v; want it whole every time", inc, ok)
		}
	}
}
