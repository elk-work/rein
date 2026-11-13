package adapter

// PersonalFolders are the person's own folders, relative to home, that a
// worker stays out of unless its queue allows them (config
// Queue.PersonalFolders; Elk #1832, Casey's ruling 2026-11-12). macOS guards
// each one and asks, with no warning and no reason Elk can show, the moment a
// background agent touches it. Library as a whole is left alone: agent tools
// read caches and settings there. Only iCloud Drive inside it is a person's
// files.
var PersonalFolders = []string{
	"Documents", "Desktop", "Downloads", "Pictures", "Movies", "Music",
	"Library/Mobile Documents",
}
