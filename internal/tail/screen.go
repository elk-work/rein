package tail

import (
	"io"
	"strings"
)

// screen draws a scrolling pane with a status block pinned under it.
//
// The whole trick is two escape sequences. The status block is the last thing
// on screen, so before writing anything new we move the cursor up by however
// many lines it occupies and erase from there to the bottom; then we write the
// new stream lines, which scroll normally; then we print the status block
// again. Nothing needs to know the terminal's height, nothing has to redraw
// what has already scrolled away, and the output is still a stream of lines —
// so the degraded path is not a different renderer, it is this one with the
// two escapes turned off.
//
// Status lines are truncated to the terminal width before they are counted. A
// line that wraps occupies two rows, and the cursor-up count would then be
// wrong by one for every wrapped line — which is how an in-place renderer
// starts eating the scrollback above it.
type screen struct {
	out   io.Writer
	tty   bool
	width int

	status []string
	drawn  int
}

func newScreen(out io.Writer, tty bool, width int) *screen {
	return &screen{out: out, tty: tty, width: width}
}

// Emit writes stream lines above the status block.
func (s *screen) Emit(lines ...string) {
	if len(lines) == 0 {
		return
	}
	s.erase()
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	io.WriteString(s.out, b.String())
	s.paint()
}

// SetStatus replaces the pinned block and repaints it. On a pipe it does
// nothing at all: a status block that cannot be erased would be a line of
// duplicated noise every refresh.
func (s *screen) SetStatus(lines []string) {
	if !s.tty {
		return
	}
	if equal(s.status, lines) {
		return
	}
	s.erase()
	s.status = lines
	s.paint()
}

// Close takes the status block away, leaving the terminal holding the stream
// and nothing else.
func (s *screen) Close() {
	s.erase()
	s.status = nil
}

func (s *screen) erase() {
	if !s.tty || s.drawn == 0 {
		return
	}
	// \r to column 0, up by the block's height, then erase to the end of the
	// screen. Erase-to-end rather than per-line clears is what makes a status
	// block that just got shorter disappear completely.
	io.WriteString(s.out, "\r\x1b["+itoa(s.drawn)+"A\x1b[J")
	s.drawn = 0
}

func (s *screen) paint() {
	if !s.tty || len(s.status) == 0 {
		return
	}
	var b strings.Builder
	for _, l := range s.status {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	io.WriteString(s.out, b.String())
	s.drawn = len(s.status)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// itoa avoids pulling strconv into the hot path of a repaint that happens
// twice a second.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
