package secretenv_test

import (
	"encoding/json"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/secretenv"
)

func TestFilterKeepsOnlySystemVariables(t *testing.T) {
	in := []string{
		"PATH=/usr/bin", "HOME=/Users/me", "LANG=en_US.UTF-8", "LC_ALL=C", "TMPDIR=/tmp",
		"SSH_AUTH_SOCK=/tmp/agent", "HTTPS_PROXY=http://proxy:3128",
		"SUPABASE_SERVICE_ROLE_KEY=nope", "AWS_SECRET_ACCESS_KEY=nope", "GITHUB_TOKEN=nope",
		"OPENAI_API_KEY=nope", "REIN_KEYRING=file", "NOEQUALS", "=C:=C:\\dir",
	}
	got := secretenv.Filter(in)
	want := []string{
		"PATH=/usr/bin", "HOME=/Users/me", "LANG=en_US.UTF-8", "LC_ALL=C", "TMPDIR=/tmp",
		"SSH_AUTH_SOCK=/tmp/agent", "HTTPS_PROXY=http://proxy:3128",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Filter =\n%v\nwant\n%v", got, want)
	}
}

func TestInheritedIsCaseInsensitiveOnlyOnWindows(t *testing.T) {
	if !secretenv.Inherited("SystemRoot") && runtime.GOOS == "windows" {
		t.Error("SystemRoot is not inherited on Windows")
	}
	if secretenv.Inherited("path") && runtime.GOOS != "windows" {
		t.Error("lower-case path is a different variable off Windows")
	}
	for _, name := range []string{"PATH", "HOME", "LC_CTYPE"} {
		if !secretenv.Inherited(name) {
			t.Errorf("%s is not inherited", name)
		}
	}
	for _, name := range []string{"GITHUB_TOKEN", "ANTHROPIC_API_KEY", "SUPABASE_ACCESS_TOKEN", "AWS_PROFILE"} {
		if secretenv.Inherited(name) {
			t.Errorf("%s is inherited by a scoped run", name)
		}
	}
}

func TestRedactorReplacesEveryValueWithItsName(t *testing.T) {
	r := secretenv.NewRedactor(map[string]string{
		"POSTHOG_API_KEY": "phc_abcdefghijklmnop",
		"EMPTY":           "",
	})
	in := "key=phc_abcdefghijklmnop, again phc_abcdefghijklmnop."
	want := "key=[redacted:POSTHOG_API_KEY], again [redacted:POSTHOG_API_KEY]."
	if got := r.String(in); got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
	if got := string(r.Bytes([]byte(in))); got != want {
		t.Errorf("Bytes = %q, want %q", got, want)
	}
	if got := r.String("nothing secret here"); got != "nothing secret here" {
		t.Errorf("text with no value changed: %q", got)
	}
}

func TestRedactorReplacesTheLongerValueFirst(t *testing.T) {
	r := secretenv.NewRedactor(map[string]string{
		"SHORT": "abcdefgh",
		"LONG":  "abcdefgh-and-more-0123",
	})
	if got := r.String("x abcdefgh-and-more-0123 y abcdefgh z"); got != "x [redacted:LONG] y [redacted:SHORT] z" {
		t.Errorf("got %q — a value containing another must go whole", got)
	}
}

func TestRedactorCatchesAValueInsideJSON(t *testing.T) {
	value := `p"ss<w>rd\&more`
	r := secretenv.NewRedactor(map[string]string{"TOKEN": value})
	line, _ := json.Marshal(map[string]string{"text": "it is " + value})
	got := string(r.Bytes(line))
	if strings.Contains(got, "ss") && strings.Contains(got, "rd") && !strings.Contains(got, "[redacted:TOKEN]") {
		t.Fatalf("the JSON-escaped value survived: %s", got)
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("redaction broke the JSON: %s", got)
	}
}

func TestANilRedactorIsANoOp(t *testing.T) {
	var r *secretenv.Redactor
	if !r.Empty() || r.String("x") != "x" || string(r.Bytes([]byte("x"))) != "x" {
		t.Error("a nil redactor is not a no-op")
	}
}

func TestFilterPassesNamedVariablesButNeverAMeteredKey(t *testing.T) {
	in := []string{"PATH=/bin", "SENTRY_ORG=elk", "STRAY=x", "ANTHROPIC_API_KEY=k", "XAI_API_KEY=k", "DEVELOPER_DIR=/x"}
	got := secretenv.Filter(in, "SENTRY_ORG", "ANTHROPIC_API_KEY")
	want := []string{"PATH=/bin", "SENTRY_ORG=elk", "DEVELOPER_DIR=/x"}
	if !slices.Equal(got, want) {
		t.Errorf("Filter = %v; want %v", got, want)
	}
	for _, name := range secretenv.MeteredNames() {
		if !secretenv.Metered(name) {
			t.Errorf("%s is listed but not matched", name)
		}
	}
	if secretenv.Metered("POSTHOG_API_KEY") {
		t.Error("an ordinary credential read as metered")
	}
}
