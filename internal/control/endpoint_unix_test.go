//go:build !windows

package control_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/control"
)

// A `$REIN_HOME` deep enough to push the socket path past the kernel's limit
// gets an answer it can act on.
//
// The kernel's own answer is `EINVAL` — "invalid argument", with no mention of
// a length — which is exactly the error nobody can do anything with. This is
// not hypothetical: it was found by hand-testing `rein attach` from a sandbox
// scratch directory, where the home alone was ninety characters.
func TestSocketPathTooLong(t *testing.T) {
	base, err := os.MkdirTemp("", "rn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })

	home := base
	for len(control.Endpoint(home)) < 110 {
		home = filepath.Join(home, "a-directory-with-a-long-name")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err = control.Listen(home)
	if err == nil {
		t.Fatal("Listen accepted a path past the limit")
	}
	if !errors.Is(err, control.ErrEndpoint) {
		t.Errorf("Listen error is not ErrEndpoint: %v", err)
	}
	for _, want := range []string{"past the", "REIN_HOME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Listen error does not mention %q: %v", want, err)
		}
	}

	// And Dial says the same thing rather than "no runner is listening",
	// which would send somebody off to start one that could never help.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = control.Dial(ctx, home)
	if !errors.Is(err, control.ErrEndpoint) || errors.Is(err, control.ErrNoRunner) {
		t.Errorf("Dial error = %v", err)
	}
}
