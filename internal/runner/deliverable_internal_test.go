package runner

import (
	"strings"
	"testing"
)

func TestWeakDeliverable(t *testing.T) {
	long := strings.Repeat("Merged elk-work/scout#2171 and applied migration 0505. ", 6)
	for _, c := range []struct {
		name, text, prev string
		weak             bool
	}{
		{"empty", "  \n", "", true},
		{"short", "Done.", "", true},
		{"promise", "The pull request is open and its checks are running. I'll merge it once they pass. " +
			strings.Repeat("More words to clear the length bar. ", 4), "", true},
		{"waiting", "Auto-merge is set on the pull request, so it lands when its checks pass. I'm waiting " +
			"for that result before I write the final report. " + strings.Repeat("Padding padding. ", 6), "", true},
		{"repeat", long, long, true},
		{"real", long, "", false},
		{"long with a to-do", long + strings.Repeat(" Left: I'll apply the migration next.", 8), "", false},
	} {
		if got := weakDeliverable(c.text, c.prev) != ""; got != c.weak {
			t.Errorf("%s: weak = %v, want %v (%q)", c.name, got, c.weak, weakDeliverable(c.text, c.prev))
		}
	}
}

func TestPRRefs(t *testing.T) {
	got := prRefs("gh pr checks 2171 -R elk-work/scout --watch; gh pr merge -R elk-work/rein --auto 12; " +
		"see https://github.com/elk-work/elk/pull/400 and https://github.com/elk-work/elk/pull/400")
	want := []string{
		"https://github.com/elk-work/elk/pull/400",
		"https://github.com/elk-work/scout/pull/2171",
		"https://github.com/elk-work/rein/pull/12",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("prRefs = %v, want %v", got, want)
	}
}
