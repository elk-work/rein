package gemini

import (
	"sync"
	"time"
)

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
// It fails closed. Each gemini queue's heartbeat reply is that queue's say,
// and a say counts for [SwitchFresh]. The switch is on only while at least one
// say counts and every say that counts is "on": a reply without the field
// turns it off, two Elk servers that disagree leave it off, and heartbeats
// that keep failing let it lapse to off. While it is off Rein never executes `agy` at all — no
// version probe, no preflight, no run. A session already running on
// Antigravity when it turns off runs to its end; it cannot be continued.

// SwitchFresh is how long one queue's "on" counts without a new heartbeat:
// three of Rein's five-minute heartbeats.
const SwitchFresh = 15 * time.Minute

type vote struct {
	on bool
	at time.Time
}

type switchState struct {
	mu    sync.Mutex
	votes map[string]vote
	last  bool   // the last answer settle gave
	gen   uint64 // bumped whenever that answer changes
	now   func() time.Time
}

var sw switchState

func (s *switchState) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *switchState) settle() bool {
	on, now := false, s.clock()
	for _, v := range s.votes {
		if now.Sub(v.at) > SwitchFresh {
			continue // lapsed: a queue that stopped, or cannot reach Elk
		}
		if !v.on {
			on = false
			break
		}
		on = true
	}
	if on != s.last {
		s.last = on
		s.gen++
	}
	return on
}

// SetAntigravity records what one queue's Elk server said in its heartbeat
// reply; a reply that says nothing is false.
func SetAntigravity(queue string, on bool) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.votes == nil {
		sw.votes = map[string]vote{}
	}
	sw.votes[queue] = vote{on: on, at: sw.clock()}
	sw.settle()
}

// ResetAntigravity forgets every queue's say, which turns the switch off.
// For tests.
func ResetAntigravity() {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.votes = nil
	sw.settle()
}

// AntigravityOn reports whether Antigravity is switched on: at least one
// queue's say counts, and every say that counts is "on".
func AntigravityOn() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.settle()
}

// SwitchGeneration changes whenever AntigravityOn's answer does, so a reading
// that names the program (the queue's program version) can tell it is stale.
func SwitchGeneration() uint64 {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.settle()
	return sw.gen
}
