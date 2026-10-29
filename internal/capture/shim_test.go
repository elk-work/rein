package capture

import (
	"os"
	"strings"
	"testing"
)

func TestShimForwardsToReinAndDoesNotCallTheSystemTool(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteScreencaptureShim(dir, "/usr/local/bin/rein")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "exec '/usr/local/bin/rein' capture screencapture -- \"$@\"") {
		t.Fatalf("shim = %q", got)
	}
	if strings.Contains(got, "/usr/sbin/screencapture") {
		t.Fatalf("the shim calls the system tool itself:\n%s", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("shim mode %v is not executable", info.Mode())
	}
}

func TestShimQuotesAPathWithAQuote(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteScreencaptureShim(dir, "/tmp/it's rein")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `'/tmp/it'\''s rein'`) {
		t.Fatalf("shim = %q", body)
	}
}
