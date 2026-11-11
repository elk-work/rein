package runner

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk"
)

// What task live step 2 adds to the run loop (scout spec
// 2026-11-11-task-live-step-2-design.md, sections 6-8).

// vendorNoise is progress the Claude adapter makes out of stream-json it has
// no kind for. It stays in the local log and never reaches Elk.
func vendorNoise(text string) bool {
	return strings.HasPrefix(text, "claude event ") || strings.HasPrefix(text, "claude system/")
}

const (
	// replyWindow is how long after a person's message goes into the session
	// the agent's text is sent as it comes, rather than with the next receipt.
	replyWindow = 3 * time.Minute
	// sayCoalesce gathers text arriving close together into one say.
	sayCoalesce = 500 * time.Millisecond
	// sayMax is Elk's cap on one event body.
	sayMax = 4000
	// planEvery is the least time between two plan reports.
	planEvery = 5 * time.Second
)

// inReplyWindow reports whether now is inside the window a delivered message
// opened at opened.
func inReplyWindow(opened, now time.Time) bool {
	return !opened.IsZero() && now.Sub(opened) < replyWindow
}

// sayParts cuts the agent's text into bodies of at most sayMax bytes, on rune
// boundaries, so none is truncated. Blank text is no parts.
func sayParts(text string) []string {
	text = strings.TrimSpace(text)
	var out []string
	for len(text) > sayMax {
		cut := sayMax
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// repoPath is p relative to the worktree dir, slash-separated, or "" when p
// is outside it. A worktree reached through a symlink (macOS /var is
// /private/var) still matches the file's real path.
func repoPath(dir, p string) string {
	if p = strings.TrimSpace(p); p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	dirs := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dirs = append(dirs, real)
	}
	paths := []string{p}
	if real, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		paths = append(paths, filepath.Join(real, filepath.Base(p)))
	}
	for _, d := range dirs {
		for _, q := range paths {
			rel, err := filepath.Rel(d, q)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return filepath.ToSlash(rel)
			}
		}
	}
	return ""
}

// todoPlan is the plan in a Claude TodoWrite call, and whether t is one. A
// status Elk does not know reads as pending.
func todoPlan(t *adapter.ToolUse) ([]elk.PlanItem, bool) {
	if t == nil || t.Name != "TodoWrite" {
		return nil, false
	}
	var in struct {
		Todos []elk.PlanItem `json:"todos"`
	}
	if json.Unmarshal(t.Input, &in) != nil {
		return nil, false
	}
	plan := make([]elk.PlanItem, 0, len(in.Todos))
	for _, item := range in.Todos {
		switch item.Status {
		case "pending", "in_progress", "completed":
		default:
			item.Status = "pending"
		}
		plan = append(plan, item)
	}
	return plan, true
}
