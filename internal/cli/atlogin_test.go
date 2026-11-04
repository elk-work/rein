package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/elk-work/rein/internal/runner"
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
