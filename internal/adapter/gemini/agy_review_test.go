package gemini

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

// Antigravity takes no MCP server from Rein, so a run that brings one is
// refused rather than run without it.
func TestAnAntigravityRunWithAnMCPServerIsRefused(t *testing.T) {
	agyOn(t)
	agyBin, agyRec := standin(t, AgyBinary, standinConfig{Fixture: "agy-tool.jsonl"})
	a := agyAdapter(t, agyBin, "")
	spec := baseSpec(t)
	spec.MCPServers = map[string]any{"github": map[string]any{}}
	if _, err := a.Start(t.Context(), spec); !errors.Is(err, adapter.ErrCapability) {
		t.Fatalf("err=%v, want a capability refusal", err)
	}
	if _, err := readRecordErr(agyRec); err == nil {
		t.Fatal("Antigravity ran")
	}
}

// One program's conversation never reaches the other: Antigravity can be
// installed or removed between a run's first round and its follow-up.
func TestAConversationOnlyResumesOnTheProgramThatStartedIt(t *testing.T) {
	agyOn(t)
	agyBin, agyRec := standin(t, AgyBinary, standinConfig{Fixture: "agy-tool.jsonl"})
	gemBin, _ := standin(t, Binary, standinConfig{Fixture: "text.jsonl", Version: "0.64.0"})

	spec := baseSpec(t)
	spec.ResumeID = agyResumePrefix + "conv-1"
	if _, err := agyAdapter(t, "", gemBin).Start(t.Context(), spec); !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("Antigravity's conversation without Antigravity: err=%v", err)
	}

	a := agyAdapter(t, agyBin, gemBin)
	spec.ResumeID = "a-gemini-cli-session"
	if _, err := a.Start(t.Context(), spec); !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("Gemini CLI's session on Antigravity: err=%v", err)
	}

	spec.ResumeID = agyResumePrefix + "conv & calc"
	if _, err := a.Start(t.Context(), spec); !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("a conversation id that is not a plain name: err=%v", err)
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
	agyOn(t)
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

// Linux is left out until Antigravity's sandbox is measured there.
func TestAntigravityIsNotDrivenOnLinuxYet(t *testing.T) {
	agyOn(t)
	agyLinux = false
	if slices.Contains(agyPlatforms(), adapter.AnyArch("linux")) {
		t.Fatal("Antigravity lists Linux")
	}
	if runtime.GOOS == "linux" {
		agyBin, _ := standin(t, AgyBinary, standinConfig{Fixture: "agy-tool.jsonl"})
		if m := agyAdapter(t, agyBin, "").Manifest(); m.Binary != Binary {
			t.Fatalf("Linux drives %q", m.Binary)
		}
	}
}
