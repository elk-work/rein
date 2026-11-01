package check

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/secretenv"
)

// A build check's artifacts, from the job's output to the suite record
// (ark:rein#100 (01M6AMERCA97G5D02P6HEGCDFZ)). After a job passes, each
// suite that declared an artifact (declare.go) has its bytes staged here,
// uploaded to Ark and recorded on the check's Ark run (ark.go), and its record
// carries `artifacts: [{kind, ref, digest, run}]`, which Elk Cloud's
// deploy_app reads (pulse docs/suite-records.md, Artifacts).
//
// Nothing here can change a check's result. An artifact that cannot be staged,
// uploaded or recorded is left out of the record and the deliverable says
// why; a deploy of that commit is then refused for want of one, which is the
// safe way round.

// Artifact is one entry of a record's `artifacts`.
type Artifact struct {
	// Kind is worker or container.
	Kind string `json:"kind"`
	// Ref is the Ark artifact record holding the bytes: its record id.
	Ref string `json:"ref"`
	// Digest is the bytes' SHA-256, 64 lowercase hex characters, as the Ark
	// artifact record's sha256 holds it.
	Digest string `json:"digest"`
	// Run is the check's Ark run, which the artifact is recorded on.
	Run string `json:"run,omitempty"`
}

// maxArtifactBytes bounds one artifact. A Worker bundle is a few megabytes;
// this leaves room for an image archive.
const maxArtifactBytes = 2 << 30

// artifactBudget bounds staging, uploading and recording all of a check's
// artifacts, well inside the run loop's fifteen silent minutes.
const artifactBudget = 12 * time.Minute

// blob is one declared artifact's bytes, staged in the run's record directory.
type blob struct {
	decl      Declaration
	file      string
	name      string
	mediaType string
	sha256    string
	size      int64
}

// artifactOutcome is what recording a check's artifacts came to: the entries
// for each suite's record, and the lines the deliverable says about them.
type artifactOutcome struct {
	bySuite map[string][]Artifact
	lines   []string
}

// stageFile copies a declared file out of the artifact directory into the
// record directory, hashing it as it goes. The file was written by the job,
// so it is checked before it is read: a regular file, not a link, inside the
// directory — a link to a credential on this machine would otherwise be
// uploaded as a build — and the bytes come from the very file that was
// checked.
func stageFile(artDir, rel, dest string) (string, int64, error) {
	root, err := filepath.EvalSymlinks(artDir)
	if err != nil {
		return "", 0, fmt.Errorf("$%s is gone: %w", ArtifactDirEnv, err)
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, fmt.Errorf("the job did not write it to $%s", ArtifactDirEnv)
	}
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, errors.New("it is not a regular file")
	}
	if resolved, err := filepath.EvalSymlinks(full); err != nil || resolved != full {
		return "", 0, fmt.Errorf("it is not inside $%s", ArtifactDirEnv)
	}
	if info.Size() > maxArtifactBytes {
		return "", 0, fmt.Errorf("it is %d bytes, over the %d-byte limit", info.Size(), int64(maxArtifactBytes))
	}
	in, err := os.Open(full)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", 0, errors.New("it changed while Rein was reading it")
	}
	return copyHashed(in, dest)
}

// copyHashed writes r to dest, 0600, and returns its SHA-256 and size.
func copyHashed(r io.Reader, dest string) (string, int64, error) {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(r, maxArtifactBytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dest)
		return "", 0, err
	}
	if n > maxArtifactBytes {
		os.Remove(dest)
		return "", 0, fmt.Errorf("it is over the %d-byte limit", int64(maxArtifactBytes))
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// saveImage writes a container image from the Docker daemon the job used to
// dest with `docker image save`, and hashes the archive.
func (s *session) saveImage(ctx context.Context, image, dest string) (string, int64, error) {
	docker, err := exec.LookPath(s.a.bin(s.a.Docker, "docker"))
	if err != nil {
		return "", 0, errors.New("docker is not on PATH")
	}
	tmp := dest + ".partial"
	cmd := exec.CommandContext(ctx, docker, "image", "save", "-o", tmp, image)
	cmd.Env = s.env
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return "", 0, fmt.Errorf("`docker image save` failed: %s", firstLine(s.redact.String(string(out))))
	}
	defer os.Remove(tmp)
	f, err := os.Open(tmp)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return copyHashed(f, dest)
}

var nameUnsafeRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// stage gets one declaration's bytes into the record directory.
func (s *session) stage(ctx context.Context, d Declaration, i int) (blob, error) {
	b := blob{decl: d, mediaType: "application/javascript"}
	if d.Kind == KindContainer {
		b.mediaType = "application/x-tar"
	}
	var err error
	if d.Image != "" {
		b.name = strings.Trim(nameUnsafeRE.ReplaceAllString(d.Image, "-"), "-.") + ".tar"
		b.file = filepath.Join(s.recDir, fmt.Sprintf("artifact-%d.tar", i+1))
		b.sha256, b.size, err = s.saveImage(ctx, d.Image, b.file)
	} else {
		b.name = filepath.Base(d.Path)
		b.file = filepath.Join(s.recDir, fmt.Sprintf("artifact-%d", i+1))
		if s.artDir == "" {
			return b, fmt.Errorf("$%s was not given to the job", ArtifactDirEnv)
		}
		b.sha256, b.size, err = stageFile(s.artDir, d.Path, b.file)
	}
	return b, err
}

// recordArtifacts stages, uploads and records every declared artifact of a
// passing check. It runs once the job has finished and before the records are
// written, so each record carries what was recorded for its suite.
//
// The order leaves nothing dangling: every blob is stored and confirmed
// before the check's Ark run is opened, so a credential with no write grant
// is found out at the first upload and no run is opened at all.
func (s *session) recordArtifacts(ended time.Time) artifactOutcome {
	out := artifactOutcome{bySuite: map[string][]Artifact{}}
	ctx, cancel := context.WithTimeout(context.Background(), artifactBudget)
	defer cancel()
	say := func(format string, args ...any) {
		s.emit(adapter.Event{Kind: adapter.EventProgress, Text: fmt.Sprintf(format, args...)})
	}
	none := func(why string) artifactOutcome {
		out.lines = append(out.lines, "Artifacts: none recorded — "+why+". Elk Cloud will not deploy this commit from this check.")
		return out
	}

	// Ark first: with nowhere to record them, staging bytes is wasted work.
	t, err := s.a.resolveArk(arkRoot(ctx, s.git, s.spec.WorktreeDir), s.spec.Env)
	if err != nil {
		return none(s.redact.String(err.Error()))
	}
	// The credential is printed nowhere; this is the last line of defence
	// for anything the service echoes back.
	redact := secretenv.NewRedactor(map[string]string{"ARK_TOKEN": t.token})
	clean := func(err error) string { return redact.String(s.redact.String(err.Error())) }
	c := s.a.arkClient(t)

	// One blob per distinct declaration: two suites declaring the same
	// thing upload it once and share its ref.
	say("Uploading the build's artifacts to Ark repository %s.", t.repo)
	stored := map[string]blob{}
	why := map[string]string{} // declaration key → why it was not recorded
	var order []string
	for i, d := range s.decls {
		k := d.key()
		if _, seen := stored[k]; seen {
			continue
		}
		if _, seen := why[k]; seen {
			continue
		}
		b, err := s.stage(ctx, d, i)
		if b.file != "" {
			// In Ark now, or not going there: the bytes do not stay here.
			defer os.Remove(b.file)
		}
		if err == nil {
			say("Uploading %s %s (%d bytes, sha256 %s).", d.Kind, d.what(), b.size, b.sha256)
			err = c.storeBlob(ctx, b)
			if status, no := refused(err); no {
				return none(refusal(t, status, clean(err)))
			}
		}
		if err != nil {
			why[k] = fmt.Sprintf("%s %s was not uploaded: %s", d.Kind, d.what(), clean(err))
			continue
		}
		stored[k] = b
		order = append(order, k)
	}

	refs := map[string]string{} // declaration key → artifact ref
	run := ""
	if len(order) > 0 {
		key := "rein-check:" + s.spec.RunID
		meta := map[string]any{
			"kind": "check", "elk_run": s.spec.RunID, "repo": s.check.Repo, "suites": s.check.Suites,
			"workflow": s.check.Workflow, "job": s.jobID, "queue": s.spec.Queue, "result": ResultPass,
			"started": s.started.UTC().Format(time.RFC3339Nano), "ended": ended.UTC().Format(time.RFC3339Nano),
		}
		summary := fmt.Sprintf("Check %s at %s: the %s job of %s passed, and its build is recorded here.",
			joinNames(s.check.Suites), s.check.SHA, s.jobID, s.check.Workflow)
		run, err = c.openRun(ctx, key+":run", summary, s.check.SHA, meta)
		if err != nil {
			if status, no := refused(err); no {
				return none(refusal(t, status, clean(err)))
			}
			return none("the bytes were uploaded, but the check's Ark run could not be opened: " + clean(err))
		}
		for _, k := range order {
			b := stored[k]
			ref, err := c.recordArtifact(ctx, run, fmt.Sprintf("%s:artifact:%s:%s", key, b.decl.Kind, b.sha256), b)
			if err != nil {
				why[k] = fmt.Sprintf("%s %s was uploaded but not recorded on Ark run %s: %s", b.decl.Kind, b.decl.what(), run, clean(err))
				continue
			}
			refs[k] = ref
		}
		status, result := "succeeded", fmt.Sprintf("Recorded %d artifact(s) from the check.", len(refs))
		if len(refs) < len(order) {
			status, result = "failed", fmt.Sprintf("Recorded %d of %d artifacts from the check.", len(refs), len(order))
		}
		if err := c.finishRun(ctx, run, key+":finish", status, result); err != nil {
			// The artifacts are recorded and verified; a run left open says
			// nothing false about them, so they stand.
			out.lines = append(out.lines, fmt.Sprintf("Ark run `%s` could not be finished: %s", run, clean(err)))
		}
	}

	for _, d := range s.decls {
		k := d.key()
		ref, ok := refs[k]
		if !ok {
			out.lines = append(out.lines, fmt.Sprintf("Artifact for %s: none recorded — %s. Elk Cloud will not deploy "+
				"this commit's %s from this check.", d.Suite, why[k], d.Kind))
			continue
		}
		b := stored[k]
		out.bySuite[d.Suite] = append(out.bySuite[d.Suite], Artifact{Kind: d.Kind, Ref: ref, Digest: b.sha256, Run: run})
		out.lines = append(out.lines, fmt.Sprintf("Artifact for %s: %s %s, %d bytes, sha256 `%s`, recorded as Ark artifact "+
			"`%s` on Ark run `%s` in repository %s, uploaded with %s", d.Suite, d.Kind, d.what(), b.size, b.sha256, ref, run, t.repo, t.source))
	}
	return out
}

// refusal is the sentence for a credential Ark turned away.
func refusal(t arkTarget, status int, detail string) string {
	if status == 401 {
		return fmt.Sprintf("Ark at %s did not accept the credential from %s (%s); log in again with `ark login` on this machine", t.remote, t.source, detail)
	}
	return fmt.Sprintf("the credential from %s has no write grant on Ark repository %s (%s); a person with admin on it grants one with `ark repo grant`", t.source, t.repo, detail)
}
