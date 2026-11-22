package claude

import (
	"slices"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

// Claude Code matches Windows paths in POSIX form, the drive letter a leading
// folder, so a rule for a Documents that OneDrive moved to D: names //d/...
func TestWindowsPathsAreWrittenInTheFormClaudeCodeMatches(t *testing.T) {
	for in, want := range map[string]string{
		`D:\OneDrive\Documents`:           "/d/OneDrive/Documents",
		`C:\Users\me\OneDrive\Documents\`: "/c/Users/me/OneDrive/Documents",
	} {
		if got := posixPath(in); got != want {
			t.Errorf("posixPath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ruleRoot(`C:\Users\me`, `c:\users\me\OneDrive\Documents`); got != "~/OneDrive/Documents" {
		t.Errorf("a folder under home in another case: %q", got)
	}
	if got := ruleRoot(`C:\Users\me`, `D:\OneDrive\Documents`); got != "//d/OneDrive/Documents" {
		t.Errorf("a folder on another drive: %q", got)
	}

	useFolders(t, func(string) []adapter.PersonalFolder {
		return []adapter.PersonalFolder{{Name: "Documents", Path: `D:\OneDrive\Documents`}}
	})
	deny := personalFolderDenies(`C:\Users\me`, `C:\Users\me\.rein\work\run1`)
	if !slices.Equal(deny, []string{
		"Read(//d/OneDrive/Documents)", "Read(//d/OneDrive/Documents/**)",
		"Edit(//d/OneDrive/Documents)", "Edit(//d/OneDrive/Documents/**)",
	}) {
		t.Errorf("deny = %v", deny)
	}
}

// With the real lookup, the deny list names wherever this machine's Documents
// really is, OneDrive or not.
func TestTheRealDocumentsFolderIsDenied(t *testing.T) {
	var docs string
	for _, f := range adapter.PersonalFolderPaths("") {
		if f.Name == "Documents" {
			docs = f.Path
			break
		}
	}
	if docs == "" {
		t.Skip("this machine has no Documents known folder")
	}
	deny := personalFolderDenies("", `Z:\nowhere\run1`)
	if want := "Read(/" + posixPath(docs) + "/**)"; !slices.Contains(deny, want) {
		t.Errorf("missing %s in %v", want, deny)
	}
}
