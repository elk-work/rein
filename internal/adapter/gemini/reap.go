package gemini

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
)

// ReapRun implements [adapter.RunReaper]. The private home is kept between
// runs so `--conversation` can resume, which means agy's per-conversation
// files — a database of some 400 KB each, plus its notes — would pile up there
// forever. Each run records which conversation it used (runs/<run id>); this
// keeps the newest policy.KeepRuns of those, never one that is live, and
// removes the conversation files of the rest, and agy's own logs beyond the
// same count.
func (a *Adapter) ReapRun(spec adapter.RunSpec, policy adapter.ReapPolicy) ([]string, error) {
	if policy.KeepRuns <= 0 {
		return nil, nil
	}
	root, err := a.root()
	if err != nil {
		return nil, err
	}
	runsDir := filepath.Join(root, "runs")
	entries, err := os.ReadDir(runsDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type rec struct {
		name string
		mod  int64
	}
	var recs []rec
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		recs = append(recs, rec{e.Name(), info.ModTime().UnixNano()})
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].mod > recs[j].mod })

	live := map[string]bool{}
	for id := range policy.Live {
		live[safeName(id)] = true
	}
	agyDir := filepath.Join(root, "home", ".gemini", "antigravity-cli")
	var removed []string
	// Live runs count against what is kept, wherever they sort.
	kept := 0
	for _, r := range recs {
		if live[r.name] {
			kept++
		}
	}
	for _, r := range recs {
		if live[r.name] {
			continue
		}
		if kept < policy.KeepRuns {
			kept++
			continue
		}
		blob, err := os.ReadFile(filepath.Join(runsDir, r.name))
		if err != nil {
			continue
		}
		cid := strings.TrimSpace(string(blob))
		if validID(cid) && !liveConversation(runsDir, cid, live) {
			for _, p := range []string{
				filepath.Join(agyDir, "conversations", cid+".db"),
				filepath.Join(agyDir, "brain", cid),
				filepath.Join(agyDir, "annotations", cid+".pbtxt"),
				filepath.Join(agyDir, "presence", cid+".lock"),
			} {
				_ = os.RemoveAll(p)
			}
			removed = append(removed, fmt.Sprintf("gemini: removed Antigravity CLI conversation %s (run %s)", cid, r.name))
		}
		_ = os.Remove(filepath.Join(runsDir, r.name))
	}
	removed = append(removed, pruneLogs(filepath.Join(agyDir, "log"), policy.KeepRuns)...)
	return removed, nil
}

// liveConversation reports whether a live run is using cid — a resumed run
// carries on the conversation an older run started.
func liveConversation(runsDir, cid string, live map[string]bool) bool {
	for name := range live {
		blob, err := os.ReadFile(filepath.Join(runsDir, name))
		if err == nil && strings.TrimSpace(string(blob)) == cid {
			return true
		}
	}
	return false
}

// pruneLogs keeps agy's newest keep cli-*.log files.
func pruneLogs(dir string, keep int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "cli-") && strings.HasSuffix(e.Name(), ".log") {
			names = append(names, e.Name())
		}
	}
	// cli-YYYYMMDD_HHMMSS.log sorts by time.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	var out []string
	for i, n := range names {
		if i < keep {
			continue
		}
		if os.Remove(filepath.Join(dir, n)) == nil {
			out = append(out, "gemini: removed Antigravity CLI log "+n)
		}
	}
	return out
}

var idRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z-]{7,63}$`)

// validID accepts agy's conversation ids (UUIDs) and nothing that could name
// a path outside the directory it is joined to.
func validID(id string) bool { return idRE.MatchString(id) }

// safeName keeps a run id usable as a file name.
func safeName(runID string) string {
	var b strings.Builder
	for _, r := range runID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "run"
	}
	return b.String()
}
