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

func TestParseModelsIgnoresProse(t *testing.T) {
	got := parseModels("You are logged in.\ngrok-4 is deprecated since the last release\n")
	if got != nil {
		t.Errorf("prose line gave %v, want nil", got)
	}
	got = parseModels("You are logged in.\n  - grok-4\nNote: grok-5 is coming soon, stay tuned\ngrok-6\n")
	if want := []string{"grok-4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parseModels = %v, want %v", got, want)
	}
	got = parseModels("You are logged in.\nModels:\ngrok-4\n\ngrok-3\n")
	if want := []string{"grok-4", "grok-3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("header then ids = %v, want %v", got, want)
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
