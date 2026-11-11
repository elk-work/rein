package runner

import (
	"context"
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
		// Rooted but driveless ("/etc/hosts" on Windows) is outside, not
		// relative to the worktree.
		if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
			return ""
		}
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

// The inbox loop's pacing. inboxFloor is the least time between two polls,
// so an Elk that answers at once (or a fake) is not hammered; inboxBackoff is
// the wait after a failure, the first and then every later one in a row.
var (
	inboxFloor   = time.Second
	inboxBackoff = [2]time.Duration{5 * time.Second, 30 * time.Second}
)

// pollInbox long-polls `run_inbox` until ctx ends, handing every answer that
// says something to news. Comments are held on qr before anything else, so a
// reply that lands as the session ends is not lost: the next session, the
// revision or the run's end takes them. An Elk without the tool, or a
// failure, is retried after inboxBackoff.
//
// ponytail: a poll in flight when ctx ends is abandoned; if Elk had already
// taken a comment off its cursor, that comment reaches no session. Waiting
// for the poll would hold the session's end up to 25 seconds.
func (qr *queueRunner) pollInbox(ctx context.Context, runID string, news chan<- elk.Inbox) {
	cursor, fails := 0, 0
	for {
		start := time.Now()
		in, err := qr.elk.RunInbox(ctx, runID, cursor)
		pause := time.Until(start.Add(inboxFloor))
		switch {
		case ctx.Err() != nil:
			if err == nil {
				qr.holdComments(in.Comments)
			}
			return
		case err != nil:
			// Elk advances its cursor before it answers, so a reply lost in
			// transit is comments nobody will resend: every such failure is
			// logged. An Elk without the tool is said once.
			if fails == 0 || !elk.IsUnknownTool(err) {
				qr.logf("run %s: run_inbox: %v; any comment in a lost reply is not resent; trying again shortly", runID, err)
			}
			pause = inboxBackoff[min(fails, len(inboxBackoff)-1)]
			fails++
		default:
			fails = 0
			cursor = in.Cursor
			qr.holdComments(in.Comments)
			if in.Comments != "" || in.Status != "" || in.Model != "" {
				select {
				case news <- *in:
				case <-ctx.Done():
					return
				}
			}
		}
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return
		}
	}
}

// canSwitchModel reports whether a person's model change can be honoured
// mid-run: Claude Code only (scout spec section 8), resumed from its session.
func (qr *queueRunner) canSwitchModel(sess adapter.Session) bool {
	return qr.q.AgentKind == "claude" && sess.ID() != "" &&
		qr.live != nil && qr.live.manifest.Has(adapter.CapResume)
}
