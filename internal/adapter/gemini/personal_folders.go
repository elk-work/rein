package gemini

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/elk-work/rein/internal/adapter"
)

// Keeping a run out of the person's own folders (Elk #1832, ark:rein#162).
//
// Gemini CLI has no read-deny setting, but its policy engine does: a `deny`
// rule refuses a tool call whose arguments match a regular expression, in
// every approval mode, yolo included (geminicli.com/docs/reference/policy-engine).
// The rule file goes in the private home's `.gemini/policies`, the user tier,
// which outranks the default tier where yolo's allow-everything rule lives
// (Gemini CLI 0.64: `policies/yolo.toml`, priority 998 at tier 1; a user rule
// at priority 999 is 4.999). The workspace tier is switched off in Gemini CLI,
// so a repository cannot outrank it.
//
// Its file tools already refuse a path outside the workspace ("Path not in
// workspace"); the rules cover them anyway, and they are what stops
// `run_shell_command` naming one of the folders. Like the Claude Code deny
// rules this is a default, not a sandbox: a command that reaches a folder
// without naming it (`cd ~; cat */x`) is not matched, and nor is a folder
// named through a variable the rules do not know.

// policyFile is the rule file's name in the private home's policies folder.
const policyFile = "rein-personal-folders.toml"

// policyTools are Gemini CLI's built-in tools that take a path or a command
// (Gemini CLI 0.64, docs/reference/tools.md). MCP tools are left alone: their
// arguments are text for another service, and a deliverable that mentions a
// folder is not a worker reading it.
var policyTools = []string{
	"run_shell_command", "read_file", "read_many_files", "write_file", "replace",
	"list_directory", "glob", "grep_search", "search_file_content",
}

const policyDenyMessage = "Rein keeps this worker out of the person's own folders (Documents, Desktop, " +
	"Downloads, Pictures, Movies or Videos, Music, iCloud Drive, OneDrive). The person can allow them " +
	"for this queue with `rein enrol --update --personal-folders`; until then, work inside the repository."

// policyRule is one `[[rule]]` of a Gemini CLI policy file.
type policyRule struct {
	ToolName    []string `toml:"toolName"`
	ArgsPattern string   `toml:"argsPattern"`
	Decision    string   `toml:"decision"`
	Priority    int      `toml:"priority"`
	DenyMessage string   `toml:"denyMessage"`
}

// writePersonalFolderPolicy writes the deny rules into conf (the private
// home's `.gemini`), unless the queue allows the folders.
func writePersonalFolderPolicy(conf string, spec adapter.RunSpec) error {
	if spec.PersonalFolders {
		return nil
	}
	home, _ := os.UserHomeDir() // unknown home: the `~/` forms only
	blob, err := personalFolderPolicy(runtime.GOOS, home, adapter.DeniedPersonalFoldersForRun(home, spec.WorktreeDir))
	if err != nil || blob == nil {
		return err
	}
	dir := filepath.Join(conf, "policies")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("writing the personal-folder rules: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, policyFile), blob, 0o600); err != nil {
		return fmt.Errorf("writing the personal-folder rules: %w", err)
	}
	return nil
}

// personalFolderPolicy is the policy file: one deny rule per folder. With no
// folder's place known (home unknown), each folder is named from `~` and
// $HOME, which the shell resolves. Nil when there is nothing to deny: every
// folder holds the run's own repository.
func personalFolderPolicy(goos, home string, folders []adapter.PersonalFolder) ([]byte, error) {
	var patterns []string
	if home == "" && len(folders) == 0 {
		for _, f := range adapter.PersonalFolders {
			patterns = append(patterns, folderPattern(goos, nil, homeForms(goos), strings.Split(f, "/")))
		}
	}
	for _, f := range folders {
		patterns = append(patterns, folderPattern(goos, splitPath(f.Path), nil, nil))
		if rel, ok := under(goos, home, f.Path); ok {
			patterns = append(patterns, folderPattern(goos, nil, homeForms(goos), rel))
		}
	}
	if len(patterns) == 0 {
		return nil, nil
	}
	var rules []policyRule
	for _, p := range patterns {
		if !safeForGemini(p) {
			// A rule Gemini CLI would drop is no rule: never run on it.
			return nil, fmt.Errorf("gemini: a personal-folder rule Gemini CLI would refuse to load: %s", p)
		}
		rules = append(rules, policyRule{ToolName: policyTools, ArgsPattern: p, Decision: "deny", Priority: 999, DenyMessage: policyDenyMessage})
	}
	var b bytes.Buffer
	b.WriteString("# Written by Rein for this run: keeps it out of the person's own folders.\n")
	if err := toml.NewEncoder(&b).Encode(map[string]any{"rule": rules}); err != nil {
		return nil, fmt.Errorf("encoding the personal-folder rules: %w", err)
	}
	return b.Bytes(), nil
}

// homeForms are the ways a command names home: `~` and $HOME everywhere,
// and on Windows the profile as cmd and PowerShell name it.
func homeForms(goos string) []string {
	forms := []string{"~", "$HOME", "${HOME}"}
	if goos == "windows" {
		forms = append(forms, "%USERPROFILE%", "$env:USERPROFILE", "${env:USERPROFILE}")
	}
	return forms
}

// under is path's segments below home, when it is under home.
func under(goos, home, path string) ([]string, bool) {
	if home == "" {
		return nil, false
	}
	h, p := splitPath(home), splitPath(path)
	if len(p) <= len(h) {
		return nil, false
	}
	for i := range h {
		if !sameSegment(goos, h[i], p[i]) {
			return nil, false
		}
	}
	return p[len(h):], true
}

func sameSegment(goos, a, b string) bool {
	if goos == "darwin" || goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// splitPath is path's segments, a drive letter first on Windows.
func splitPath(path string) []string {
	var out []string
	for _, s := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		out = append(out, s)
	}
	return out
}

// Gemini CLI tests a rule's argsPattern against the call's arguments as JSON
// (a JavaScript RegExp, so only the syntax RE2 and JavaScript share is used).
// In that JSON a backslash is written `\\`, which the classes below match.
//
// No group carries a quantifier. Gemini CLI drops, with only a line in its
// debug log, every rule whose pattern looks to it like a ReDoS risk: one
// matching `\([^)]*[*+?{].*\)[*+?{]`, or longer than 2048 characters
// (isSafeRegExp, Gemini CLI 0.64). `(?:/|\\\\)+` is such a pattern, and a
// file of them loaded no rule at all. safeForGemini checks every pattern.
const (
	jsonSep   = `[/\\]+`                // a path separator: / or a JSON-escaped \
	jsonSpace = `[\\ ]+`                // a space, or one a shell escaped (`Mobile\ Documents`)
	jsonEnd   = `(?:[^A-Za-z0-9_.-]|$)` // the folder itself, not Documents2
)

// geminiUnsafe is Gemini CLI's own ReDoS test (isSafeRegExp).
var geminiUnsafe = regexp.MustCompile(`\([^)]*[*+?{].*\)[*+?{]`)

// safeForGemini reports whether Gemini CLI will load pattern rather than drop
// the rule.
func safeForGemini(pattern string) bool {
	if _, err := regexp.Compile(pattern); err != nil {
		return false
	}
	return len(pattern) <= 2048 && !geminiUnsafe.MatchString(pattern)
}

// folderPattern matches a folder named in a tool call's arguments: by its
// absolute path (abs, without a home form), or from one of homeForms (rel).
func folderPattern(goos string, abs, forms, rel []string) string {
	seg := func(s string) string {
		var parts []string
		for _, w := range strings.Split(s, " ") {
			parts = append(parts, caseless(goos, regexp.QuoteMeta(w)))
		}
		return strings.Join(parts, jsonSpace)
	}
	join := func(segs []string) string {
		var parts []string
		for _, s := range segs {
			parts = append(parts, seg(s))
		}
		return strings.Join(parts, jsonSep)
	}
	if abs != nil {
		lead := ""
		if goos != "windows" {
			lead = jsonSep
		}
		return lead + join(abs) + jsonEnd
	}
	var alts []string
	for _, f := range forms {
		alts = append(alts, regexp.QuoteMeta(f))
	}
	return `(?:` + strings.Join(alts, "|") + `)` + jsonSep + join(rel) + jsonEnd
}

// caseless makes ASCII letters match either case where the disk ignores
// case (macOS, Windows): `ls ~/documents` reaches the same folder there.
func caseless(goos, quoted string) string {
	if goos != "darwin" && goos != "windows" {
		return quoted
	}
	var b strings.Builder
	for _, r := range quoted {
		switch {
		case r >= 'a' && r <= 'z':
			fmt.Fprintf(&b, "[%c%c]", r, r-'a'+'A')
		case r >= 'A' && r <= 'Z':
			fmt.Fprintf(&b, "[%c%c]", r-'A'+'a', r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
