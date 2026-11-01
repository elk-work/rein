package check

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// What a build suite says it produced (ark:rein#100
// (01M6AMERCA97G5D02P6HEGCDFZ)): the repository's own ci/artifacts.yml, read
// from the worktree at the commit tested. Never the packet: Elk relays what a
// commit's suites are, and a deploy must ship what the commit itself said it
// builds.
//
// The file is not ci/suites.yml, though it names the same suites, because Elk
// reads that one strictly and refuses a manifest with a field it does not
// know (scout ci_plan.mjs, KNOWN), so adding a field there would stop every
// check the repository has. Its grammar is suites.yml's own subset: a version,
// a `suites:` block, two-space suite names, four-space `key: value` scalars.
//
//	version: 1
//	suites:
//	  build:
//	    kind: worker
//	    path: worker.js
//	  image:
//	    kind: container
//	    image: elk-app:ci

// ArtifactsFile is where a repository declares its build suites' artifacts.
const ArtifactsFile = "ci/artifacts.yml"

// ArtifactDirEnv names the directory a job copies a declared file into. act
// copies the worktree before it runs a job and deletes the copy afterwards,
// on both executors, so a file a step writes into its workspace is gone by
// the time the job ends. This directory is outside the worktree and outlives
// the job. A workflow that also runs on GitHub guards the copy with
// `if: ${{ env.REIN_ARTIFACT_DIR }}`, which is false where it is unset.
const ArtifactDirEnv = "REIN_ARTIFACT_DIR"

// The kinds a suite may declare: what Elk Cloud deploys.
const (
	KindWorker    = "worker"
	KindContainer = "container"
)

// maxArtifactsFile bounds what Rein reads of the file.
const maxArtifactsFile = 64 << 10

// Declaration is one suite's declared artifact. A suite declares one: a
// deploy refuses a commit whose checks recorded several of one kind.
type Declaration struct {
	// Suite is the suite the artifact is recorded for.
	Suite string
	// Kind is worker or container.
	Kind string
	// Path is the file the job copied into $REIN_ARTIFACT_DIR, relative to
	// it. A worker declares a path; a container may declare one instead of
	// an image, for an image archive the job saved itself.
	Path string
	// Image is a container image on the Docker daemon the job built it on,
	// which Rein saves with `docker image save` after the job passes.
	Image string
}

// key identifies the bytes a declaration names, so two suites declaring the
// same thing upload it once.
func (d Declaration) key() string { return d.Kind + "\x00" + d.Path + "\x00" + d.Image }

// what names the declared thing for a person.
func (d Declaration) what() string {
	if d.Image != "" {
		return "image `" + d.Image + "`"
	}
	return "`" + d.Path + "`"
}

// validate refuses a declaration Rein could not honour.
func (d Declaration) validate() error {
	switch {
	case d.Kind != KindWorker && d.Kind != KindContainer:
		return fmt.Errorf("suite %s: kind %q is not worker or container", d.Suite, d.Kind)
	case d.Kind == KindWorker && d.Image != "":
		return fmt.Errorf("suite %s: a worker is a file; give a path, not an image", d.Suite)
	case d.Path == "" && d.Image == "":
		return fmt.Errorf("suite %s: give a path (or, for a container, an image)", d.Suite)
	case d.Path != "" && d.Image != "":
		return fmt.Errorf("suite %s: give a path or an image, not both", d.Suite)
	case d.Image != "" && !imageRE.MatchString(d.Image):
		return fmt.Errorf("suite %s: image %q is not an image reference", d.Suite, d.Image)
	case d.Path != "" && (len(d.Path) > 255 || strings.ContainsAny(d.Path, "\\\x00") ||
		!filepath.IsLocal(filepath.FromSlash(d.Path)) || path.Clean(d.Path) != d.Path):
		return fmt.Errorf("suite %s: path %q is not a plain relative path inside $%s", d.Suite, d.Path, ArtifactDirEnv)
	}
	return nil
}

// readDeclarations reads the worktree's ci/artifacts.yml and returns the
// declarations for the suites this check records, in the order of suites. No
// file is no declarations and no error; a file that will not parse is an
// error, which costs the check its artifacts and never its result.
func readDeclarations(worktree string, suites []string) ([]Declaration, error) {
	path := filepath.Join(worktree, filepath.FromSlash(ArtifactsFile))
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ArtifactsFile, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", ArtifactsFile)
	}
	if info.Size() > maxArtifactsFile {
		return nil, fmt.Errorf("%s is over %d KiB", ArtifactsFile, maxArtifactsFile>>10)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ArtifactsFile, err)
	}
	all, err := parseDeclarations(string(b))
	if err != nil {
		return nil, err
	}
	var out []Declaration
	for _, s := range suites {
		if d, ok := all[s]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}

// parseDeclarations reads the file's text: suites.yml's grammar, strictly. A
// key it does not know is an error rather than a guess, so a typo in `path`
// is reported instead of quietly recording nothing.
func parseDeclarations(text string) (map[string]Declaration, error) {
	out := map[string]Declaration{}
	var (
		version  string
		inSuites bool
		suite    string
		seen     map[string]bool
	)
	finish := func() error {
		if suite == "" {
			return nil
		}
		if err := out[suite].validate(); err != nil {
			return fmt.Errorf("%s: %w", ArtifactsFile, err)
		}
		return nil
	}
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		where := fmt.Sprintf("%s:%d", ArtifactsFile, i+1)
		lead := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if strings.Contains(lead, "\t") {
			return nil, fmt.Errorf("%s: tabs are not allowed for indentation", where)
		}
		body := stripComment(line)
		if strings.TrimSpace(body) == "" {
			continue
		}
		indent := len(body) - len(strings.TrimLeft(body, " "))
		t := strings.TrimSpace(body)
		key, val, hasColon := strings.Cut(t, ":")
		val = strings.TrimSpace(val)
		switch {
		case indent == 0:
			if err := finish(); err != nil {
				return nil, err
			}
			suite = ""
			switch {
			case !hasColon:
				return nil, fmt.Errorf("%s: expected \"key: value\" at the top level", where)
			case key == "version":
				version = unquoteScalar(val)
				inSuites = false
			case key == "suites" && val == "":
				inSuites = true
			case key == "suites":
				return nil, fmt.Errorf("%s: \"suites:\" takes a block of suites", where)
			default:
				return nil, fmt.Errorf("%s: unknown top-level key %q (want version and suites)", where, key)
			}
		case indent == 2 && inSuites:
			if err := finish(); err != nil {
				return nil, err
			}
			if !hasColon || val != "" || key == "" || strings.ContainsAny(key, " \t") {
				return nil, fmt.Errorf("%s: expected \"<suite>:\"", where)
			}
			if _, dup := out[key]; dup {
				return nil, fmt.Errorf("%s: suite %q is declared twice", where, key)
			}
			suite, seen = key, map[string]bool{}
			out[suite] = Declaration{Suite: suite}
		case indent == 4 && suite != "":
			if !hasColon || val == "" {
				return nil, fmt.Errorf("%s: expected \"key: value\"", where)
			}
			if seen[key] {
				return nil, fmt.Errorf("%s: %s.%s is set twice", where, suite, key)
			}
			seen[key] = true
			d := out[suite]
			switch v := unquoteScalar(val); key {
			case "kind":
				d.Kind = v
			case "path":
				d.Path = v
			case "image":
				d.Image = v
			default:
				return nil, fmt.Errorf("%s: %s.%s is not a field (want kind, and path or image)", where, suite, key)
			}
			out[suite] = d
		default:
			return nil, fmt.Errorf("%s: unexpected indentation (%d spaces)", where, indent)
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}
	if version != "1" {
		return nil, fmt.Errorf("%s: version must be 1", ArtifactsFile)
	}
	return out, nil
}

// stripComment drops a `#` comment that is not inside quotes.
func stripComment(line string) string {
	var q rune
	for i, c := range line {
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i]
		}
	}
	return line
}

// unquoteScalar is a scalar's value: quotes, when it has them, removed.
func unquoteScalar(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
