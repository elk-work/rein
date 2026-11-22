package adapter

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The real lookup returns this machine's Documents where Windows says it is,
// which is where OneDrive's folder backup puts it when that is on.
func TestRealKnownFoldersAreWhereWindowsSays(t *testing.T) {
	want, err := windows.KnownFolderPath(windows.FOLDERID_Documents, 0)
	if err != nil {
		t.Skipf("no Documents known folder: %v", err)
	}
	if got := knownFolder("Documents"); got != want {
		t.Errorf("knownFolder(Documents) = %q, want %q", got, want)
	}
	found := false
	for _, f := range PersonalFolderPaths("") {
		if f.Name == "Documents" && strings.EqualFold(f.Path, filepath.Clean(want)) {
			found = true
		}
	}
	if !found {
		t.Errorf("PersonalFolderPaths lacks Documents at %s: %v", want, names(PersonalFolderPaths("")))
	}
	if knownFolder("Movies") != "" {
		t.Error("a Mac folder name found a Windows known folder")
	}
}
