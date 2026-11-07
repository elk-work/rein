package runner

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
)

// didLine says in plain words what ran since the last report: the files
// edited (up to three), the commands, and any other tools. "" when nothing.
func didLine(tools, commands, files []string) string {
	var parts []string
	if names := distinct(files); len(names) > 0 {
		s := "edited " + strings.Join(names[:min(3, len(names))], ", ")
		if len(names) > 3 {
			s += " and " + strconv.Itoa(len(names)-3) + " more"
		}
		parts = append(parts, s)
	}
	for _, c := range commands {
		parts = append(parts, commandWords(c))
	}
	if names := distinct(tools); len(names) > 0 {
		parts = append(parts, "ran "+strings.Join(names, ", "))
	}
	parts = distinct(parts)
	if len(parts) == 0 {
		return ""
	}
	line := strings.Join(parts, " · ")
	return strings.ToUpper(line[:1]) + line[1:]
}

// commandWords is a short verb for a shell command where one is obvious.
func commandWords(cmd string) string {
	for _, t := range []string{"go test", "npm test", "swift test", "pytest"} {
		if strings.Contains(cmd, t) {
			return "ran the tests"
		}
	}
	return "ran " + strings.Fields(cmd)[0]
}

// editedFile is the base name of the file an edit tool wrote, or "".
// ponytail: matches tool names on edit/write; an adapter whose edit tool is
// named otherwise (Codex's apply_patch) reads as "ran <tool>".
func editedFile(t *adapter.ToolUse) string {
	if t == nil || len(t.Input) == 0 {
		return ""
	}
	if n := strings.ToLower(t.Name); !strings.Contains(n, "edit") && !strings.Contains(n, "write") {
		return ""
	}
	var in struct {
		FilePath     string `json:"file_path"`
		Path         string `json:"path"`
		NotebookPath string `json:"notebook_path"`
	}
	if json.Unmarshal(t.Input, &in) != nil {
		return ""
	}
	for _, p := range []string{in.FilePath, in.Path, in.NotebookPath} {
		if p = strings.TrimSpace(p); p != "" {
			return filepath.Base(p)
		}
	}
	return ""
}

// whyNext reads the agent's last `Why:` and `Next:` lines, prefixes
// case-insensitive and stripped. Missing ones come back "".
func whyNext(text string) (why, next string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 5 {
			continue
		}
		switch strings.ToLower(line[:4]) {
		case "why:":
			why = strings.TrimSpace(line[4:])
		case "next":
			if line[4] == ':' {
				next = strings.TrimSpace(line[5:])
			}
		}
	}
	return why, next
}

func distinct(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
