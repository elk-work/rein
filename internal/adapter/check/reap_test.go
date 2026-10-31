package check

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// Every test here works in its own temporary directories. None may resolve
// the real act cache or the real Rein home: a daemon on the machine running
// the tests may be using both.

func mkdirAged(t *testing.T, dir string, age time.Duration, children ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	for _, c := range children {
		p := filepath.Join(dir, c)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(dir, at, at); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// The records follow log_keep_runs exactly as the run logs do: the newest
// kept, a live run counted against the budget and never removed however old,
// and nothing that is not a run's directory touched at all.
func TestReapKeepsTheNewestRecordsAndNeverALiveOne(t *testing.T) {
	records := t.TempDir()
	for i, id := range []string{"run-a", "run-b", "run-c", "run-d", "run-e", "run-f"} {
		mkdirAged(t, filepath.Join(records, id), time.Duration(10-i)*time.Hour)
	}
	// The oldest of all is still going.
	mkdirAged(t, filepath.Join(records, "run-live"), 30*time.Hour)
	if err := os.WriteFile(filepath.Join(records, "stray.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{RecordRoot: records, CacheRoot: filepath.Join(t.TempDir(), "act")}
	now := time.Now()

	lines, err := a.ReapRun(adapter.RunSpec{RunID: "run-f"}, adapter.ReapPolicy{
		KeepRuns: 3, Now: now, Live: map[string]time.Time{"run-live": now.Add(-30 * time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(names(t, records), ","), "run-e,run-f,run-live,stray.txt"; got != want {
		t.Errorf("left %s, want %s", got, want)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "4 old check run(s)") {
		t.Errorf("lines = %q", lines)
	}
	// Nothing to do the second time.
	if lines, err := a.ReapRun(adapter.RunSpec{}, adapter.ReapPolicy{KeepRuns: 3, Now: now,
		Live: map[string]time.Time{"run-live": now.Add(-30 * time.Hour)}}); err != nil || len(lines) != 0 {
		t.Errorf("second reap = %q, %v", lines, err)
	}
}

// log_keep_runs below zero keeps everything; a records directory that does not
// exist yet is not an error.
func TestReapKeepsEveryRecordWhenToldTo(t *testing.T) {
	records := t.TempDir()
	for _, id := range []string{"run-a", "run-b"} {
		mkdirAged(t, filepath.Join(records, id), 48*time.Hour)
	}
	a := &Adapter{RecordRoot: records, CacheRoot: filepath.Join(t.TempDir(), "act")}
	if _, err := a.ReapRun(adapter.RunSpec{}, adapter.ReapPolicy{KeepRuns: 0, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := names(t, records); len(got) != 2 {
		t.Errorf("left %v", got)
	}
	a.RecordRoot = filepath.Join(t.TempDir(), "not-yet")
	if lines, err := a.ReapRun(adapter.RunSpec{}, adapter.ReapPolicy{KeepRuns: 1, Now: time.Now()}); err != nil || len(lines) != 0 {
		t.Errorf("reap of a missing root = %q, %v", lines, err)
	}
}

// Only a host-executor directory act left behind is removed, and only once
// nothing has touched it for a day. act's action clones and its tool cache
// stay, and so does anything else in the directory.
func TestReapRemovesOnlyStaleHostExecutorDirectories(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "act")
	day := ActWorkdirMaxAge
	mkdirAged(t, filepath.Join(cache, "ea7e347e5de3bc16"), 2*day, "act", "hostexecutor", "tmp")     // stale: goes
	mkdirAged(t, filepath.Join(cache, "dc67558ee758d3b2"), time.Hour, "act", "hostexecutor", "tmp") // recent
	mkdirAged(t, filepath.Join(cache, "0123456789abcdef"), 2*day, "act", "hostexecutor", "tmp")     // touched below
	mkdirAged(t, filepath.Join(cache, "fedcba9876543210"), 2*day, "act", "tmp")                     // no hostexecutor
	mkdirAged(t, filepath.Join(cache, "ABCDEF0123456789"), 2*day, "act", "hostexecutor", "tmp")     // not act's spelling
	mkdirAged(t, filepath.Join(cache, "abcdef012345678"), 2*day, "act", "hostexecutor", "tmp")      // fifteen digits
	mkdirAged(t, filepath.Join(cache, "actions-setup-node@v7"), 30*day, "dist")                     // an action clone
	mkdirAged(t, filepath.Join(cache, "tool_cache"), 30*day, "node")                                // the tool cache
	now := time.Now()
	if err := os.Chtimes(filepath.Join(cache, "0123456789abcdef", "tmp"), now, now); err != nil {
		t.Fatal(err)
	}
	// A link with act's spelling, to an old directory that is not act's.
	outside := filepath.Join(t.TempDir(), "elsewhere")
	mkdirAged(t, outside, 2*day, "hostexecutor")
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(cache, "1111111111111111")); err != nil {
			t.Fatal(err)
		}
	}
	a := &Adapter{RecordRoot: t.TempDir(), CacheRoot: cache}

	lines, err := a.ReapRun(adapter.RunSpec{}, adapter.ReapPolicy{KeepRuns: 50, Now: now,
		Live: map[string]time.Time{"run-1": now.Add(-time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	want := "0123456789abcdef,ABCDEF0123456789,abcdef012345678,actions-setup-node@v7,dc67558ee758d3b2,fedcba9876543210,tool_cache"
	if runtime.GOOS != "windows" {
		want = "0123456789abcdef,1111111111111111,ABCDEF0123456789,abcdef012345678,actions-setup-node@v7,dc67558ee758d3b2,fedcba9876543210,tool_cache"
	}
	if got := strings.Join(names(t, cache), ","); got != want {
		t.Errorf("left %s\nwant %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(outside, "hostexecutor")); err != nil {
		t.Errorf("the link's target was touched: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "removed 1 host-executor") {
		t.Errorf("lines = %q", lines)
	}
}

// A run that has been going since before a directory was last touched could
// be the one using it, so the directory stays until that run ends. A live run
// whose start nobody recorded keeps everything.
func TestReapSparesWhatALongLiveRunCouldBeUsing(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "act")
	mkdirAged(t, filepath.Join(cache, "ea7e347e5de3bc16"), 2*ActWorkdirMaxAge, "act", "hostexecutor", "tmp")
	a := &Adapter{RecordRoot: t.TempDir(), CacheRoot: cache}
	now := time.Now()

	for name, started := range map[string]time.Time{
		"a run three days old": now.Add(-3 * ActWorkdirMaxAge),
		"a run with no start":  {},
	} {
		if _, err := a.ReapRun(adapter.RunSpec{}, adapter.ReapPolicy{Now: now,
			Live: map[string]time.Time{"long": started}}); err != nil {
			t.Fatal(err)
		}
		if got := names(t, cache); len(got) != 1 {
			t.Fatalf("with %s live, the directory went: %v", name, got)
		}
	}
	// Once it has ended, the directory is fair game.
	if _, err := a.ReapRun(adapter.RunSpec{}, adapter.ReapPolicy{Now: now}); err != nil {
		t.Fatal(err)
	}
	if got := names(t, cache); len(got) != 0 {
		t.Errorf("left %v", got)
	}
}

// A cache root that is not act's own — relative, or not named act — is never
// swept, whatever is in it.
func TestReapSweepsOnlyADirectoryNamedAct(t *testing.T) {
	for _, root := range []string{t.TempDir(), filepath.Join("relative", "act")} {
		if filepath.IsAbs(root) {
			mkdirAged(t, filepath.Join(root, "ea7e347e5de3bc16"), 2*ActWorkdirMaxAge, "hostexecutor")
		}
		if n, err := pruneActWorkdirs(root, time.Now()); n != 0 || err != nil {
			t.Errorf("%s: removed %d, %v", root, n, err)
		}
	}
}

// act's cache is where act puts it: XDG_CACHE_HOME when set and not empty,
// else the home directory's .cache, then act.
func TestActCacheRootIsWhereActPutsIt(t *testing.T) {
	homeVar := "HOME"
	if runtime.GOOS == "windows" {
		homeVar = "USERPROFILE"
	}
	home, xdg := filepath.FromSlash("/h/me"), filepath.FromSlash("/x/cache")
	for _, tc := range []struct {
		env  []string
		want string
	}{
		{[]string{homeVar + "=" + home, "XDG_CACHE_HOME=" + xdg}, filepath.Join(xdg, "act")},
		{[]string{homeVar + "=" + home, "XDG_CACHE_HOME="}, filepath.Join(home, ".cache", "act")},
		{[]string{homeVar + "=" + home}, filepath.Join(home, ".cache", "act")},
		{[]string{"PATH=/bin"}, ""},
	} {
		if got := actCacheRoot(tc.env); got != tc.want {
			t.Errorf("actCacheRoot(%q) = %q, want %q", tc.env, got, tc.want)
		}
	}
}

// With no CacheRoot set, the reap reads act's cache from the same environment
// Rein hands act — here a temporary XDG_CACHE_HOME, with HOME pointed at a
// temporary directory too, so that nothing real can be reached.
func TestReapFindsActsCacheThroughTheEnvironmentItHandsAct(t *testing.T) {
	home, xdg := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CACHE_HOME", xdg)
	stale := filepath.Join(xdg, "act", "ea7e347e5de3bc16")
	mkdirAged(t, stale, 2*ActWorkdirMaxAge, "act", "hostexecutor", "tmp")
	a := &Adapter{RecordRoot: t.TempDir()}

	if _, err := a.ReapRun(adapter.RunSpec{RunID: "run-1"}, adapter.ReapPolicy{Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale directory under XDG_CACHE_HOME is still there: %v", err)
	}
}
