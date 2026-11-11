//go:build !windows

package gemini

// sandboxUnsupported is "" on macOS and Linux, where Antigravity's terminal
// sandbox runs. Rein drives agy 1.5.5 and later only (agyMinVersion).
func sandboxUnsupported() string { return "" }
