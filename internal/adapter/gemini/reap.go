package gemini

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
)

// ReapRun implements [adapter.RunReaper]. A session removes its own private
// home when it ends; this removes any that a crash or a killed daemon left
// behind, never one belonging to a run that is still live on this machine.
func (a *Adapter) ReapRun(spec adapter.RunSpec, policy adapter.ReapPolicy) ([]string, error) {
	root, err := a.root()
	if err != nil {
		return nil, err
	}
	runs := filepath.Join(root, "runs")
	entries, err := os.ReadDir(runs)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	live := make([]string, 0, len(policy.Live))
	for id := range policy.Live {
		live = append(live, safeName(id)+"-")
	}
	var removed []string
	for _, e := range entries {
		if !e.IsDir() || isLive(e.Name(), live) {
			continue
		}
		if os.RemoveAll(filepath.Join(runs, e.Name())) == nil {
			removed = append(removed, "gemini: removed a private Gemini CLI home left by an earlier run: "+e.Name())
		}
	}
	return removed, nil
}

func isLive(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// safeName keeps a run id usable as part of a directory name.
func safeName(runID string) string {
	var b strings.Builder
	for _, r := range runID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
		if b.Len() >= 40 {
			break
		}
	}
	if b.Len() == 0 {
		return "run"
	}
	return b.String()
}
