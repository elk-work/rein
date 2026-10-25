package power

import (
	"context"
	"sync"
	"time"
)

// Wake is one sleep the watch noticed, reported once the machine is awake
// again.
type Wake struct {
	// At is the wall time the watch noticed it.
	At time.Time
	// Slept is how far wall time ran ahead of awake time since the watch's
	// previous reading: the length of the sleep, to within one interval.
	Slept time.Duration
}

const (
	// DefaultSleepInterval is how often the watch reads both clocks. A
	// maintenance wake lasts about 45 seconds, and the point of noticing is
	// to renew a run's lease inside it, so the watch has to notice in the
	// first few.
	DefaultSleepInterval = 2 * time.Second

	// DefaultSleepThreshold is the smallest gap the watch calls a sleep. Wall
	// time and awake time drift apart by milliseconds when NTP slews the
	// clock; a person setting the clock by hand can step it by more, which
	// reads as a sleep and costs one extra lease renewal.
	DefaultSleepThreshold = 10 * time.Second
)

// SleepWatch notices that the machine has slept by reading wall time and
// awake time together and seeing wall time run ahead. Rein is built without
// cgo, so IOKit's sleep and wake notifications are out of reach; the two
// clocks give the same answer within one interval, on macOS and Linux alike.
type SleepWatch struct {
	clock     Clock
	interval  time.Duration
	threshold time.Duration

	mu     sync.Mutex
	subs   map[int]chan Wake
	nextID int
	primed bool
	wall   time.Time // monotonic reading stripped
	awake  time.Duration
}

// NewSleepWatch builds a watch on c, or on [System] when c is nil.
func NewSleepWatch(c Clock) *SleepWatch {
	if c == nil {
		c = System
	}
	return &SleepWatch{
		clock:     c,
		interval:  DefaultSleepInterval,
		threshold: DefaultSleepThreshold,
		subs:      map[int]chan Wake{},
	}
}

// Subscribe returns a channel that receives every sleep the watch notices
// from now on, and the function that stops it. A subscriber that has not read
// the last wake when the next one comes gets one wake covering both.
func (w *SleepWatch) Subscribe() (<-chan Wake, func()) {
	ch := make(chan Wake, 1)
	w.mu.Lock()
	id := w.nextID
	w.nextID++
	w.subs[id] = ch
	w.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			w.mu.Lock()
			delete(w.subs, id)
			w.mu.Unlock()
		})
	}
}

// Observe reads both clocks once and reports the sleep since the previous
// reading, if there was one, telling every subscriber. The first reading only
// sets the baseline.
func (w *SleepWatch) Observe() (Wake, bool) {
	now := w.clock.Now()
	awake := w.clock.Awake()
	wall := now.Round(0) // wall time only: Sub must not use the monotonic reading

	w.mu.Lock()
	defer w.mu.Unlock()
	prevWall, prevAwake, primed := w.wall, w.awake, w.primed
	w.wall, w.awake, w.primed = wall, awake, true
	if !primed {
		return Wake{}, false
	}
	slept := wall.Sub(prevWall) - (awake - prevAwake)
	if slept < w.threshold {
		return Wake{}, false
	}
	wake := Wake{At: now, Slept: slept}
	for _, ch := range w.subs {
		select {
		case ch <- wake:
		default:
			// The subscriber has not read the last wake. Fold this one into
			// it; every send happens under w.mu, so once the old one is
			// taken out there is room.
			select {
			case old := <-ch:
				ch <- Wake{At: wake.At, Slept: old.Slept + wake.Slept}
			default:
				ch <- wake
			}
		}
	}
	return wake, true
}

// Run reads the clocks every interval until ctx is done.
func (w *SleepWatch) Run(ctx context.Context) {
	w.Observe()
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Observe()
		}
	}
}
