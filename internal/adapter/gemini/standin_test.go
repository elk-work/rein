package gemini

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A stand-in for both Google programs that runs on every platform Rein's CI
// covers, Windows included: the test binary itself, copied into a folder
// under the program's name. TestMain sees the name and plays the program
// instead of running the tests. What it does is in standin.json beside it, and
// what it saw it writes beside it too.

// standinConfig is what the stand-in replays.
type standinConfig struct {
	Fixture string `json:"fixture"` // a file of stream-json lines to print
	Exit    int    `json:"exit"`
	Stderr  string `json:"stderr"`   // printed before the fixture
	Hang    bool   `json:"hang"`     // after stderr, wait to be killed
	Version string `json:"version"`  // what --version prints
	Rec     string `json:"rec"`      // where to record what it saw
	NeedKey bool   `json:"need_key"` // play a person whose settings say modelProvider gemini
	Models  string `json:"models"`   // what `agy models` prints
}

// standinRecord is what the stand-in saw.
type standinRecord struct {
	Args  []string `json:"args"`
	Env   []string `json:"env"`
	Stdin string   `json:"stdin"`
	Dir   string   `json:"dir"`
}

func TestMain(m *testing.M) {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if name == AgyBinary || name == Binary {
		os.Exit(runStandin())
	}
	if os.Getenv("REIN_AGY_LIVE") != "1" {
		if err := hideRealAntigravity(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}

// hideRealAntigravity keeps the unit tests from ever finding the Antigravity
// installed on the machine running them, which is signed in to somebody's
// Google account: Rein runs agy wherever it is installed, so a test that names
// no stand-in would otherwise start the real one. HOME (where Google's
// installer puts agy) becomes an empty folder, and every PATH folder holding
// an agy is dropped. The live tests (REIN_AGY_LIVE=1) mean to find it.
func hideRealAntigravity() error {
	home, err := os.MkdirTemp("", "rein-gemini-test-home-")
	if err != nil {
		return err
	}
	for _, k := range []string{"HOME", "USERPROFILE"} {
		os.Setenv(k, home)
	}
	os.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	var keep []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, AgyBinary)); err == nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, AgyBinary+".exe")); err == nil {
			continue
		}
		keep = append(keep, dir)
	}
	os.Setenv("PATH", strings.Join(keep, string(os.PathListSeparator)))
	if _, err := (&Adapter{}).resolveAgy(); err == nil {
		return fmt.Errorf("the real Antigravity is still reachable; refusing to run tests that could start it")
	}
	return nil
}

func runStandin() int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 90
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "standin.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 91
	}
	var c standinConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 92
	}
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(c.Version)
		appendFile(filepath.Join(c.Rec, "versions"), "asked\n")
		return 0
	}
	if len(os.Args) > 1 && os.Args[1] == "models" {
		// agy exits 0 whether or not it is signed in (measured).
		fmt.Println(c.Models)
		appendFile(filepath.Join(c.Rec, "models"), os.Getenv(KeyName)+"\n")
		return 0
	}
	in, _ := io.ReadAll(os.Stdin)
	wd, _ := os.Getwd()
	rec, _ := json.Marshal(standinRecord{Args: os.Args[1:], Env: os.Environ(), Stdin: string(in), Dir: wd})
	_ = os.WriteFile(filepath.Join(c.Rec, "run.json"), rec, 0o600)
	if c.NeedKey && os.Getenv(KeyName) == "" {
		fmt.Fprintln(os.Stderr, `modelProvider is set to "gemini" in settings.json, but the GEMINI_API_KEY environment variable is not set.`)
		return 1
	}
	if c.Stderr != "" {
		fmt.Fprintln(os.Stderr, c.Stderr)
	}
	if c.Hang {
		time.Sleep(60 * time.Second)
		return 0
	}
	if c.Fixture != "" {
		b, err := os.ReadFile(c.Fixture)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 93
		}
		_, _ = os.Stdout.Write(b)
	}
	return c.Exit
}

func appendFile(path, s string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(s)
}

// standin installs the stand-in as program (AgyBinary or Binary) and returns
// its path and the folder it records into.
func standin(t *testing.T, program string, c standinConfig) (bin, rec string) {
	t.Helper()
	dir := t.TempDir()
	rec = filepath.Join(dir, "rec")
	must(t, os.MkdirAll(rec, 0o700))
	self, err := os.Executable()
	must(t, err)
	bin = filepath.Join(dir, program)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	b, err := os.ReadFile(self)
	must(t, err)
	must(t, os.WriteFile(bin, b, 0o755))
	if c.Fixture != "" && !filepath.IsAbs(c.Fixture) {
		c.Fixture, err = filepath.Abs(filepath.Join("testdata", c.Fixture))
		must(t, err)
	}
	if c.Version == "" {
		c.Version = "1.5.5"
	}
	c.Rec = rec
	blob, err := json.Marshal(c)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "standin.json"), blob, 0o600))
	return bin, rec
}

func readRecord(t *testing.T, rec string) standinRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(rec, "run.json"))
	if err != nil {
		t.Fatalf("the stand-in never ran: %v", err)
	}
	var r standinRecord
	must(t, json.Unmarshal(raw, &r))
	return r
}

func envValue(env []string, name string) (string, int) {
	var v string
	n := 0
	for _, kv := range env {
		k, val, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, name) {
			v = val
			n++
		}
	}
	return v, n
}

// agyOn lets a test drive the Antigravity stand-in on Linux too, where Rein
// does not drive the real one yet.
func agyOn(t *testing.T) {
	t.Helper()
	agyLinux = true
	t.Cleanup(func() { agyLinux = false })
}

func readRecordErr(rec string) (standinRecord, error) {
	var r standinRecord
	raw, err := os.ReadFile(filepath.Join(rec, "run.json"))
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(raw, &r)
}
