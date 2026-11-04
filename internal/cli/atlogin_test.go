package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/runner"
	"github.com/elk-work/rein/internal/service"
)

func TestAtLoginRestartsAfterAnUpgradeAndStopsOnACleanExit(t *testing.T) {
	codes := []int{runner.RestartExitCode, runner.RestartExitCode, 0}
	var seen [][]string
	start := func(_ context.Context, args []string) (int, error) {
		seen = append(seen, args)
		c := codes[0]
		codes = codes[1:]
		return c, nil
	}
	var log bytes.Buffer
	args := atLoginChildArgs([]string{"run", "--at-login", "--log-file", "x.log"})
	if err := superviseAtLogin(context.Background(), &log, args, start); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("started %d times, want 3", len(seen))
	}
	if want := []string{"run", "--supervised", "--log-file", "x.log"}; !reflect.DeepEqual(seen[0], want) {
		t.Errorf("child args = %v, want %v", seen[0], want)
	}
}

func TestAtLoginStopsWhenTold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	start := func(_ context.Context, _ []string) (int, error) {
		n++
		cancel()
		return -1, errors.New("killed")
	}
	if err := superviseAtLogin(ctx, &bytes.Buffer{}, nil, start); err != nil || n != 1 {
		t.Fatalf("err=%v starts=%d", err, n)
	}
}

// Parse the registered task's arguments through the real command tree after
// losing the installer's environment, as happens at the next Windows sign-in.
func TestAtLoginChildRestoresInstallingHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	args, err := service.LogonTaskArgs("", filepath.Join(home, "log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvHome, t.TempDir())
	root := NewRootCommand("test")
	root.SetArgs(atLoginChildArgs(args))
	// No config was written: reaching the captured path proves argument parsing
	// and home restoration without starting queues or opening the OS keyring.
	err = root.Execute()
	if err == nil || !strings.Contains(err.Error(), "no config at "+filepath.Join(home, config.FileName)) {
		t.Fatalf("expected missing captured config, got %v", err)
	}
	got, err := config.Dir() // The same home runRun passes to keyring.Open.
	if err != nil || got != home {
		t.Fatalf("keyring home = %q, %v; want %q", got, err, home)
	}
}
