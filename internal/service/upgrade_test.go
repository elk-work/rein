package service

import (
	"context"
	"github.com/elk-work/rein/internal/runner"
	"os"
	"testing"
	"time"
)

func TestProgramUpgradeExit(t *testing.T) {
	exits := make(chan int, 1)
	p := &program{ctx: context.Background(), run: func(context.Context) error { return runner.ErrRestartForUpgrade }, done: make(chan struct{}), stopping: make(chan struct{}), errOut: os.Stderr, exit: func(code int) { exits <- code }}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-exits:
		if got != 75 {
			t.Fatalf("exit = %d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not exit")
	}
}
