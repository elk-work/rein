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

// ark:rein#66: a changed config.toml drains the way an upgrade does; one the
// config check would refuse, an identical rewrite and the opt-out do not.
func TestConfigChangeDrains(t *testing.T) {
	const base = "workspace = \"Scout\"\n\n[repos]\nscout = \"/dev/scout\"\n\n[[queues]]\nname = \"mac-claude\"\nagent_kind = \"claude\"\nrepo = \"scout\"\n"
	off := false
	const two = base + "\n[[queues]]\nname = \"mac-codex\"\nagent_kind = \"codex\"\nrepo = \"scout\"\n"
	for _, tc := range []struct {
		name    string
		rewrite string
		opts    Options
		drain   bool
		start   string
	}{
		{"unchanged", base, Options{UnderService: true}, false, ""},
		{"a queue added", base + "\n[[queues]]\nname = \"mac-codex\"\nagent_kind = \"codex\"\nrepo = \"scout\"\n", Options{UnderService: true}, true, ""},
		{"a file that does not load", "this is [not toml", Options{UnderService: true}, false, ""},
		// A revoke takes one queue out on purpose; the other keeps its repositories.
		{"one of two queues dropped", base, Options{UnderService: true}, true, two},
		// A mistyped workspace is not a revoke: the queue moves, and loses its repository.
		{"a workspace renamed with a queue dropped", "workspace = \"Scuot\"\n\n[[queues]]\nname = \"mac-claude\"\nagent_kind = \"claude\"\n", Options{UnderService: true}, false, two},
		{"every queue dropped", "workspace = \"Scout\"\n", Options{UnderService: true}, false, ""},
		{"opt-out", base + "\n# changed\n", Options{UnderService: true, Config: config.Config{SelfRestart: &off}}, false, ""},
		{"interactive", base + "\n# changed\n", Options{}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/config.toml"
			start := base
			if tc.start != "" {
				start = tc.start
			}
			if err := os.WriteFile(path, []byte(start), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.opts.Config.SelfRestart != nil {
				cfg.SelfRestart = tc.opts.Config.SelfRestart
			}
			tc.opts.Config, tc.opts.ConfigPath, tc.opts.Version, tc.opts.Out = cfg, path, "v1", io.Discard
			r := &Runner{opts: tc.opts, upgradeDone: make(chan struct{})}
			r.baselineConfig()
			if err := os.WriteFile(path, []byte(tc.rewrite), 0600); err != nil {
				t.Fatal(err)
			}
			r.checkConfigChange()
			if r.isDraining() != tc.drain {
				t.Fatalf("draining = %v, want %v", r.isDraining(), tc.drain)
			}
			if !tc.drain && r.isDraining() {
				t.Fatal("drained")
			}
		})
	}
}

func TestConfigChangeWaitsForInFlightRuns(t *testing.T) {
	path := t.TempDir() + "/config.toml"
	os.WriteFile(path, []byte("workspace = \"Scout\"\n"), 0600)
	r := &Runner{opts: Options{UnderService: true, ConfigPath: path, Version: "v1", Out: io.Discard}, upgradeDone: make(chan struct{})}
	r.baselineConfig()
	r.beginClaim()
	os.WriteFile(path, []byte("workspace = \"Scout\"\n# edited\n"), 0600)
	r.checkConfigChange()
	if !r.isDraining() || r.beginClaim() {
		t.Fatal("not draining, or admitted a claim")
	}
	select {
	case <-r.upgradeDone:
		t.Fatal("exited with a run in flight")
	default:
	}
	r.endClaim()
	select {
	case <-r.upgradeDone:
	case <-time.After(time.Second):
		t.Fatal("did not finish drain")
	}
}
