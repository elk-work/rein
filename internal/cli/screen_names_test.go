package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The screens Elk's apps no longer have. A person following Rein's help or
// output to one of these finds nothing (ark:rein#116).
var retiredElkScreens = []string{"Connected agents", "Connect another agent", "Reveal URL", "reveals it", "Your agents"}

func TestUserFacingWordsNameCurrentElkScreens(t *testing.T) {
	var files []string
	for _, glob := range []string{"*.go", "../../README.md"} {
		found, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, found...)
	}
	if len(files) < 2 {
		t.Fatalf("found too few files to check: %v", files)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, retired := range retiredElkScreens {
			if strings.Contains(string(body), retired) {
				t.Errorf("%s still names %q", file, retired)
			}
		}
	}
}

func TestEnrolHelpNamesTheAgentsPage(t *testing.T) {
	body, err := os.ReadFile("enrol.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Agents →", "Connect an agent", "Agents page"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("enrol.go does not say %q", want)
		}
	}
}
