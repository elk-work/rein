//go:build integration

package gemini

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// The real agy, a throwaway private home, and a key Google will refuse. It
// proves the whole path without a real key: the private home puts agy in key
// mode, the key reaches agy, agy sends it to the Gemini API and not to a
// Google sign-in, and Google's refusal comes back as the run's error.
//
//	REIN_GEMINI_LIVE=1 go test -tags integration ./internal/adapter/gemini/ -run Live
//
// It sends one short prompt and a key that is not a key to Google.
func TestLiveAgyOnARefusedKey(t *testing.T) {
	if os.Getenv("REIN_GEMINI_LIVE") != "1" {
		t.Skip("set REIN_GEMINI_LIVE=1 to run against the real agy")
	}
	bin, err := exec.LookPath(Binary)
	if err != nil {
		t.Skipf("agy is not installed: %v", err)
	}
	real, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	a := &Adapter{binary: bin, Root: filepath.Join(t.TempDir(), "gemini"), RealHome: real,
		Items: items{KeyService + "/" + KeyName: testKey}}
	if err := a.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	spec := baseSpec(t)
	spec.PermissionMode = adapter.PermissionReadOnly
	spec.Prompt = "Reply with the single word: hi"
	spec.Timeouts.Total = 2 * time.Minute
	s, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for range s.Events() {
	}
	res, err := s.Wait()
	t.Logf("status %s, exit %d, err %v", res.Status, res.ExitCode, err)
	if err == nil || !strings.Contains(err.Error(), "API key not valid") {
		t.Fatalf("want Google's own refusal of the key; got %v", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Error("the error carries the key")
	}
}
