package power

import "sync"

// StartFunc takes the platform's assertion and returns what lets it go.
type StartFunc func() (stop func(), err error)

// Assertion keeps the machine from idling to sleep while anything holds it.
// It is counted: the first Hold takes the assertion and the last Release lets
// it go, so several runs in flight on one machine share one.
//
// What it does and does not do, on a Mac: it keeps an open or docked machine
// from idle sleep for as long as a run is in flight. It does not keep a
// closed-lid machine awake, on battery or otherwise, and nothing Rein can
// hold would; the run's clocks are measured in awake time for that case
// (see [Clock]). On other platforms it does nothing.
//
// A nil *Assertion is valid and does nothing.
type Assertion struct {
	start StartFunc
	logf  func(string, ...any)

	mu      sync.Mutex
	held    int
	stop    func()
	running bool // the assertion is in place now
	warned  bool // a failure to take it has been logged
}

// NewAssertion builds the platform's assertion, logging through logf. Off a
// Mac it holds nothing.
func NewAssertion(logf func(string, ...any)) *Assertion {
	return NewAssertionWith(platformStart, logf)
}

// NewAssertionWith builds an assertion on start, which may be nil for one
// that holds nothing. Tests use it to count without spawning anything.
func NewAssertionWith(start StartFunc, logf func(string, ...any)) *Assertion {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Assertion{start: start, logf: logf}
}

// Hold takes one hold on the assertion, taking the assertion itself if this
// is the first. A failure to take it is logged once and is not an error: the
// run goes ahead, measured in awake time either way.
func (a *Assertion) Hold() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held++
	if a.held > 1 || a.start == nil {
		return
	}
	stop, err := a.start()
	if err != nil {
		if !a.warned {
			a.warned = true
			a.logf("power: could not keep this machine awake for the run (%v); a sleep will pause the run instead", err)
		}
		return
	}
	a.stop, a.running = stop, true
}

// Release gives one hold back, letting the assertion go if it was the last.
// A Release without a Hold is ignored.
func (a *Assertion) Release() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held == 0 {
		return
	}
	a.held--
	if a.held > 0 || !a.running {
		return
	}
	a.stop()
	a.stop, a.running = nil, false
}

// Held reports whether the assertion is in place now.
func (a *Assertion) Held() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}
