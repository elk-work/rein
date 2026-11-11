package runner

import (
	"strings"
	"time"
	"unicode/utf8"
)

// What task live step 2 adds to the run loop (scout spec
// 2026-11-11-task-live-step-2-design.md, sections 6-8).

// vendorNoise is progress the Claude adapter makes out of stream-json it has
// no kind for. It stays in the local log and never reaches Elk.
func vendorNoise(text string) bool {
	return strings.HasPrefix(text, "claude event ") || strings.HasPrefix(text, "claude system/")
}

const (
	// replyWindow is how long after a person's message goes into the session
	// the agent's text is sent as it comes, rather than with the next receipt.
	replyWindow = 3 * time.Minute
	// sayCoalesce gathers text arriving close together into one say.
	sayCoalesce = 500 * time.Millisecond
	// sayMax is Elk's cap on one event body.
	sayMax = 4000
)

// inReplyWindow reports whether now is inside the window a delivered message
// opened at opened.
func inReplyWindow(opened, now time.Time) bool {
	return !opened.IsZero() && now.Sub(opened) < replyWindow
}

// sayParts cuts the agent's text into bodies of at most sayMax bytes, on rune
// boundaries, so none is truncated. Blank text is no parts.
func sayParts(text string) []string {
	text = strings.TrimSpace(text)
	var out []string
	for len(text) > sayMax {
		cut := sayMax
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}
