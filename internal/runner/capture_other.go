//go:build !darwin

package runner

import "github.com/elk-work/rein/internal/adapter"

// giveCapture is a no-op off macOS. There is no screencapture to stand in
// front of, and a page is still `rein capture site` through chrome.
func (qr *queueRunner) giveCapture(spec *adapter.RunSpec) func() {
	return func() {}
}
