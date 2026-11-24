package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/elk-work/rein/internal/adapter"
)

// policyRules reads a policy file back as Gemini CLI would.
func policyRules(t *testing.T, blob []byte) []policyRule {
	t.Helper()
	var f struct {
		Rule []policyRule `toml:"rule"`
	}
	if _, err := toml.Decode(string(blob), &f); err != nil {
		t.Fatalf("the policy file is not TOML: %v\n%s", err, blob)
	}
	return f.Rule
}

// denied is Gemini CLI's decision on a tool call, from the rules alone: it
// tests argsPattern against the arguments as JSON (stableStringify).
func denied(t *testing.T, rules []policyRule, tool string, args map[string]string) bool {
	t.Helper()
	blob, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if r.Decision != "deny" || !contains(r.ToolName, tool) {
			continue
		}
		if regexp.MustCompile(r.ArgsPattern).Match(blob) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Gemini CLI 0.64 drops a rule whose pattern looks like a ReDoS risk and runs
// on without it. Measured: a file whose patterns quantified a group, such as
// `(?:/|\\\\)+`, loaded no rule at all.
func TestEveryRuleIsOneGeminiCLILoads(t *testing.T) {
	for _, c := range []struct {
		goos, home string
		folders    []adapter.PersonalFolder
	}{
		{"darwin", "/Users/me", []adapter.PersonalFolder{
			{Name: "Documents", Path: "/Users/me/Documents"},
			{Name: "Library/Mobile Documents", Path: "/Users/me/Library/Mobile Documents"},
		}},
		{"linux", "/home/me", []adapter.PersonalFolder{{Name: "Music", Path: "/home/me/Music"}}},
		{"windows", `C:\Users\me`, []adapter.PersonalFolder{
			{Name: "Documents", Path: `D:\OneDrive\Documents`},
			{Name: "OneDrive", Path: `C:\Users\me\OneDrive - Elk`},
		}},
		{"darwin", "", nil},
	} {
		blob, err := personalFolderPolicy(c.goos, c.home, c.folders)
		if err != nil {
			t.Fatalf("%s: %v", c.goos, err)
		}
		rules := policyRules(t, blob)
		if len(rules) == 0 {
			t.Fatalf("%s %q: no rules", c.goos, c.home)
		}
		for _, r := range rules {
			if !safeForGemini(r.ArgsPattern) {
				t.Errorf("%s: Gemini CLI would drop %s", c.goos, r.ArgsPattern)
			}
			if r.Decision != "deny" || r.Priority != 999 || r.DenyMessage == "" {
				t.Errorf("%s: rule %+v", c.goos, r)
			}
		}
	}
	for _, p := range []string{`(?:/|\\\\)+Documents`, `a(?:\\\\)?\x20b`} {
		if safeForGemini(p) {
			t.Errorf("safeForGemini(%s) = true, Gemini CLI drops it", p)
		}
	}
}

func TestTheRulesStopAToolNamingAPersonalFolder(t *testing.T) {
	blob, err := personalFolderPolicy("darwin", "/Users/me", []adapter.PersonalFolder{
		{Name: "Documents", Path: "/Users/me/Documents"},
		{Name: "Library/Mobile Documents", Path: "/Users/me/Library/Mobile Documents"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rules := policyRules(t, blob)
	for _, c := range []struct {
		tool, key, value string
		want             bool
	}{
		{"run_shell_command", "command", "ls ~/Documents", true},
		{"run_shell_command", "command", `cat "$HOME/documents/a.txt"`, true}, // the Mac disk ignores case
		{"run_shell_command", "command", "ls ${HOME}/Documents/", true},
		{"run_shell_command", "command", "find /Users/me/Documents -name x", true},
		{"run_shell_command", "command", `ls /Users/me/Library/Mobile\ Documents`, true},
		{"read_file", "file_path", "/Users/me/Documents/secret.txt", true},
		{"list_directory", "dir_path", "/Users/me/Documents", true},
		{"run_shell_command", "command", "ls /Users/me/Documents2", false},
		{"run_shell_command", "command", "go test ./...", false},
		{"read_file", "file_path", "internal/Documents.go", false},
		{"mcp_elk_capture", "text", "it lives in ~/Documents", false}, // text for another service
	} {
		if got := denied(t, rules, c.tool, map[string]string{c.key: c.value}); got != c.want {
			t.Errorf("%s %s: denied = %v, want %v", c.tool, c.value, got, c.want)
		}
	}
}

// On Windows a folder is named with backslashes, which the JSON Gemini CLI
// matches writes doubled, and from the profile as cmd and PowerShell name it.
func TestTheWindowsRulesMatchWindowsPaths(t *testing.T) {
	blob, err := personalFolderPolicy("windows", `C:\Users\me`, []adapter.PersonalFolder{
		{Name: "Documents", Path: `C:\Users\me\Documents`},
		{Name: "Documents", Path: `D:\OneDrive\Documents`},
	})
	if err != nil {
		t.Fatal(err)
	}
	rules := policyRules(t, blob)
	for _, c := range []struct {
		command string
		want    bool
	}{
		{`dir C:\Users\me\Documents`, true},
		{`type %USERPROFILE%\documents\a.txt`, true},
		{`Get-ChildItem $env:USERPROFILE\Documents`, true},
		{`ls ~/Documents`, true},
		{`copy x D:\OneDrive\Documents\y`, true},
		{`dir C:/Users/me/Documents`, true},
		{`dir C:\Users\me\Documents2`, false},
		{`go test ./...`, false},
	} {
		if got := denied(t, rules, "run_shell_command", map[string]string{"command": c.command}); got != c.want {
			t.Errorf("%s: denied = %v, want %v", c.command, got, c.want)
		}
	}
}

// With home unknown each folder is still named from `~` and $HOME.
func TestUnknownHomeStillDeniesFromHome(t *testing.T) {
	blob, err := personalFolderPolicy("linux", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rules := policyRules(t, blob)
	if len(rules) != len(adapter.PersonalFolders) {
		t.Fatalf("%d rules, want one per folder", len(rules))
	}
	if !denied(t, rules, "run_shell_command", map[string]string{"command": "ls $HOME/Desktop"}) {
		t.Error("$HOME/Desktop not denied")
	}
}

func TestNothingToDenyWritesNoFile(t *testing.T) {
	blob, err := personalFolderPolicy("darwin", "/Users/me", nil)
	if err != nil || blob != nil {
		t.Fatalf("blob = %q, err = %v; want none", blob, err)
	}
}

// The rules land where Gemini CLI reads the user tier, the private home's
// .gemini/policies, unless the queue allows the folders.
func TestThePrivateHomeCarriesTheRulesUnlessTheQueueAllowsThem(t *testing.T) {
	if _, err := os.UserHomeDir(); err != nil {
		t.Skip("no home directory")
	}
	for _, allowed := range []bool{false, true} {
		a := &Adapter{Root: t.TempDir(), RealHome: t.TempDir()}
		spec := baseSpec(t)
		spec.PersonalFolders = allowed
		h, err := a.newHome(spec, testKey)
		must(t, err)
		blob, err := os.ReadFile(filepath.Join(h.dir, ".gemini", "policies", policyFile))
		switch {
		case allowed && err == nil:
			t.Error("the queue allows the folders, yet the run carries rules")
		case !allowed && err != nil:
			t.Errorf("no rules in the private home: %v", err)
		case !allowed && !strings.Contains(string(blob), `decision = "deny"`):
			t.Errorf("rules = %s", blob)
		}
		h.remove()
	}
}
