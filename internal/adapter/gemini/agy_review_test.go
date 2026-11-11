package gemini

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// The switch is each gemini queue's say, and it fails closed: one queue's
// "off" outvotes another's "on", and a say lapses when its queue stops
// hearing from Elk.
func TestTheSwitchNeedsEveryQueueAndLapses(t *testing.T) {
	ResetAntigravity()
	t.Cleanup(func() { sw.mu.Lock(); sw.now = nil; sw.mu.Unlock(); ResetAntigravity() })
	clock := time.Unix(1_800_000_000, 0)
	sw.mu.Lock()
	sw.now = func() time.Time { return clock }
	sw.mu.Unlock()

	if AntigravityOn() {
		t.Fatal("on with no queue having said anything")
	}
	gen := SwitchGeneration()
	SetAntigravity("a", true)
	if !AntigravityOn() {
		t.Fatal("one queue saying on did not turn it on")
	}
	if SwitchGeneration() == gen {
		t.Fatal("turning on did not change the generation")
	}
	SetAntigravity("b", false)
	if AntigravityOn() {
		t.Fatal("two Elk servers disagree and it is on")
	}
	SetAntigravity("b", true)
	if !AntigravityOn() {
		t.Fatal("both on and it is off")
	}
	// b keeps hearing; a goes quiet past SwitchFresh: a's say lapses, b's stands.
	clock = clock.Add(SwitchFresh / 2)
	SetAntigravity("b", true)
	clock = clock.Add(SwitchFresh/2 + time.Second)
	if !AntigravityOn() {
		t.Fatal("a lapsed say from a stopped queue blocked the switch")
	}
	// Then b goes quiet too: nothing counts, so it is off.
	gen = SwitchGeneration()
	clock = clock.Add(SwitchFresh)
	if AntigravityOn() {
		t.Fatal("still on with every heartbeat failing")
	}
	if SwitchGeneration() == gen {
		t.Fatal("lapsing off did not change the generation")
	}
}

// The Elk run connector, which the runner adds while the switch is off, is
// left out of an Antigravity run with a line that says so; it does not refuse
// the run. Any other server still does.
func TestAnAntigravityRunGoesWithoutTheRunConnectorAndSaysSo(t *testing.T) {
	switchOn(t)
	agyBin, _ := standin(t, AgyBinary, standinConfig{Fixture: "agy-tool.jsonl"})
	a := agyAdapter(t, agyBin, "")
	spec := baseSpec(t)
	spec.MCPServers = map[string]any{adapter.RunConnectorName: map[string]any{"url": "https://elk.example/mcp"}}
	evs, res, err := runToEnd(t, a, spec)
	if err != nil || res.Status != adapter.StatusSucceeded {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	said := false
	for _, ev := range evs {
		said = said || strings.Contains(ev.Text, "without Elk's run connector")
	}
	if !said {
		t.Fatal("the run connector was dropped without a word")
	}

	spec = baseSpec(t)
	spec.MCPServers = map[string]any{"github": map[string]any{}}
	if _, err := a.Start(t.Context(), spec); !errors.Is(err, adapter.ErrCapability) {
		t.Fatalf("another MCP server: err=%v, want a capability refusal", err)
	}
}

// One program's conversation never reaches the other: the switch can flip
// between a run's first round and its follow-up.
func TestAConversationOnlyResumesOnTheProgramThatStartedIt(t *testing.T) {
	agyBin, agyRec := standin(t, AgyBinary, standinConfig{Fixture: "agy-tool.jsonl"})
	gemBin, _ := standin(t, Binary, standinConfig{Fixture: "text.jsonl", Version: "0.64.0"})
	a := agyAdapter(t, agyBin, gemBin)

	ResetAntigravity()
	spec := baseSpec(t)
	spec.ResumeID = agyResumePrefix + "conv-1"
	if _, err := a.Start(t.Context(), spec); !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("Antigravity's conversation with the switch off: err=%v", err)
	}

	switchOn(t)
	spec.ResumeID = "a-gemini-cli-session"
	if _, err := a.Start(t.Context(), spec); !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("Gemini CLI's session on Antigravity: err=%v", err)
	}

	spec.ResumeID = agyResumePrefix + "conv-1"
	if _, _, err := runToEnd(t, a, spec); err != nil {
		t.Fatal(err)
	}
	args := readRecord(t, agyRec).Args
	if i := slices.Index(args, "--conversation"); i < 0 || args[i+1] != "conv-1" {
		t.Fatalf("args %q", args)
	}
}

// An Antigravity older than the one Rein measured is not driven: Gemini CLI
// runs instead.
func TestAnOldAntigravityIsPassedOver(t *testing.T) {
	switchOn(t)
	agyBin, agyRec := standin(t, AgyBinary, standinConfig{Fixture: "agy-tool.jsonl", Version: "1.4.9"})
	gemBin, gemRec := standin(t, Binary, standinConfig{Fixture: "text.jsonl", Version: "0.64.0"})
	a := agyAdapter(t, agyBin, gemBin)
	if m := a.Manifest(); m.Binary != Binary {
		t.Fatalf("manifest is %q's", m.Binary)
	}
	if _, _, err := runToEnd(t, a, baseSpec(t)); err != nil {
		t.Fatal(err)
	}
	readRecord(t, gemRec)
	if _, err := readRecordErr(agyRec); err == nil {
		t.Fatal("the old Antigravity ran")
	}
	for v, want := range map[string]bool{"1.5.5": true, "agy 1.6.0 (abc)": true, "2.0.0": true, "1.5.4": false, "": false, "dev": false} {
		if got := versionAtLeast(v, agyMinVersion); got != want {
			t.Errorf("versionAtLeast(%q) = %v", v, got)
		}
	}
}

// A model or effort that is not a plain name never reaches a command line:
// on Windows, Gemini CLI's npm shim goes through cmd.exe.
func TestAModelThatIsNotAPlainNameIsRefused(t *testing.T) {
	ResetAntigravity()
	gemBin, gemRec := standin(t, Binary, standinConfig{Fixture: "text.jsonl", Version: "0.64.0"})
	a := agyAdapter(t, "", gemBin)
	for _, m := range []string{"gemini & calc", "x|y", "a\"b", "-rf"} {
		spec := baseSpec(t)
		spec.Model = m
		if _, err := a.Start(t.Context(), spec); !errors.Is(err, adapter.ErrNotSupported) {
			t.Fatalf("model %q: err=%v", m, err)
		}
	}
	if _, err := readRecordErr(gemRec); err == nil {
		t.Fatal("Gemini CLI ran with a bad model")
	}
	spec := baseSpec(t)
	spec.Model = "models/gemini-3.1-pro-preview"
	if _, _, err := runToEnd(t, a, spec); err != nil {
		t.Fatal(err)
	}
}

// Google's installer's folder is where Rein looks when agy is not on PATH:
// ~/.local/bin on a Mac or Linux, %LOCALAPPDATA%\agy\bin on Windows.
func TestAntigravityIsFoundWhereGooglesInstallerPutsIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("PATH", t.TempDir())
	want := filepath.Join(home, ".local", "bin", AgyBinary)
	if runtime.GOOS == "windows" {
		want = filepath.Join(home, "AppData", "Local", "agy", "bin", AgyBinary+".exe")
	}
	if got := agyInstalledPath(); got != want {
		t.Fatalf("looks in %q, want %q", got, want)
	}
	must(t, os.MkdirAll(filepath.Dir(want), 0o700))
	must(t, os.WriteFile(want, []byte("x"), 0o755))
	got, err := (&Adapter{}).resolveAgy()
	if err != nil || got != want {
		t.Fatalf("resolveAgy = %q, %v", got, err)
	}
}
