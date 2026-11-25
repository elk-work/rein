package runner

import (
	"regexp"
	"strings"
)

// What a finished run leaves for a person (scout run_outcome.ts): `none`,
// `decision` or `check`. Rein submits every run, so an agent cannot pass
// `person_next` itself; it writes one line in its deliverable instead, and
// Rein carries the value to Elk and takes the line out of the report.
//
// Evidence (Elk Scout #2242, run arun-dc0de89d): "Rein submits the run, so I
// can't set `person_next` myself: it should be check, not none." Without the
// line Elk reads the report's opener, and a check stated only in prose closed
// its task with the check undone.

// personNextLineRE is the line an agent writes on purpose: "Person next:
// check", case-insensitive, markdown emphasis and a closing full stop allowed.
// Only the three values count; any other value leaves the line in the report.
var personNextLineRE = regexp.MustCompile(`(?i)^person[ _-]next\s*:\s*(none|decision|check)\.?$`)

// takePersonNext finds the agent's "Person next:" line outside code blocks,
// and returns the deliverable without it and the value it names. With no such
// line it returns the deliverable unchanged and "". When the agent wrote more
// than one, the last one wins and every one is removed.
func takePersonNext(deliverable string) (string, string) {
	lines := strings.Split(deliverable, "\n")
	kept := make([]string, 0, len(lines))
	kind := ""
	fence := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			kept = append(kept, line)
			continue
		}
		if f := fenceOpener(trimmed); f != "" {
			fence = f
			kept = append(kept, line)
			continue
		}
		bare := strings.TrimSpace(strings.Trim(trimmed, "*_`"))
		bare = strings.Replace(bare, ":**", ":", 1)
		bare = strings.Replace(bare, ":__", ":", 1)
		bare = strings.TrimSpace(strings.Trim(bare, "*_`"))
		if m := personNextLineRE.FindStringSubmatch(bare); m != nil {
			kind = strings.ToLower(m[1])
			// Removing the line must not leave two blank lines where it was.
			if n := len(kept); n > 0 && strings.TrimSpace(kept[n-1]) == "" {
				kept = kept[:n-1]
			}
			continue
		}
		kept = append(kept, line)
	}
	if kind == "" {
		return deliverable, ""
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), kind
}

// fenceOpener is the fence a line opens a code block with, or "".
func fenceOpener(trimmed string) string {
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, f) {
			return f
		}
	}
	return ""
}
