package adapter

import "golang.org/x/sys/windows"

var knownFolderIDs = map[string]*windows.KNOWNFOLDERID{
	"Documents": windows.FOLDERID_Documents,
	"Desktop":   windows.FOLDERID_Desktop,
	"Downloads": windows.FOLDERID_Downloads,
	"Pictures":  windows.FOLDERID_Pictures,
	"Videos":    windows.FOLDERID_Videos,
	"Music":     windows.FOLDERID_Music,
}

// knownFolder is where Windows says one of WindowsPersonalFolders is now,
// following a move by the person or by OneDrive's folder backup. Flags 0
// asks for the current path without creating the folder.
func knownFolder(name string) string {
	id, ok := knownFolderIDs[name]
	if !ok {
		return ""
	}
	path, err := windows.KnownFolderPath(id, 0)
	if err != nil {
		return ""
	}
	return path
}
