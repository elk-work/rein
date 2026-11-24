package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/config"
)

// The block as a lab machine writes it reads back whole, with the defaults
// filled in where it says nothing.
func TestYieldToGitHubLoads(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	path := filepath.Join(home, config.FileName)
	body := `[yield_to_github]
runner    = "win-01"
labels    = ["self-hosted", "windows", "win11", "ui-lab", "win-01"]
repos     = ["elk-work/scout"]
workflows = ["port-train.yml"]
grace     = "90s"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	y := cfg.YieldToGitHub
	if !y.Enabled() || y.Runner != "win-01" || len(y.Labels) != 5 || y.Workflows[0] != "port-train.yml" {
		t.Fatalf("read back %+v", y)
	}
	if y.GraceOrDefault() != 90*time.Second || y.IntervalOrDefault() != config.DefaultYieldInterval {
		t.Errorf("grace %s interval %s", y.GraceOrDefault(), y.IntervalOrDefault())
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestYieldToGitHubValidate(t *testing.T) {
	ok := config.YieldToGitHub{Runner: "win-02", Labels: []string{"self-hosted", "win-02"}, Repos: []string{"elk-work/scout"}}
	for _, tc := range []struct {
		name string
		edit func(*config.YieldToGitHub)
		want string
	}{
		{"complete", func(*config.YieldToGitHub) {}, ""},
		{"off", func(y *config.YieldToGitHub) { *y = config.YieldToGitHub{} }, ""},
		{"settings without a runner", func(y *config.YieldToGitHub) { y.Runner = "" }, "names no runner"},
		{"no labels", func(y *config.YieldToGitHub) { y.Labels = nil }, "labels must list"},
		{"no repos", func(y *config.YieldToGitHub) { y.Repos = nil }, "repos must name"},
		{"repo not owner/name", func(y *config.YieldToGitHub) { y.Repos = []string{"scout"} }, "not owner/name"},
		{"workflow path", func(y *config.YieldToGitHub) { y.Workflows = []string{".github/workflows/x.yml"} }, "workflow file name"},
		{"negative grace", func(y *config.YieldToGitHub) { y.Grace = config.Duration(-time.Second) }, "negative"},
		{"interval too short", func(y *config.YieldToGitHub) { y.Interval = config.Duration(time.Second) }, "floor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			y := ok
			y.Labels = append([]string(nil), ok.Labels...)
			tc.edit(&y)
			err := config.Config{YieldToGitHub: y}.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Validate = %v, want nil", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
