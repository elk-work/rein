package adapter

import (
	"path/filepath"
	"slices"
	"testing"
)

func names(fs []PersonalFolder) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name+"="+f.Path)
	}
	return out
}

// On a Mac the folders are under home, and with no home there are none.
func TestPersonalFoldersAreUnderHomeOffWindows(t *testing.T) {
	home := t.TempDir()
	none := func(string) string { return "" }
	got := personalFolderPaths("darwin", home, none, none)
	if len(got) != len(PersonalFolders) {
		t.Fatalf("got %v", names(got))
	}
	if got[0] != (PersonalFolder{"Documents", filepath.Join(home, "Documents")}) {
		t.Errorf("first folder %v", got[0])
	}
	if want := filepath.Join(home, "Library", "Mobile Documents"); got[len(got)-1].Path != want {
		t.Errorf("iCloud Drive at %s, want %s", got[len(got)-1].Path, want)
	}
	if got := personalFolderPaths("darwin", "", none, none); got != nil {
		t.Errorf("no home gave %v", names(got))
	}
}

// On Windows each folder is where the known-folder lookup says, so a
// Documents and Pictures OneDrive moved are followed there; the usual place
// in the profile is kept too, a folder the lookup left alone appears once,
// and OneDrive's own folders join the list.
func TestWindowsPersonalFoldersFollowOneDrive(t *testing.T) {
	home := t.TempDir()
	onedrive := filepath.Join(t.TempDir(), "OneDrive")
	work := filepath.Join(t.TempDir(), "OneDrive - Elk")
	known := func(name string) string {
		switch name {
		case "Documents", "Pictures":
			return filepath.Join(onedrive, name)
		case "Desktop":
			return filepath.Join(home, "Desktop")
		}
		return ""
	}
	env := map[string]string{"OneDrive": onedrive, "OneDriveConsumer": onedrive, "OneDriveCommercial": work}
	got := names(personalFolderPaths("windows", home, known, func(k string) string { return env[k] }))
	want := []string{
		"Documents=" + filepath.Join(onedrive, "Documents"),
		"Documents=" + filepath.Join(home, "Documents"),
		"Desktop=" + filepath.Join(home, "Desktop"),
		"Downloads=" + filepath.Join(home, "Downloads"),
		"Pictures=" + filepath.Join(onedrive, "Pictures"),
		"Pictures=" + filepath.Join(home, "Pictures"),
		"Videos=" + filepath.Join(home, "Videos"),
		"Music=" + filepath.Join(home, "Music"),
		"OneDrive=" + onedrive,
		"OneDrive=" + work,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}

	// No home: the known folders alone.
	got = names(personalFolderPaths("windows", "", known, func(string) string { return "" }))
	if !slices.Equal(got, []string{
		"Documents=" + filepath.Join(onedrive, "Documents"),
		"Desktop=" + filepath.Join(home, "Desktop"),
		"Pictures=" + filepath.Join(onedrive, "Pictures"),
	}) {
		t.Errorf("no home gave %v", got)
	}
}
