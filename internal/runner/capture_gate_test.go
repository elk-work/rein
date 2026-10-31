package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/capture"
)

func TestApplyCaptureGatePrependsTheShimAndKeepsTheOldPath(t *testing.T) {
	dir := t.TempDir()
	spec := &adapter.RunSpec{Env: map[string]string{"PATH": "/usr/bin", "KEEP": "yes"}}
	if err := applyCaptureGate(spec, dir, "/opt/rein"); err != nil {
		t.Fatal(err)
	}
	if spec.Env["KEEP"] != "yes" {
		t.Fatalf("lost an existing variable: %+v", spec.Env)
	}
	if spec.Env[capture.EnvBin] != dir {
		t.Fatalf("bin = %q", spec.Env[capture.EnvBin])
	}
	first, rest, ok := strings.Cut(spec.Env["PATH"], string(os.PathListSeparator))
	if !ok || first != dir || rest != "/usr/bin" {
		t.Fatalf("PATH = %q", spec.Env["PATH"])
	}
	body, err := os.ReadFile(filepath.Join(dir, "screencapture"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "exec '/opt/rein' capture screencapture") {
		t.Fatalf("shim = %q", body)
	}
}
