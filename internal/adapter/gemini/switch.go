package gemini

import "sync/atomic"

// The Antigravity switch.
//
// Rein can drive two Google programs for a gemini queue: Gemini CLI (`gemini`)
// and Antigravity's command-line program (`agy`). Gemini CLI is the default
// and the only one Rein runs unless Elk's server says otherwise.
//
// Antigravity stays off until Google confirms in writing that Elk may run it
// on a person's own key (Casey, 2026-11-10; Elk #1755). Its help page says
// outside software driving Antigravity breaks its terms on consumer plans and
// recommends a key for third-party agents; whether Elk running it on a key is
// allowed is the question put to Google. So the switch lives on Elk's server,
// not in a release: the heartbeat reply's `antigravity` field turns it on, and
// Google's yes needs no new Rein.
//
// It fails closed. Off at start-up, off whenever a heartbeat reply does not
// say true, and while it is off Rein never executes `agy` at all — no version
// probe, no preflight, no run.

var antigravityOn atomic.Bool

// SetAntigravity records what Elk's server last said about the switch. The
// run loop calls it on every heartbeat reply; a reply that says nothing turns
// it off.
func SetAntigravity(on bool) { antigravityOn.Store(on) }

// AntigravityOn reports whether Elk's server has switched Antigravity on.
func AntigravityOn() bool { return antigravityOn.Load() }
