//go:build !windows

package grok

import (
	"context"
	"reflect"
	"testing"
)

// The sample is the shape this adapter assumes `grok models` prints: the login
// line, then one model id per line (bullet and trailing description allowed).
// It is NOT a capture from a real grok binary (none was available when this
// was written), which is why the parser accepts only tokens starting "grok-".
const modelsSample = `You are logged in with grok-code-fast-1.

Available models:
  - grok-code-fast-1  (default)
  * grok-4
  grok-4-fast-reasoning
`

func TestParseModels(t *testing.T) {
	want := []string{"grok-code-fast-1", "grok-4", "grok-4-fast-reasoning"}
	if got := parseModels(modelsSample); !reflect.DeepEqual(got, want) {
		t.Errorf("parseModels = %v, want %v", got, want)
	}
	for _, odd := range []string{"", "You are logged in with grok-code-fast-1.", "You are logged in.\nsomething new: 12\nmodels follow below", "You are not authenticated."} {
		if got := parseModels(odd); got != nil {
			t.Errorf("parseModels(%q) = %v, want nil", odd, got)
		}
	}
}

func TestPreflightKeepsModels(t *testing.T) {
	path, _ := fakeGrok(t, modelsSample, "unused", 0)
	a := New()
	a.binary = path
	if err := a.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.Models(); len(got) != 3 || got[1] != "grok-4" {
		t.Errorf("Models = %v", got)
	}
	// An odd answer still passes preflight and leaves no list.
	path, _ = fakeGrok(t, "You are logged in with grok-code-fast-1.", "unused", 0)
	a.binary = path
	if err := a.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.Models(); got != nil {
		t.Errorf("Models after an empty list = %v, want nil", got)
	}
}
