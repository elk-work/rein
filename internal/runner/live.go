package runner

import "strings"

// What task live step 2 adds to the run loop (scout spec
// 2026-11-11-task-live-step-2-design.md, sections 6-8).

// vendorNoise is progress the Claude adapter makes out of stream-json it has
// no kind for. It stays in the local log and never reaches Elk.
func vendorNoise(text string) bool {
	return strings.HasPrefix(text, "claude event ") || strings.HasPrefix(text, "claude system/")
}
