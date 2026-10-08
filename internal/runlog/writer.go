package runlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/secretenv"
)

// Writer appends one run's records and keeps its [Meta] current.
//
// It is safe for concurrent use, and every method is safe on a nil receiver:
// the run loop calls these unconditionally, and a runner with no log store is
// meant to take exactly the same path as one with a log.
//
// No method returns an error. A log is a convenience and a run is not: the
// first failure is kept for [Writer.Err] and everything afterwards is a
// no-op, so a full disk costs the log and nothing else.
type Writer struct {
	store *Store
	runID string

	mu     sync.Mutex
	f      *os.File
	seq    uint64
	meta   Meta
	err    error
	closed bool

	// redact replaces a scoped run's secret values in every record before it
	// reaches disk (ark:rein#48). Nil until the run loop resolves the secrets,
	// and for every queue that has none.
	redact *secretenv.Redactor
}

// Create opens a log for a run, truncating any log the same run id already
// had — a re-claimed run starts its account again rather than appending to a
// half-finished one from a runner that died.
//
// A nil store returns a nil writer, which is a working no-op.
func (s *Store) Create(meta Meta) *Writer {
	if s == nil || meta.RunID == "" {
		return nil
	}
	w := &Writer{store: s, runID: meta.RunID, meta: meta}
	if w.meta.StartedAt.IsZero() {
		w.meta.StartedAt = time.Now()
	}
	if w.meta.Status == "" {
		w.meta.Status = StatusRunning
	}
	if w.meta.PID == 0 {
		w.meta.PID = os.Getpid()
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		w.err = err
		return w
	}
	f, err := os.OpenFile(s.LogPath(meta.RunID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		w.err = err
		return w
	}
	w.f = f
	w.writeMeta()
	return w
}

// SetRedactor installs the redactor every later record passes through. The
// run loop sets it once a scoped run's secrets are resolved, before the agent
// starts, so no record the agent can influence is written without it.
func (w *Writer) SetRedactor(r *secretenv.Redactor) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.redact = r
}

// RunID is the run this log belongs to.
func (w *Writer) RunID() string {
	if w == nil {
		return ""
	}
	return w.runID
}

// Path is the log file, or "" for a no-op writer.
func (w *Writer) Path() string {
	if w == nil || w.store == nil {
		return ""
	}
	return w.store.LogPath(w.runID)
}

// Err is the first error this writer hit, if any. The run loop reports it once
// rather than per record.
func (w *Writer) Err() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Event records one [adapter.Event].
func (w *Writer) Event(ev adapter.Event) {
	if w == nil {
		return
	}
	rec := AgentRecord(ev)
	if ev.Kind == adapter.EventDone && ev.Result != nil && ev.Result.SessionID != "" {
		w.Update(func(m *Meta) { m.SessionID = ev.Result.SessionID })
	}
	w.Write(rec)
}

// Runner records one of Rein's own lifecycle records.
func (w *Writer) Runner(kind Kind, format string, args ...any) {
	if w == nil {
		return
	}
	w.Write(RunnerRecord(kind, format, args...))
}

// Write appends a record, stamping its sequence number and, where the caller
// left it zero, its timestamp.
func (w *Writer) Write(rec Record) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil || w.err != nil || w.closed {
		return
	}
	w.seq++
	rec.Seq = w.seq
	rec.RunID = w.runID
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	if rec.Source == "" {
		rec.Source = SourceRunner
	}
	// Encoded whole and redacted as bytes, so every field — text, a tool's
	// input, an error, the vendor's raw event — passes the same check, and a
	// field added later cannot forget to.
	line, err := json.Marshal(rec)
	if err != nil {
		w.err = err
		return
	}
	line = append(w.redact.Bytes(line), '\n')
	if _, err := w.f.Write(line); err != nil {
		w.err = err
	}
}

// Update edits the meta and rewrites it.
//
// Meta is written on change rather than on every record: the dynamic columns a
// live view shows — elapsed, last event, tokens — are all derivable from the
// log itself, so this file only has to carry the facts that are not, and those
// change a handful of times per run.
func (w *Writer) Update(fn func(*Meta)) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return
	}
	fn(&w.meta)
	w.writeMeta()
}

// Meta returns the current meta.
func (w *Writer) Meta() Meta {
	if w == nil {
		return Meta{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.meta
}

// Close marks the run finished in the given status and closes the log. It is
// idempotent; the first status wins, because the first thing to settle a run
// is the thing that settled it.
func (w *Writer) Close(status Status) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	w.meta.Status = status
	if w.meta.EndedAt.IsZero() {
		w.meta.EndedAt = time.Now()
	}
	w.writeMeta()
	if w.f != nil {
		if err := w.f.Close(); err != nil && w.err == nil {
			w.err = err
		}
		w.f = nil
	}
}

// writeMeta rewrites the meta file. The caller holds w.mu.
//
// Write-and-rename rather than truncate-and-write: `rein tail` reads this file
// on every refresh, and a reader that catches a truncated file mid-write would
// see a run vanish and come back.
func (w *Writer) writeMeta() {
	if w.store == nil {
		return
	}
	if err := writeMetaFile(w.store.MetaPath(w.runID), w.meta); err != nil && w.err == nil {
		w.err = err
	}
}

func writeMetaFile(path string, meta Meta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".meta-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}
