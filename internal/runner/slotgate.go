package runner

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultSlotInterval is how often a queue waiting for a slot asks again. Short
// enough that memory freed by a finishing run is noticed promptly, long enough
// that the measurement is not the workload.
const DefaultSlotInterval = 5 * time.Second

// slotPriority orders the claims waiting for a slot on this machine. A higher
// priority goes first; equal priorities go in the order they started waiting.
//
// Until ark:rein#60 there was no order at all. Every waiting queue re-measured
// on its own five-second clock and whichever happened to ask first after a run
// finished took the slot, so on a machine held to one slot the Wrangler cycle —
// the run that reviews, deploys and unsticks everybody else's — waited an hour
// and a half on 2026-10-06 while the slot went to seven build runs in turn.
type slotPriority int

const (
	// prioBuild is an ordinary claim: the next run on a queue that builds.
	prioBuild slotPriority = iota

	// prioRevision is a run this machine already holds, which gave its slot
	// back while Elk's review pass thought about it and has now been reopened
	// with revisions to make. It has a lease Rein is keeping alive and a
	// worktree on disk, so it goes ahead of anything not yet claimed — but
	// behind a Wrangler cycle, which is the one claim allowed to jump a build.
	prioRevision

	// prioWrangler is a claim on a Wrangler queue (`wrangler = true`): it takes
	// the next free slot ahead of every build claim on the machine. See
	// [queueRunner.claimPriority] for why that is by queue rather than by run,
	// and how a Wrangler queue that is also a build queue is kept from
	// starving the others.
	prioWrangler
)

func (p slotPriority) String() string {
	switch p {
	case prioWrangler:
		return "Wrangler"
	case prioRevision:
		return "revision"
	}
	return "build"
}

// slotRequest is one queue asking for a slot.
type slotRequest struct {
	queue string
	prio  slotPriority
}

// slotWaiter is a request in line. seq is its place in arrival order.
type slotWaiter struct {
	slotRequest
	seq   uint64
	since time.Time

	// reason is the last thing the gate said it was waiting on, carried into
	// the hold so the run that finally gets the slot can say what it waited
	// for.
	reason string
}

// ahead reports whether w goes before o.
func (w *slotWaiter) ahead(o *slotWaiter) bool {
	if w.prio != o.prio {
		return w.prio > o.prio
	}
	return w.seq < o.seq
}

// slotHold is one slot, held by one queue's run.
type slotHold struct {
	g     *slotGate
	queue string
	prio  slotPriority
	since time.Time

	// grant is this hold's number in the gate's sequence of grants: a grant
	// after it is evidence another claim has had a turn since.
	grant uint64

	// waited is how long the claim stood in line for this slot, and waitedOn
	// what it was waiting on when it last asked. Zero and empty when the slot
	// was free on the first ask.
	waited   time.Duration
	waitedOn string

	// runID and released are guarded by g.mu: the telemetry tick and other
	// queues' wait reasons read them.
	runID    string
	released bool
}

// setRun names the run this slot is serving, once it has been claimed — so
// that a queue waiting behind it can say what it is waiting behind.
func (h *slotHold) setRun(id string) {
	if h == nil {
		return
	}
	h.g.mu.Lock()
	h.runID = id
	h.g.mu.Unlock()
}

// isReleased reports whether the slot has been given back.
func (h *slotHold) isReleased() bool {
	h.g.mu.Lock()
	defer h.g.mu.Unlock()
	return h.released
}

// release gives the slot back. It is safe to call more than once and on nil,
// because a run under review gives its slot back before claimAndDrive's
// deferred release runs.
func (h *slotHold) release() {
	if h == nil {
		return
	}
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	if h.released {
		return
	}
	h.released = true
	for i, o := range g.holds {
		if o == h {
			g.holds = append(g.holds[:i], g.holds[i+1:]...)
			break
		}
	}
}

// slotGate is the concurrency limit, recomputed rather than fixed, and the
// line the claims waiting on it stand in.
//
// It replaces a buffered channel sized once at startup. A channel can only
// express the ceiling; this has to express "four is allowed and one is
// affordable", which is a different number every few minutes on a machine
// somebody is also using — and, since ark:rein#60, who goes next.
type slotGate struct {
	mu sync.Mutex
	// last is the slot count as of the most recent measurement, and the state
	// behind "log it when it changes" — 0 means nothing has been measured.
	last int

	holds   []*slotHold
	waiting []*slotWaiter
	seq     uint64
	grants  uint64

	hard    int
	workDir string
	probe   HeadroomFunc
	logf    func(string, ...any)
	retry   time.Duration
}

// join puts a request in line.
func (g *slotGate) join(req slotRequest) *slotWaiter {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seq++
	w := &slotWaiter{slotRequest: req, seq: g.seq, since: time.Now()}
	g.waiting = append(g.waiting, w)
	return w
}

// quit takes a request out of line, granted or not.
func (g *slotGate) quit(w *slotWaiter) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, o := range g.waiting {
		if o == w {
			g.waiting = append(g.waiting[:i], g.waiting[i+1:]...)
			return
		}
	}
}

// enter takes a slot for req, waiting until the machine can afford one and
// every claim ahead of it in line has had its turn. It returns false only when
// ctx is done.
//
// onWait, when not nil, is told what the claim is waiting on each time the
// gate refuses it — the sentence a person reading Elk needs instead of
// silence (ark:rein#59). It is called with the gate unlocked.
//
// Waiting rather than skipping the tick is deliberate: a queue that gave up on
// a full machine would report "nothing waiting to claim" and, under --once,
// exit having done nothing — indistinguishable in the log from an empty queue.
func (g *slotGate) enter(ctx context.Context, req slotRequest, onWait func(reason string)) (*slotHold, bool) {
	w := g.join(req)
	defer g.quit(w)
	for {
		if h, reason := g.tryEnter(w); h != nil {
			return h, true
		} else if onWait != nil {
			onWait(reason)
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(g.retry):
		}
	}
}

// tryEnter grants w a slot if the machine can afford one more run AND nothing
// ahead of w in line is still waiting for one. Otherwise it says why not.
//
// "Ahead" is what makes the order real: a build claim that measures a free
// slot a moment before the Wrangler's own five-second clock comes round does
// not get to take it, because the Wrangler is in line ahead of it and the slot
// is counted as the Wrangler's.
func (g *slotGate) tryEnter(w *slotWaiter) (*slotHold, string) {
	n, why := Slots(g.probe(g.workDir), g.hard)

	g.mu.Lock()
	defer g.mu.Unlock()
	if n != g.last {
		g.logf("concurrency: %s", why)
		g.last = n
	}
	var ahead []*slotWaiter
	for _, o := range g.waiting {
		if o != w && o.ahead(w) {
			ahead = append(ahead, o)
		}
	}
	if len(g.holds)+len(ahead) >= n {
		w.reason = g.reasonLocked(why, ahead)
		return nil, w.reason
	}
	g.grants++
	now := time.Now()
	h := &slotHold{
		g: g, queue: w.queue, prio: w.prio, since: now, grant: g.grants,
		waited: now.Sub(w.since), waitedOn: w.reason,
	}
	g.holds = append(g.holds, h)
	return h, ""
}

// slotWaitPrefix opens every slot-wait reason.
const slotWaitPrefix = "waiting for a slot on this machine: "

// reasonLocked is the sentence for a claim that cannot have a slot yet: how
// many slots the machine allows and why, who holds them, and who is in line
// ahead. Every part of it is Rein's own — no error text, no path, nothing a
// vendor wrote — because it rides the heartbeat to Elk.
func (g *slotGate) reasonLocked(why string, ahead []*slotWaiter) string {
	var b strings.Builder
	b.WriteString(slotWaitPrefix)
	b.WriteString(why)
	if len(g.holds) > 0 {
		now := time.Now()
		parts := make([]string, 0, len(g.holds))
		for _, h := range g.holds {
			p := h.queue
			if h.runID != "" {
				p += " run " + h.runID
			}
			p += " for " + span(now.Sub(h.since))
			parts = append(parts, p)
		}
		b.WriteString("; in use by ")
		b.WriteString(strings.Join(parts, ", "))
	}
	if len(ahead) > 0 {
		sort.Slice(ahead, func(i, j int) bool { return ahead[i].ahead(ahead[j]) })
		parts := make([]string, 0, len(ahead))
		for _, o := range ahead {
			p := o.queue
			if o.prio != prioBuild {
				p += " (" + o.prio.String() + ")"
			}
			parts = append(parts, p)
		}
		b.WriteString("; next in line ")
		b.WriteString(strings.Join(parts, ", "))
	}
	return b.String()
}

// grantCount is how many slots the gate has handed out since it started.
func (g *slotGate) grantCount() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.grants
}

// inFlight is how many runs hold a slot right now.
func (g *slotGate) inFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.holds)
}

// span renders a duration the way a person reads one in a sentence: "40s",
// "25m", "1h12m".
func span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%02dm", h, m)
}
