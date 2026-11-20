package runner

import "github.com/elk-work/rein/internal/control"

// DrainForTest stops the runner claiming, the way an installed upgrade does.
func (r *Runner) DrainForTest(why string) { r.drainFor(why) }

// StatusForTest is what `rein status` is told over the control plane.
func (r *Runner) StatusForTest() control.Status { return r.status() }
