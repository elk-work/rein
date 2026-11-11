//go:build !windows

package gemini

// sandboxUnsupported is "" on macOS and Linux, where Antigravity's terminal
// sandbox runs (agy 1.3.5 and later).
func sandboxUnsupported() string { return "" }
