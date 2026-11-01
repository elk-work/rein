package check

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ci/artifacts.yml: what a build suite declares it produced (ark:rein#100
// (01M6AMERCA97G5D02P6HEGCDFZ)).

const declaredFile = `# ci/artifacts.yml
version: 1

suites:
  build:
    kind: worker
    path: worker.js        # copied into $REIN_ARTIFACT_DIR by the job
  image:
    kind: "container"
    image: 'elk-app:ci'
  saved:
    kind: container
    path: out/image.tar
`

func TestParseDeclarations(t *testing.T) {
	got, err := parseDeclarations(declaredFile)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Declaration{
		"build": {Suite: "build", Kind: KindWorker, Path: "worker.js"},
		"image": {Suite: "image", Kind: KindContainer, Image: "elk-app:ci"},
		"saved": {Suite: "saved", Kind: KindContainer, Path: "out/image.tar"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDeclarations =\n%+v\nwant\n%+v", got, want)
	}
}

// A declaration Rein cannot honour is an error naming it, never a guess: a
// typo in `path` that quietly recorded nothing would look, from Elk, exactly
// like a build that produced nothing.
func TestParseDeclarationsRefuses(t *testing.T) {
	head := "version: 1\nsuites:\n  build:\n"
	for name, text := range map[string]string{
		"an unknown field":        head + "    kind: worker\n    paht: worker.js\n",
		"an unknown kind":         head + "    kind: lambda\n    path: worker.js\n",
		"a worker image":          head + "    kind: worker\n    image: x:y\n",
		"neither path nor image":  head + "    kind: container\n",
		"both path and image":     head + "    kind: container\n    path: a.tar\n    image: x:y\n",
		"a path above the dir":    head + "    kind: worker\n    path: ../worker.js\n",
		"an absolute path":        head + "    kind: worker\n    path: /etc/passwd\n",
		"an unclean path":         head + "    kind: worker\n    path: a//worker.js\n",
		"a field set twice":       head + "    kind: worker\n    kind: worker\n    path: w.js\n",
		"a suite declared twice":  head + "    kind: worker\n    path: w.js\n  build:\n    kind: worker\n    path: w.js\n",
		"no version":              "suites:\n  build:\n    kind: worker\n    path: w.js\n",
		"another version":         "version: 2\nsuites:\n  build:\n    kind: worker\n    path: w.js\n",
		"a tab":                   head + "\tkind: worker\n",
		"odd indentation":         head + "   kind: worker\n",
		"an unknown top-level":    "version: 1\nartifacts:\n",
		"a bad image":             head + "    kind: container\n    image: \"-rm x\"\n",
		"a value-less field":      head + "    kind:\n",
		"a suite with a value":    "version: 1\nsuites:\n  build: worker\n",
		"suites with a value":     "version: 1\nsuites: build\n",
		"a line with no key":      "version: 1\nsuites:\n  build:\n    worker\n",
		"a top line with no key":  "version 1\n",
		"a suite name with space": "version: 1\nsuites:\n  my build:\n",
	} {
		if got, err := parseDeclarations(text); err == nil {
			t.Errorf("%s: accepted %+v", name, got)
		} else if !strings.Contains(err.Error(), ArtifactsFile) {
			t.Errorf("%s: %v does not name %s", name, err, ArtifactsFile)
		}
	}
}

func TestReadDeclarationsKeepsTheCheckSuitesInOrder(t *testing.T) {
	dir := t.TempDir()
	if d, err := readDeclarations(dir, []string{"build"}); d != nil || err != nil {
		t.Fatalf("no file: %v, %v; want nothing and no error", d, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "ci"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ci", "artifacts.yml"), []byte(declaredFile), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readDeclarations(dir, []string{"lint", "image", "build"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Suite != "image" || got[1].Suite != "build" {
		t.Fatalf("readDeclarations = %+v; want image then build, and nothing for lint", got)
	}
	if got, err := readDeclarations(dir, []string{"lint"}); got != nil || err != nil {
		t.Fatalf("a check whose suites declare nothing: %v, %v", got, err)
	}

	// A link is not the repository's file.
	link := t.TempDir()
	os.MkdirAll(filepath.Join(link, "ci"), 0o755)
	if err := os.Symlink(filepath.Join(dir, "ci", "artifacts.yml"), filepath.Join(link, "ci", "artifacts.yml")); err != nil {
		t.Skip("no symlinks here")
	}
	if _, err := readDeclarations(link, []string{"build"}); err == nil {
		t.Error("a linked ci/artifacts.yml was read")
	}
}
