package runlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DirName is the run-log directory inside the Rein home's log directory:
// ~/.rein/log/runs.
const DirName = "runs"

// DefaultKeep is how many runs' logs are kept. Fifty is roughly a fortnight
// of a busy single-machine fleet and a few megabytes; the point of the bound
// is that nothing here is ever pruned by hand.
const DefaultKeep = 50

// logExt and metaExt are the two files a run leaves behind.
const (
	logExt  = ".jsonl"
	metaExt = ".meta.json"
)

// ErrNotFound reports that no run matched a reference.
var ErrNotFound = errors.New("runlog: no such run")

// Store is one directory of run logs.
//
// Every method is safe on a nil *Store — a runner with logging switched off
// holds nil and takes the same code path as one with a log.
type Store struct {
	// Dir is the directory holding <run>.jsonl and <run>.meta.json.
	Dir string
}

// Open returns the store under a Rein home, creating the directory. It sits
// beside the daemon's own rein.log, under log/, because everything a person
// would go looking for when a run misbehaves should be in one place.
func Open(home string) (*Store, error) {
	if home == "" {
		return nil, errors.New("runlog: no Rein home")
	}
	dir := filepath.Join(home, "log", DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("runlog: create %s: %w", dir, err)
	}
	return &Store{Dir: dir}, nil
}

// LogPath is where a run's records are.
func (s *Store) LogPath(runID string) string {
	if s == nil {
		return ""
	}
	return filepath.Join(s.Dir, runID+logExt)
}

// MetaPath is where a run's meta is.
func (s *Store) MetaPath(runID string) string {
	if s == nil {
		return ""
	}
	return filepath.Join(s.Dir, runID+metaExt)
}

// List returns every run the store holds, newest first.
//
// A meta file that will not parse is skipped rather than failing the listing:
// one interrupted write must not make `rein tail` refuse to show the other
// forty-nine runs.
func (s *Store) List() ([]Meta, error) {
	if s == nil {
		return nil, nil
	}
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runlog: read %s: %w", s.Dir, err)
	}
	var out []Meta
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, metaExt) {
			continue
		}
		m, err := s.ReadMeta(strings.TrimSuffix(name, metaExt))
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// Active returns the runs still being driven, newest first.
func (s *Store) Active() ([]Meta, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, m := range all {
		if m.Status.Active() {
			out = append(out, m)
		}
	}
	return out, nil
}

// ReadMeta reads one run's meta.
func (s *Store) ReadMeta(runID string) (Meta, error) {
	if s == nil {
		return Meta{}, ErrNotFound
	}
	data, err := os.ReadFile(s.MetaPath(runID))
	if errors.Is(err, os.ErrNotExist) {
		return Meta{}, fmt.Errorf("%w: %s", ErrNotFound, runID)
	}
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, fmt.Errorf("runlog: parse %s: %w", s.MetaPath(runID), err)
	}
	if m.RunID == "" {
		m.RunID = runID
	}
	return m, nil
}

// Lookup resolves a reference a person typed to the runs it means.
//
// In order: an exact run id, then a unique run-id prefix, then every run on a
// queue of that name. A prefix that matches more than one run is an error
// rather than a guess — following the wrong run is worse than being asked to
// type four more characters.
func (s *Store) Lookup(ref string) ([]Meta, error) {
	if s == nil || ref == "" {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, ref)
	}
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var prefix, queue []Meta
	for _, m := range all {
		switch {
		case m.RunID == ref:
			return []Meta{m}, nil
		case strings.HasPrefix(m.RunID, ref):
			prefix = append(prefix, m)
		}
		if m.Queue == ref {
			queue = append(queue, m)
		}
	}
	switch {
	case len(prefix) == 1:
		return prefix, nil
	case len(prefix) > 1:
		ids := make([]string, 0, len(prefix))
		for _, m := range prefix {
			ids = append(ids, Short(m.RunID))
		}
		return nil, fmt.Errorf("runlog: %q matches %d runs (%s) — say which",
			ref, len(prefix), strings.Join(ids, ", "))
	case len(queue) > 0:
		return queue, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrNotFound, ref)
}

// Records reads a whole log. A truncated final line — the log of a run still
// being written — is dropped rather than reported, because a partial line is
// the normal state of a live log and not a corruption.
func (s *Store) Records(runID string) ([]Record, error) {
	if s == nil {
		return nil, ErrNotFound
	}
	f, err := os.Open(s.LogPath(runID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, runID)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	recs, _, err := readFrom(f, 0)
	return recs, err
}

// Tailer reads a run's log forward, remembering how far it got.
//
// It polls rather than watching the filesystem: a poll is a stat and a read of
// whatever is new, it behaves identically on all three platforms Rein ships
// to, and the alternative is a filesystem-notification dependency for a screen
// that repaints twice a second anyway.
type Tailer struct {
	path   string
	offset int64
}

// Tail starts reading a run's log at the beginning.
func (s *Store) Tail(runID string) *Tailer {
	return &Tailer{path: s.LogPath(runID)}
}

// Next returns whatever has been appended since the last call. A log that does
// not exist yet is not an error — a run is claimed before its first record is
// written, and a live view should show the run rather than a failure.
func (t *Tailer) Next() ([]Record, error) {
	f, err := os.Open(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// A log that shrank was truncated under us — a re-claimed run starting its
	// account again. Start over rather than reading from a stale offset into
	// the middle of a line.
	if fi, err := f.Stat(); err == nil && fi.Size() < t.offset {
		t.offset = 0
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, err
	}
	recs, n, err := readFrom(f, t.offset)
	t.offset = n
	return recs, err
}

// readFrom decodes complete lines from r, returning the offset just past the
// last complete one. Anything after it is a partial write and is left for the
// next read.
func readFrom(r io.Reader, start int64) ([]Record, int64, error) {
	var (
		out []Record
		at  = start
		br  = bufio.NewReaderSize(r, 64*1024)
	)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			at += int64(len(line))
			trimmed := strings.TrimSpace(string(line))
			if trimmed == "" {
				continue
			}
			var rec Record
			if jsonErr := json.Unmarshal([]byte(trimmed), &rec); jsonErr == nil {
				out = append(out, rec)
			}
			// A line that will not parse is skipped in silence. The only way
			// to produce one is a partial write from a killed process, and
			// refusing to show the rest of the log over it helps nobody.
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, at, nil
			}
			return out, at, err
		}
	}
}

// Prune keeps the newest keep runs and removes the rest, both files each.
//
// It runs where the worktree reaper runs — at the end of a run — for the same
// reason the reaper does: the cheapest moment to tidy up after a run is the
// moment one finishes, and nothing else is going to remember. A run still
// being driven is never pruned however old it is, because the log of a live
// run is the one thing a live view is reading.
func (s *Store) Prune(keep int) (int, error) {
	if s == nil {
		return 0, nil
	}
	if keep <= 0 {
		keep = DefaultKeep
	}
	all, err := s.List()
	if err != nil {
		return 0, err
	}
	var candidates []Meta
	for _, m := range all {
		if !m.Status.Active() {
			candidates = append(candidates, m)
		}
	}
	// Newest first, so everything past the budget is the oldest. Active runs
	// are counted against the budget but never removed.
	budget := keep - (len(all) - len(candidates))
	if budget < 0 {
		budget = 0
	}
	if len(candidates) <= budget {
		return 0, nil
	}
	var removed int
	var errs []error
	for _, m := range candidates[budget:] {
		for _, p := range []string{s.LogPath(m.RunID), s.MetaPath(m.RunID)} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// Totals sums the usage increments in a slice of records — what a live view's
// token column shows.
func Totals(recs []Record) (in, out int64, model string) {
	for _, r := range recs {
		if r.Usage == nil {
			continue
		}
		in += r.Usage.InputTokens
		out += r.Usage.OutputTokens
		if r.Usage.Model != "" {
			model = r.Usage.Model
		}
	}
	return in, out, model
}

// SinceCutoff is the timestamp a --since window starts at. A zero or negative
// window means everything.
func SinceCutoff(now time.Time, window time.Duration) time.Time {
	if window <= 0 {
		return time.Time{}
	}
	return now.Add(-window)
}
