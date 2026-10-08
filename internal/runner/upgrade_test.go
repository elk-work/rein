package runner

import (
	"context"
	"github.com/elk-work/rein/internal/config"
	"io"
	"os"
	"testing"
	"time"
)

func TestUpgradeProbe(t *testing.T) {
	for _, tc := range []struct {
		name, version  string
		missing, drain bool
	}{
		{"same", "v1", false, false}, {"new", "v2", false, true}, {"missing", "v2", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/rein"
			if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			baseline, _ := os.Stat(path)
			if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
				t.Fatal(err)
			}
			r := &Runner{opts: Options{Version: "v1", UnderService: true, Out: io.Discard, UpgradeStat: os.Stat, UpgradeVersion: func(context.Context, string) (string, error) { return tc.version, nil }}, upgradeDone: make(chan struct{})}
			if tc.missing {
				os.Remove(path)
			}
			r.checkUpgrade(context.Background(), path, &baseline)
			if r.isDraining() != tc.drain {
				t.Fatalf("draining = %v", r.isDraining())
			}
			if !tc.missing {
				info, _ := os.Stat(path)
				if !sameBinary(baseline, info) {
					t.Fatal("not rebaselined")
				}
			}
		})
	}
}

func TestDrainWaitsForClaim(t *testing.T) {
	r := &Runner{opts: Options{Out: io.Discard}, upgradeDone: make(chan struct{})}
	if !r.beginClaim() {
		t.Fatal("initial claim blocked")
	}
	r.drain("v2")
	if r.beginClaim() {
		t.Fatal("new claim admitted during drain")
	}
	select {
	case <-r.upgradeDone:
		t.Fatal("exited during drive")
	default:
	}
	r.endClaim()
	select {
	case <-r.upgradeDone:
	case <-time.After(time.Second):
		t.Fatal("did not finish drain")
	}
}

func TestUpgradeLogOnlyModes(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"interactive", Options{}}, {"once", Options{UnderService: true, Once: true}},
		{"dry-run", Options{UnderService: true, DryRun: true}},
		{"opt-out", Options{UnderService: true, Config: config.Config{SelfRestart: &off}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/binary"
			os.WriteFile(path, []byte("old"), 0600)
			baseline, _ := os.Stat(path)
			os.WriteFile(path, []byte("replacement"), 0600)
			tc.opts.Out = io.Discard
			tc.opts.Version = "v1"
			tc.opts.UpgradeStat = os.Stat
			tc.opts.UpgradeVersion = func(context.Context, string) (string, error) { return "v2", nil }
			r := &Runner{opts: tc.opts, upgradeDone: make(chan struct{})}
			r.checkUpgrade(context.Background(), path, &baseline)
			if r.isDraining() {
				t.Fatal("log-only mode drained")
			}
		})
	}
}
