package tail

import (
	"bytes"
	"strings"
	"testing"
)

// The whole contract of the in-place renderer, in one sequence: a status block
// is printed, erased by moving up exactly its own height, and reprinted after
// the new stream lines have scrolled past it.
func TestScreenErasesExactlyTheBlockItDrew(t *testing.T) {
	var b bytes.Buffer
	s := newScreen(&b, true, 80)

	s.SetStatus([]string{"one", "two"})
	if got := b.String(); got != "one\ntwo\n" {
		t.Fatalf("first paint = %q", got)
	}

	b.Reset()
	s.Emit("a line of agent text")
	// Up two, erase to the end of the screen, the new line, then the block
	// again. Erase-to-end rather than per-line clears is what makes a block
	// that just got shorter disappear completely.
	if got := b.String(); got != "\r\x1b[2A\x1b[Ja line of agent text\none\ntwo\n" {
		t.Fatalf("emit = %q", got)
	}

	b.Reset()
	s.SetStatus([]string{"only one now"})
	if got := b.String(); got != "\r\x1b[2A\x1b[Jonly one now\n" {
		t.Fatalf("shrinking the block = %q", got)
	}

	b.Reset()
	s.Close()
	if got := b.String(); got != "\r\x1b[1A\x1b[J" {
		t.Fatalf("close = %q", got)
	}
	// Closing twice must not move the cursor again: there is nothing left to
	// erase and the cursor is somebody else's now.
	b.Reset()
	s.Close()
	if got := b.String(); got != "" {
		t.Fatalf("second close = %q", got)
	}
}

// An unchanged status block is not repainted. Without this the screen would
// rewrite the same three lines two and a half times a second forever, which is
// visible as a flicker and pointless on a pipe.
func TestScreenSkipsAnUnchangedStatus(t *testing.T) {
	var b bytes.Buffer
	s := newScreen(&b, true, 80)
	s.SetStatus([]string{"steady"})
	b.Reset()
	s.SetStatus([]string{"steady"})
	if got := b.String(); got != "" {
		t.Errorf("an unchanged status repainted: %q", got)
	}
}

// On a pipe the status block does not exist and no escape sequence is written:
// it could never be erased, so it would be duplicated noise in the file.
func TestScreenDegradesToLines(t *testing.T) {
	var b bytes.Buffer
	s := newScreen(&b, false, 80)
	s.SetStatus([]string{"a status block"})
	s.Emit("first", "second")
	s.SetStatus([]string{"another"})
	s.Emit("third")
	s.Close()

	got := b.String()
	if strings.Contains(got, "\x1b") {
		t.Errorf("escape sequences reached a pipe: %q", got)
	}
	if got != "first\nsecond\nthird\n" {
		t.Errorf("pipe output = %q", got)
	}
}

func TestEmitOfNothingWritesNothing(t *testing.T) {
	var b bytes.Buffer
	s := newScreen(&b, true, 80)
	s.SetStatus([]string{"block"})
	b.Reset()
	s.Emit()
	if got := b.String(); got != "" {
		t.Errorf("an empty Emit wrote %q", got)
	}
}
