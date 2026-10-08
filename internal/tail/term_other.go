//go:build !darwin && !linux && !windows

package tail

import "os"

// termSize has no answer on a platform Rein has no probe for. The caller falls
// back to [DefaultWidth], which is a worse guess than the truth and a better
// one than assuming infinity.
func termSize(*os.File) (width, height int, ok bool) { return 0, 0, false }

func enableVirtualTerminal(*os.File) {}
