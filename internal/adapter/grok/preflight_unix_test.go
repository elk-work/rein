//go:build !windows

package grok

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

// fakeGrok writes a `grok` that answers `models` with first on its first call
// and second on every later one, exiting with code each time, and returns its
// path and a function that says how many times it ran.
func fakeGrok(t *testing.T, first, second string, code int) (string, func() int) {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = models ] || exit 64\n" +
		"echo x >> '" + count + "'\n" +
		"if [ \"$(wc -l < '" + count + "')\" -le 1 ]; then echo '" + first + "'; else echo '" + second + "'; fi\n" +
		"exit " + map[int]string{0: "0", 1: "1"}[code] + "\n"
	path := filepath.Join(dir, "grok")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "\n")
	}
}

func preflightWith(path string) error {
	a := New()
	a.binary = path
	return a.Preflight(context.Background())
}

// Started with an expired access token, `grok models` prints the login it
// started with and then refreshes it in the same call, so the first answer
// says "not authenticated" for a login that is fine (ark:rein#94). The second
// answer is the one judged.
func TestPreflightTrustsTheSecondAnswerAfterARefresh(t *testing.T) {
	path, calls := fakeGrok(t, "You are not authenticated.", "You are logged in with grok-code-fast-1.", 0)
	if err := preflightWith(path); err != nil {
		t.Fatalf("Preflight → %v, want nil: the first call refreshed the login", err)
	}
	if n := calls(); n != 2 {
		t.Errorf("grok models ran %d times, want 2", n)
	}
}

// A login that really is gone says so both times.
func TestPreflightFailsWhenBothAnswersSayNoLogin(t *testing.T) {
	path, calls := fakeGrok(t, "You are not authenticated.", "You are not authenticated.", 0)
	err := preflightWith(path)
	if !errors.Is(err, adapter.ErrPreflight) {
		t.Fatalf("Preflight → %v, want ErrPreflight", err)
	}
	if n := calls(); n != 2 {
		t.Errorf("grok models ran %d times, want 2", n)
	}
	for _, want := range []string{"not authenticated", "run `grok models` once", "grok login --device-auth"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message lacks %q: %v", want, err)
		}
	}
}

// A call that fails is not asked again: a refresh is not what is wrong.
func TestPreflightDoesNotRetryAFailedCall(t *testing.T) {
	path, calls := fakeGrok(t, "boom", "You are logged in with grok-code-fast-1.", 1)
	if err := preflightWith(path); !errors.Is(err, adapter.ErrPreflight) {
		t.Fatalf("Preflight → %v, want ErrPreflight", err)
	}
	if n := calls(); n != 1 {
		t.Errorf("grok models ran %d times, want 1", n)
	}
}

// A login that is fine the first time is asked once.
func TestPreflightAsksOnceWhenLoggedIn(t *testing.T) {
	path, calls := fakeGrok(t, "You are logged in with grok-code-fast-1.", "unused", 0)
	if err := preflightWith(path); err != nil {
		t.Fatal(err)
	}
	if n := calls(); n != 1 {
		t.Errorf("grok models ran %d times, want 1", n)
	}
}
