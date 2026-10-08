// Package tail renders Rein's per-run event logs to a terminal — the live view
// behind `rein tail`.
//
// It is split in three so that the interesting half is testable without a
// terminal:
//
//   - [Renderer] turns one [runlog.Record] into the lines a person reads. Pure,
//     and covered by golden output over a recorded fake-adapter run.
//   - [screen] does the in-place drawing: a status block that is erased and
//     reprinted above a scrolling pane. Plain ANSI cursor movement, no TUI
//     framework — there is nothing here a dependency would do better.
//   - [Run] is the loop that polls the logs, feeds one to the other, and knows
//     when it is finished.
//
// Everything degrades to plain line output when stdout is not a terminal,
// because `rein tail | tee` and a CI leg both do that and neither wants cursor
// escapes in the file.
package tail

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/runlog"
)

// Marks are the one-glyph prefixes that tell the kinds apart at a glance. They
// are the whole visual vocabulary: a person scanning a fast stream reads the
// left column, not the words.
const (
	markTool       = "▸"
	markPermission = "⚠"
	markQuestion   = "?"
	markRunner     = "·"
	markDone       = "✓"
	markError      = "✗"
)

// ANSI attributes, applied only when the destination is a terminal that wants
// colour. Defined here rather than inline so the golden tests can assert on
// text with colour off and know nothing else changed.
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiBold   = "\x1b[1m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
)

// Renderer turns log records into terminal lines.
//
// The rules are the ones a person asked for: the agent's own text verbatim,
// because paraphrasing the thing you are watching defeats the point; tool
// calls as one dense line each; a permission request or a question lit up,
// because those are the two records that mean the run is waiting on somebody;
// and token usage kept out of the stream entirely and shown in the footer,
// where it does not interrupt the reading.
type Renderer struct {
	// Color turns the ANSI attributes on.
	Color bool

	// Width truncates each rendered line. Zero means no truncation — which is
	// what a pipe wants, since the thing on the other end may be wider.
	Width int

	// Prefix labels every line with the run's short id. Set when more than one
	// run is being followed at once, and unset for a single run, where it is
	// eight characters of noise on every line.
	Prefix bool
}

// Lines renders one record. It returns nothing for the records that belong in
// the footer rather than the stream — usage — and for an empty text record,
// which some adapters emit as a keep-alive.
func (r Renderer) Lines(rec runlog.Record) []string {
	var (
		body []string
		mark string
		attr string
	)
	switch {
	case rec.Source == runlog.SourceRunner:
		mark, attr = markRunner, ansiDim
		body = split(rec.Text)

	case adapter.EventKind(rec.Kind) == adapter.EventText:
		// Verbatim, with no mark and no colour: this is the content, and
		// everything else on screen is a frame around it.
		body = split(rec.Text)

	case adapter.EventKind(rec.Kind) == adapter.EventToolUse:
		mark, attr = markTool, ansiCyan
		body = []string{toolLine(rec.Tool)}

	case adapter.EventKind(rec.Kind) == adapter.EventPermissionRequest:
		mark, attr = markPermission, ansiBold+ansiYellow
		body = []string{permissionLine(rec.Permission)}

	case adapter.EventKind(rec.Kind) == adapter.EventQuestion:
		mark, attr = markQuestion, ansiBold+ansiBlue
		body = split(orDefault(rec.Text, "the agent asked for a person"))

	case adapter.EventKind(rec.Kind) == adapter.EventProgress:
		mark, attr = markRunner, ansiDim
		body = split(rec.Text)

	case adapter.EventKind(rec.Kind) == adapter.EventIdle:
		mark, attr = markRunner, ansiDim
		body = []string{orDefault(rec.Text, "idle — waiting")}

	case adapter.EventKind(rec.Kind) == adapter.EventUsage:
		// The footer's business, not the stream's.
		return nil

	case adapter.EventKind(rec.Kind) == adapter.EventDone:
		mark, attr = markDone, ansiGreen
		body = []string{doneLine(rec)}

	case adapter.EventKind(rec.Kind) == adapter.EventError:
		mark, attr = markError, ansiRed
		body = split(orDefault(rec.Error, orDefault(rec.Text, "the session failed without saying why")))

	default:
		mark, attr = markRunner, ansiDim
		body = split(orDefault(rec.Text, rec.Kind))
	}

	if len(body) == 0 {
		return nil
	}

	prefix := ""
	if r.Prefix {
		prefix = runlog.Short(rec.RunID) + " "
	}
	out := make([]string, 0, len(body))
	for i, line := range body {
		lead := prefix
		switch {
		case mark == "":
			lead += "  "
		case i == 0:
			lead += mark + " "
		default:
			// A continuation of a marked record lines up under the first line
			// rather than repeating the mark, so a five-line tool result still
			// reads as one thing.
			lead += "  "
		}
		out = append(out, r.paint(lead, line, attr))
	}
	return out
}

// paint assembles one line, truncating before colouring so that an escape
// sequence is never cut in half.
func (r Renderer) paint(lead, body, attr string) string {
	line := lead + body
	if r.Width > 0 {
		line = truncate(line, r.Width)
	}
	if !r.Color || attr == "" {
		return line
	}
	return attr + line + ansiReset
}

// toolLine is the `▸ Bash: rm -rf …` half of the vocabulary.
func toolLine(t *adapter.ToolUse) string {
	if t == nil {
		return "a tool"
	}
	name := orDefault(t.Name, "a tool")
	if s := ToolSummary(t); s != "" {
		return name + ": " + s
	}
	return name
}

func permissionLine(p *adapter.PermissionRequest) string {
	if p == nil {
		return "permission requested"
	}
	what := orDefault(p.Summary, p.Tool)
	return "permission: " + orDefault(what, "something") + " — waiting"
}

func doneLine(rec runlog.Record) string {
	status := "finished"
	if rec.Result != nil && rec.Result.Status != "" {
		status = string(rec.Result.Status)
	}
	summary := ""
	if rec.Result != nil {
		summary = firstLine(rec.Result.Summary)
	}
	if summary == "" {
		summary = firstLine(rec.Text)
	}
	if summary == "" {
		return status
	}
	return status + " — " + summary
}

// summaryKeys are the argument names worth showing, in the order to try them.
// It is a list rather than a per-tool table because the tool names belong to
// three different vendors and a table would be three tables that drift; the
// argument that matters is nearly always called one of these.
// "pattern" comes before "path" deliberately: a Grep carries both, and the
// pattern is what the call is about while the path is usually ".".
var summaryKeys = []string{
	"command", "file_path", "notebook_path", "pattern", "url", "query",
	"path", "description", "prompt", "old_string", "content",
}

// ToolSummary picks the one argument worth putting on the line: the command a
// Bash call runs, the file a Read reads, the pattern a Grep looks for.
//
// It reads the vendor's own JSON without a schema, because there is no schema
// to have — [adapter.ToolUse.Input] is passed through verbatim by contract,
// and every vendor names these differently enough that guessing beats
// pretending to know.
func ToolSummary(t *adapter.ToolUse) string {
	if t == nil || len(t.Input) == 0 {
		return ""
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(t.Input, &args); err != nil {
		return collapse(string(t.Input))
	}
	for _, k := range summaryKeys {
		raw, ok := args[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
			return collapse(s)
		}
	}
	// Nothing recognised: show the argument names, which at least says what
	// shape the call was.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return "(" + strings.Join(keys, ", ") + ")"
}

// Footer is the one status line a single-run follow keeps at the bottom: who
// this run is, how long it has been going, and what it has spent.
func (r Renderer) Footer(m runlog.Meta, st RunState, now time.Time) string {
	parts := []string{runlog.Short(m.RunID)}
	if m.Queue != "" {
		parts = append(parts, m.Queue)
	}
	status := string(m.Status)
	if status == "" {
		status = string(runlog.StatusRunning)
	}
	if m.Attached {
		status += " · a person is driving"
	}
	parts = append(parts, status+" "+shortDuration(m.Elapsed(now)))
	if st.InputTokens > 0 || st.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s in / %s out", commas(st.InputTokens), commas(st.OutputTokens)))
	}
	if st.Model != "" {
		parts = append(parts, st.Model)
	}
	if st.LastEvent != "" {
		parts = append(parts, st.LastEvent)
	}
	line := "── " + strings.Join(parts, " · ")
	if r.Width > 0 {
		line = truncate(line, r.Width)
	}
	if r.Color {
		return ansiDim + line + ansiReset
	}
	return line
}

// Column widths for the dashboard. Everything but the direction is fixed,
// because everything but the direction is short and the same shape every time;
// the direction is the column a person actually reads, so it takes the whole
// rest of the terminal rather than a share of it.
const (
	dashGap     = 2
	dashRunCol  = 8
	dashElapCol = 7
	dashLastCol = 16
	dashTokCol  = 8
	dashMinDir  = 14
)

// Dashboard is the all-queues block: one line per run, refreshed in place.
//
// A narrow terminal drops columns from the right rather than letting the whole
// table run off the edge — LAST first, then TOKENS. Queue, run, direction and
// elapsed are what a person needs in order to decide which run to look at, and
// a truncated table that answers none of those is worse than a narrow one that
// answers all four.
func (r Renderer) Dashboard(runs []DashboardRow, now time.Time) []string {
	if len(runs) == 0 {
		return []string{r.paint("", "no runs in flight", ansiDim)}
	}
	queueCol := len("QUEUE")
	for _, row := range runs {
		if n := len([]rune(row.Meta.Queue)); n > queueCol {
			queueCol = n
		}
	}
	width := r.Width
	if width <= 0 {
		width = 120
	}

	base := queueCol + dashGap + dashRunCol + dashGap + dashMinDir + dashGap + dashElapCol
	showTokens := width >= base+dashGap+dashTokCol
	showLast := showTokens && width >= base+dashGap+dashTokCol+dashGap+dashLastCol

	fixed := queueCol + dashGap + dashRunCol + dashGap + dashGap + dashElapCol
	if showLast {
		fixed += dashGap + dashLastCol
	}
	if showTokens {
		fixed += dashGap + dashTokCol
	}
	dirCol := width - fixed
	if dirCol < dashMinDir {
		dirCol = dashMinDir
	}

	line := func(queue, run, dir, elapsed, last, tokens string) string {
		cells := []string{
			pad(fit(queue, queueCol), queueCol),
			pad(fit(run, dashRunCol), dashRunCol),
			pad(fit(dir, dirCol), dirCol),
			lpad(fit(elapsed, dashElapCol), dashElapCol),
		}
		if showLast {
			cells = append(cells, pad(fit(last, dashLastCol), dashLastCol))
		}
		if showTokens {
			cells = append(cells, lpad(fit(tokens, dashTokCol), dashTokCol))
		}
		return strings.TrimRight(strings.Join(cells, strings.Repeat(" ", dashGap)), " ")
	}

	out := []string{r.paint("", line("QUEUE", "RUN", "DIRECTION", "ELAPSED", "LAST", "TOKENS"), ansiDim)}
	for _, row := range runs {
		tokens := ""
		if n := row.State.InputTokens + row.State.OutputTokens; n > 0 {
			tokens = compactTokens(n)
		}
		last := row.State.LastEvent
		if row.Meta.Attached {
			// A person is driving this one. That is the only thing about it
			// worth a column, so it takes the column.
			last = "attached"
		}
		attr := ""
		if !row.Meta.Status.Active() {
			attr = ansiDim
		}
		out = append(out, r.paint("", line(
			orDefault(row.Meta.Queue, "-"),
			runlog.Short(row.Meta.RunID),
			firstLine(row.Meta.Direction),
			shortDuration(row.Meta.Elapsed(now)),
			last, tokens), attr))
	}
	return out
}

// DashboardRow is one run in the live view.
type DashboardRow struct {
	Meta  runlog.Meta
	State RunState
}

// RunState is what a follower has accumulated about one run: the running token
// total and what it last did. Both are derived from the log rather than stored
// in the meta, which is why the meta is only rewritten when a fact changes.
type RunState struct {
	InputTokens  int64
	OutputTokens int64
	Model        string
	LastEvent    string
	Records      int
}

// Observe folds a record into the state.
func (s *RunState) Observe(rec runlog.Record) {
	s.Records++
	if rec.Usage != nil {
		s.InputTokens += rec.Usage.InputTokens
		s.OutputTokens += rec.Usage.OutputTokens
		if rec.Usage.Model != "" {
			s.Model = rec.Usage.Model
		}
	}
	if label := eventLabel(rec); label != "" {
		s.LastEvent = label
	}
}

// eventLabel is the short name of a record for the LAST column.
func eventLabel(rec runlog.Record) string {
	if rec.Source == runlog.SourceRunner {
		return rec.Kind
	}
	switch adapter.EventKind(rec.Kind) {
	case adapter.EventToolUse:
		if rec.Tool != nil && rec.Tool.Name != "" {
			return markTool + " " + rec.Tool.Name
		}
		return markTool + " tool"
	case adapter.EventUsage:
		// Usage says nothing about what the agent is doing, and letting it
		// overwrite the label would make every busy run read "usage".
		return ""
	case adapter.EventPermissionRequest:
		return markPermission + " permission"
	case adapter.EventQuestion:
		return markQuestion + " question"
	case adapter.EventDone:
		if rec.Result != nil && rec.Result.Status != "" {
			return string(rec.Result.Status)
		}
		return "done"
	case adapter.EventError:
		return "error"
	default:
		return rec.Kind
	}
}

// Total renders the closing line a pipe gets in place of a footer: the same
// numbers, once, when the run ends.
func (r Renderer) Total(m runlog.Meta, st RunState, now time.Time) string {
	line := fmt.Sprintf("── %s finished %s after %s — %s in / %s out",
		runlog.Short(m.RunID), orDefault(string(m.Status), "unknown"),
		shortDuration(m.Elapsed(now)), commas(st.InputTokens), commas(st.OutputTokens))
	if st.Model != "" {
		line += " · " + st.Model
	}
	if r.Width > 0 {
		line = truncate(line, r.Width)
	}
	if r.Color {
		return ansiDim + line + ansiReset
	}
	return line
}

// split breaks a body into lines, dropping a trailing blank so an agent's text
// does not leave a gap after every paragraph.
func split(s string) []string {
	s = strings.TrimRight(s, "\n")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

// collapse folds a multi-line argument onto one line: a heredoc in a Bash
// command should take one row of the stream, not thirty.
func collapse(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// truncate cuts a line to n columns, counting runes rather than bytes so a
// line of CJK or an emoji does not silently overflow the terminal.
func truncate(s string, n int) string {
	if n <= 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// fit truncates to a column width, and pad grows to it. Two functions because
// a table cell needs both and Go's %-*s counts bytes.
func fit(s string, n int) string {
	s = collapse(s)
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

func pad(s string, n int) string {
	if d := n - len([]rune(s)); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func lpad(s string, n int) string {
	if d := n - len([]rune(s)); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

// shortDuration renders an elapsed time in the two units that matter, which is
// as much as a column eight wide can hold: 4m12s, 1h04m, 58s.
func shortDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// compactTokens renders a token count in a column eight wide: 940, 12.4k, 1.2M.
func compactTokens(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 1_000_000:
		return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k"
	default:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	}
}

// commas groups a number for the footer, where there is room to be exact.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return "-" + commas(-n)
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
