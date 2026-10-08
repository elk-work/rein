package tail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/elk-work/rein/internal/runlog"
)

// Defaults for the live view.
const (
	// DefaultInterval is how often the logs are re-read. Two and a half
	// repaints a second reads as live and costs a stat and a short read per
	// run; anything faster is spending CPU on frames nobody can follow.
	DefaultInterval = 400 * time.Millisecond

	// DefaultWidth is the fallback when the terminal will not say how wide it
	// is — a pipe, or a platform with no probe.
	DefaultWidth = 100

	// DefaultSince is how much of a LIVE run's history is replayed before
	// following it. A run that has been going for an hour should not paste an
	// hour of scrollback at somebody who just wanted to see what it is doing.
	// A run that has already finished ignores this and replays in full, which
	// is the only reading of `--since` that shows anything at all on a run
	// that ended before the window opened.
	DefaultSince = 2 * time.Minute
)

// Options configure [Run].
type Options struct {
	// Store is the run-log directory to read.
	Store *runlog.Store

	// Ref selects what to follow: a run id, a unique run-id prefix, or a queue
	// name. Empty means every active run on this machine — the all-queues
	// view.
	Ref string

	// Since is how much history to replay before following. See
	// [DefaultSince]; SinceSet distinguishes "the user asked for a window"
	// from "nobody said", which is what lets a finished run replay in full.
	Since    time.Duration
	SinceSet bool

	// Raw prints each record as the log's own JSON, one per line, instead of
	// rendering it.
	Raw bool

	// Interval overrides [DefaultInterval].
	Interval time.Duration

	// Out is where the view is drawn.
	Out io.Writer

	// TTY enables the in-place drawing. False gives plain line output.
	TTY bool

	// Width is the terminal width. Zero means [DefaultWidth].
	Width int

	// Color enables the ANSI attributes.
	Color bool

	// Now is the clock, replaced by tests.
	Now func() time.Time
}

func (o *Options) applyDefaults() {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Width <= 0 {
		o.Width = DefaultWidth
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// follow is one run being watched.
type follow struct {
	meta   runlog.Meta
	tailer *runlog.Tailer
	state  RunState
	cutoff time.Time
	opened bool
	// ended is set once the run's meta says it finished AND its log has been
	// read to the end, which is the only point at which there is nothing more
	// to show.
	ended bool
}

// Run draws the live view until the context is cancelled, or — when following
// a named run that has already finished — until its log has been replayed.
//
// The loop is a poll: every tick it re-reads the metas, reads whatever has been
// appended to each log, renders it above the status block, and repaints the
// block. Polling rather than watching the filesystem is deliberate — a stat
// and a short read cost nothing at this interval, the behaviour is identical
// on all three platforms Rein ships to, and the alternative is a
// filesystem-notification dependency for a screen that repaints twice a second
// anyway.
func Run(ctx context.Context, o Options) error {
	o.applyDefaults()
	if o.Store == nil {
		return errors.New("tail: no run-log store")
	}

	watch := o.Ref == ""
	sc := newScreen(o.Out, o.TTY, o.Width)
	defer sc.Close()

	follows := map[string]*follow{}
	if !watch {
		metas, err := o.Store.Lookup(o.Ref)
		if err != nil {
			return err
		}
		for _, m := range metas {
			follows[m.RunID] = o.newFollow(m)
		}
		if len(follows) == 0 {
			return fmt.Errorf("%w: %q", runlog.ErrNotFound, o.Ref)
		}
	} else if err := o.announceIdle(sc); err != nil {
		return err
	}

	ticker := time.NewTicker(o.Interval)
	defer ticker.Stop()

	for first := true; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}

		if watch {
			if err := o.discover(follows); err != nil {
				return err
			}
		} else {
			o.refresh(follows)
		}

		ordered := order(follows)
		prefix := len(ordered) > 1
		progressed := false
		for _, f := range ordered {
			lines, moved, err := o.drain(f, prefix)
			if err != nil {
				return err
			}
			progressed = progressed || moved
			sc.Emit(lines...)
		}
		sc.SetStatus(o.status(ordered, prefix))

		if watch {
			continue
		}
		// A named follow is finished when every run it named has settled and
		// its log has been read to the end. One quiet tick after the last
		// record is the check: the meta is written before the final records
		// are flushed often enough that trusting the status alone truncates
		// the last thing the agent said.
		if allEnded(ordered) && !progressed {
			sc.Close()
			if !o.Raw {
				now := o.Now()
				for _, f := range ordered {
					fmt.Fprintln(o.Out, o.renderer(false).Total(f.meta, f.state, now))
				}
			}
			return nil
		}
	}
}

func (o Options) renderer(prefix bool) Renderer {
	return Renderer{Color: o.Color, Width: o.Width, Prefix: prefix}
}

func (o Options) newFollow(m runlog.Meta) *follow {
	f := &follow{meta: m, tailer: o.Store.Tail(m.RunID)}
	// A live run replays a window; a finished one replays in full, because a
	// window measured from now cannot reach a run that ended an hour ago.
	window := o.Since
	if !o.SinceSet {
		window = 0
		if m.Status.Active() {
			window = DefaultSince
		}
	}
	f.cutoff = runlog.SinceCutoff(o.Now(), window)
	return f
}

// discover adds runs that have started since the last tick and keeps the ones
// already being followed, so a run that finishes mid-view still has its last
// records read rather than disappearing between two frames.
func (o Options) discover(follows map[string]*follow) error {
	metas, err := o.Store.List()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range metas {
		seen[m.RunID] = true
		if f, ok := follows[m.RunID]; ok {
			f.meta = m
			continue
		}
		if m.Status.Active() {
			follows[m.RunID] = o.newFollow(m)
		}
	}
	for id, f := range follows {
		if !seen[id] {
			// Pruned out from under us, or the store was cleared.
			delete(follows, id)
			continue
		}
		if f.ended && !f.meta.Status.Active() {
			// Finished, drained, and its last line is on screen. Drop it from
			// the block rather than leaving a dead row in the live view.
			delete(follows, id)
		}
	}
	return nil
}

func (o Options) refresh(follows map[string]*follow) {
	for id, f := range follows {
		if m, err := o.Store.ReadMeta(id); err == nil {
			f.meta = m
		}
	}
}

// drain reads whatever a run's log has gained and renders it. moved reports
// whether anything new arrived, which is how a named follow knows the log has
// gone quiet.
func (o Options) drain(f *follow, prefix bool) (lines []string, moved bool, err error) {
	recs, err := f.tailer.Next()
	if err != nil {
		return nil, false, err
	}
	r := o.renderer(prefix)
	for _, rec := range recs {
		// Everything is observed, including records older than the window: a
		// token total that started counting halfway through the run would be
		// wrong, and the point of the column is the total.
		f.state.Observe(rec)
		if !f.cutoff.IsZero() && rec.At.Before(f.cutoff) {
			continue
		}
		if o.Raw {
			if data, mErr := json.Marshal(rec); mErr == nil {
				lines = append(lines, string(data))
			}
			continue
		}
		lines = append(lines, r.Lines(rec)...)
	}
	if len(recs) > 0 {
		f.opened = true
	}
	f.ended = !f.meta.Status.Active() && len(recs) == 0 && f.opened
	return lines, len(recs) > 0, nil
}

func (o Options) status(follows []*follow, prefix bool) []string {
	if o.Raw {
		return nil
	}
	now := o.Now()
	r := o.renderer(false)
	if len(follows) == 1 && !prefix {
		return []string{r.Footer(follows[0].meta, follows[0].state, now)}
	}
	rows := make([]DashboardRow, 0, len(follows))
	for _, f := range follows {
		rows = append(rows, DashboardRow{Meta: f.meta, State: f.state})
	}
	return r.Dashboard(rows, now)
}

// announceIdle says, once, that there is nothing in flight and points at the
// last run there was. A live view that opens on an empty screen looks broken;
// the useful thing to say is "nothing now, and here is how to look at the last
// one".
func (o Options) announceIdle(sc *screen) error {
	all, err := o.Store.List()
	if err != nil {
		return err
	}
	for _, m := range all {
		if m.Status.Active() {
			return nil
		}
	}
	if len(all) == 0 {
		sc.Emit("no runs recorded yet — this machine has not driven one since the log was added")
		return nil
	}
	last := all[0]
	sc.Emit(fmt.Sprintf("no runs in flight. The last was %s on %s (%s, %s ago) — `rein tail %s` replays it.",
		runlog.Short(last.RunID), orDefault(last.Queue, "an unnamed queue"), last.Status,
		shortDuration(o.Now().Sub(last.EndedAt)), runlog.Short(last.RunID)))
	return nil
}

func order(follows map[string]*follow) []*follow {
	out := make([]*follow, 0, len(follows))
	for _, f := range follows {
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].meta.StartedAt.Equal(out[j].meta.StartedAt) {
			return out[i].meta.RunID < out[j].meta.RunID
		}
		return out[i].meta.StartedAt.Before(out[j].meta.StartedAt)
	})
	return out
}

func allEnded(follows []*follow) bool {
	for _, f := range follows {
		if f.meta.Status.Active() {
			return false
		}
	}
	return true
}

// Terminal reports whether f is a terminal, and how wide it is.
//
// The width probe is per platform (term_unix.go, term_windows.go); the
// terminal test is not, because os.Stat already answers it everywhere Go runs.
func Terminal(f *os.File) (tty bool, width int) {
	if f == nil {
		return false, DefaultWidth
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false, DefaultWidth
	}
	enableVirtualTerminal(f)
	if w, _, ok := termSize(f); ok {
		return true, w
	}
	return true, DefaultWidth
}
