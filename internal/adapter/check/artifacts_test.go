//go:build !windows

package check

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/keyring"
)

// A build check's artifacts, end to end against a fake Ark service
// (ark:rein#100 (01M6AMERCA97G5D02P6HEGCDFZ)): the job copies its bundle into
// $REIN_ARTIFACT_DIR, Rein uploads it with upload-url, PUT and confirm,
// records it on the check's Ark run, and the suite record carries
// artifacts: [{kind, ref, digest, run}].

const (
	testArkRepo  = "01M17TTM53XJKBZW2M9E04HHYP"
	testArkHuman = "01M5RKDTKW1EHGK35RF363Z25N"
	testArkToken = "arkc_test-credential-0123456789"
	workerBytes  = "export default { fetch() { return new Response('ok') } }\n"
	fetchSecret  = "fake-claim-fetch-credential-for-artifacts"
)

// arkRequest is one request the fake service saw.
type arkRequest struct {
	method, path, auth, key, contentType, body string
}

// fakeArk serves the six routes a build check uses, with ark-server's rules:
// a bearer on every API call, an Idempotency-Key on the two creates, the
// signed PUT outside auth, confirm checking the bytes hash to their key, and
// 409 for an artifact whose blob is not confirmed.
type fakeArk struct {
	t      *testing.T
	srv    *httptest.Server
	forbid bool // answer 403, as for a credential with no write grant

	mu        sync.Mutex
	reqs      []arkRequest
	blobs     map[string][]byte
	confirmed map[string]bool
	runs      map[string]string // idempotency key → run id
	artifacts map[string]string // idempotency key → artifact id
	finished  map[string]string // run id → status
	next      int
}

func newFakeArk(t *testing.T) *fakeArk {
	f := &fakeArk{t: t, blobs: map[string][]byte{}, confirmed: map[string]bool{}, runs: map[string]string{},
		artifacts: map[string]string{}, finished: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeArk) id() string {
	f.next++
	return fmt.Sprintf("01M6AMERCA97G5D02P6HEG%04d", f.next)
}

func (f *fakeArk) requests() []arkRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]arkRequest(nil), f.reqs...)
}

func (f *fakeArk) fail(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

func (f *fakeArk) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, arkRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
		key: r.Header.Get("Idempotency-Key"), contentType: r.Header.Get("Content-Type"), body: string(body)})
	if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/blobs/") {
		f.blobs[strings.TrimPrefix(r.URL.Path, "/blobs/")] = body
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testArkToken {
		f.fail(w, http.StatusUnauthorized, "permission", "unknown credential")
		return
	}
	if f.forbid {
		f.fail(w, http.StatusForbidden, "permission", "no write grant on "+testArkRepo)
		return
	}
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	record := func(id string) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"record": map[string]any{"record_id": id, "id": id}, "server_revision": f.next})
	}
	runs := "/v1/repositories/" + testArkRepo + "/runs"
	switch {
	case r.URL.Path == "/v1/artifacts/upload-url":
		sha := str("sha256")
		if f.confirmed[sha] {
			json.NewEncoder(w).Encode(map[string]any{"storage_key": "sha256/" + sha, "already_stored": true})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"url": f.srv.URL + "/blobs/" + sha + "?exp=1&sig=signed", "storage_key": "sha256/" + sha})
	case r.URL.Path == "/v1/artifacts/confirm":
		sha := str("sha256")
		sum := sha256.Sum256(f.blobs[sha])
		if _, ok := f.blobs[sha]; !ok || hex.EncodeToString(sum[:]) != sha {
			f.fail(w, http.StatusConflict, "conflict", "blob not found in object storage")
			return
		}
		f.confirmed[sha] = true
		json.NewEncoder(w).Encode(map[string]any{"storage_key": "sha256/" + sha, "already_stored": true})
	case r.URL.Path == runs:
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			f.fail(w, http.StatusBadRequest, "validation", "Idempotency-Key is required")
			return
		}
		if _, ok := f.runs[key]; !ok {
			f.runs[key] = f.id()
		}
		record(f.runs[key])
	case strings.HasPrefix(r.URL.Path, runs+"/") && strings.HasSuffix(r.URL.Path, "/artifacts"):
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			f.fail(w, http.StatusBadRequest, "validation", "Idempotency-Key is required")
			return
		}
		if !f.confirmed[str("sha256")] {
			f.fail(w, http.StatusConflict, "conflict", "blob is not stored in this repository: upload it first")
			return
		}
		if _, ok := f.artifacts[key]; !ok {
			f.artifacts[key] = f.id()
		}
		record(f.artifacts[key])
	case strings.HasPrefix(r.URL.Path, runs+"/") && strings.HasSuffix(r.URL.Path, "/finish"):
		run := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, runs+"/"), "/finish")
		f.finished[run] = str("status")
		json.NewEncoder(w).Encode(map[string]any{"record": map[string]any{"record_id": run, "id": run}})
	default:
		f.fail(w, http.StatusNotFound, "not_found", "no route "+r.URL.Path)
	}
}

// noItems is a keyring that holds nothing: no test reads the real one.
type noItems struct{}

func (noItems) Name() string { return "test" }
func (noItems) ReadItem(service, account string) (string, error) {
	return "", fmt.Errorf("%w: %s/%s", keyring.ErrItemNotFound, service, account)
}

// oneItem holds one Ark login, under ark's own service and the host.
type oneItem struct{ account, value string }

func (oneItem) Name() string { return "test" }
func (o oneItem) ReadItem(service, account string) (string, error) {
	if service == "ark" && account == o.account {
		return o.value, nil
	}
	return "", keyring.ErrItemNotFound
}

// buildRepo is testRepo with a ci/artifacts.yml declaring the floor suite's
// worker bundle, and a checkout joined to the fake Ark service.
func buildRepo(t *testing.T, ark *fakeArk, declared string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755)
	os.MkdirAll(filepath.Join(dir, "ci"), 0o755)
	os.WriteFile(filepath.Join(dir, ".github", "workflows", "floor.yml"), []byte(floorWorkflow), 0o644)
	if declared != "" {
		os.WriteFile(filepath.Join(dir, "ci", "artifacts.yml"), []byte(declared), 0o644)
	}
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".ark/\n"), 0o644)
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "init")
	if ark != nil {
		joinArk(t, dir, ark.srv.URL, "human")
	}
	return dir, strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
}

func joinArk(t *testing.T, dir, remote, actorType string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, ".ark"), 0o755)
	cfg := fmt.Sprintf("version = 1\nrepository_id = %q\nremote = %q\ndefault_actor_id = %q\ndefault_actor_type = %q\n",
		testArkRepo, remote, testArkHuman, actorType)
	if err := os.WriteFile(filepath.Join(dir, ".ark", "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

const floorWorker = "version: 1\nsuites:\n  floor:\n    kind: worker\n    path: worker.js\n"

// buildAct is an act that writes the worker bundle into the directory
// `--env REIN_ARTIFACT_DIR=…` names, records its environment, and replays
// fixture.
func buildAct(t *testing.T, fixture string) (act, argsFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile, envFile = filepath.Join(dir, "args"), filepath.Join(dir, "env")
	list, _ := filepath.Abs(filepath.Join("testdata", "act-list.txt"))
	log, _ := filepath.Abs(filepath.Join("testdata", fixture))
	act = stubScript(t, "act", "case \" $* \" in *\" --list \"*) cat '"+list+"'; exit 0;; esac\n"+
		"for a in \"$@\"; do echo \"$a\"; done > '"+argsFile+"'\n"+
		"env > '"+envFile+"'\n"+
		"prev=''\nfor a in \"$@\"; do\n"+
		"  if [ \"$prev\" = --env ]; then case \"$a\" in REIN_ARTIFACT_DIR=*) out=\"${a#REIN_ARTIFACT_DIR=}\";; esac; fi\n"+
		"  prev=\"$a\"\ndone\n"+
		"if [ -n \"$out\" ]; then printf '%s' \""+strings.TrimSuffix(workerBytes, "\n")+"\n\" > \"$out/worker.js\"; fi\n"+
		"cat '"+log+"'\n")
	return act, argsFile, envFile
}

// buildAdapter never reads this machine's keyring or credentials file.
func buildAdapter(t *testing.T, act string, ark *fakeArk) *Adapter {
	t.Helper()
	t.Setenv("ARK_TOKEN", "")
	t.Setenv("ARK_DELEGATED_BY", "")
	t.Setenv("ARK_NO_KEYRING", "")
	a := &Adapter{Act: act, RecordRoot: t.TempDir(), Pulse: "no-such-pulse-binary", ArkKeyring: noItems{}, ArkHome: t.TempDir()}
	if ark != nil {
		a.ArkHTTP = ark.srv.Client()
	}
	return a
}

// packetWithFetch is the floor packet with a claim's fetch credential, which
// must reach nothing the check uploads or records.
func packetWithFetch(sha string) string {
	check := `{"repo":"elk-work/tiny","sha":"` + sha + `","ref":"refs/heads/main","suite":"floor",` +
		`"workflow":".github/workflows/floor.yml","job":"floor","runs_on":["ubuntu-latest"],"provider":"rein"}`
	f := map[string]any{"kind": "github-token", "repo": "elk-work/tiny", "url": "https://github.com/elk-work/tiny.git",
		"username": "x-access-token", "token": fetchSecret, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	raw, _ := json.Marshal(map[string]any{"check": json.RawMessage(check), "fetch": f})
	return order("```json\n" + string(raw) + "\n```")
}

func TestAPassingBuildRecordsItsArtifactInArk(t *testing.T) {
	ark := newFakeArk(t)
	dir, sha := buildRepo(t, ark, floorWorker)
	act, argsFile, envFile := buildAct(t, "act-pass.jsonl")
	a := buildAdapter(t, act, ark)
	spec := baseSpec(dir, sha)
	spec.Prompt = packetWithFetch(sha)
	// A queue that maps ARK_TOKEN hands it to Rein, never to the job.
	spec.Env = map[string]string{"ARK_TOKEN": testArkToken}
	r := runCheck(t, a, spec, argsFile)

	if r.rec.Result != ResultPass || r.res.Status != adapter.StatusSucceeded {
		t.Fatalf("result %s, status %s: %v", r.rec.Result, r.res.Status, r.err)
	}
	sum := sha256.Sum256([]byte(workerBytes))
	digest := hex.EncodeToString(sum[:])

	// The sequence, in order: nothing is recorded before its bytes are
	// confirmed, and the run is opened only once there is something to put
	// on it.
	var seq []string
	for _, q := range ark.requests() {
		p := q.path
		p = strings.Replace(p, "/v1/repositories/"+testArkRepo, "/repo", 1)
		for _, id := range []string{"01M6AMERCA97G5D02P6HEG0001"} {
			p = strings.ReplaceAll(p, id, "{run}")
		}
		if strings.HasPrefix(p, "/blobs/") {
			p = "/blobs/{sha}"
		}
		seq = append(seq, q.method+" "+p)
	}
	want := []string{"POST /v1/artifacts/upload-url", "PUT /blobs/{sha}", "POST /v1/artifacts/confirm",
		"POST /repo/runs", "POST /repo/runs/{run}/artifacts", "POST /repo/runs/{run}/finish"}
	if strings.Join(seq, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Ark saw\n%s\nwant\n%s", strings.Join(seq, "\n"), strings.Join(want, "\n"))
	}
	reqs := ark.requests()
	for _, q := range reqs {
		switch {
		case q.method == http.MethodPut:
			if q.auth != "" {
				t.Errorf("the signed PUT carried an Authorization header")
			}
			if q.body != workerBytes || q.contentType != "application/javascript" {
				t.Errorf("PUT body %q, content type %q", q.body, q.contentType)
			}
		case q.auth != "Bearer "+testArkToken:
			t.Errorf("%s %s carried no Ark bearer", q.method, q.path)
		}
		if strings.HasSuffix(q.path, "/runs") || strings.HasSuffix(q.path, "/artifacts") {
			if !strings.HasPrefix(q.key, "rein-check:run-1:") {
				t.Errorf("%s has Idempotency-Key %q", q.path, q.key)
			}
		}
		// The claim's fetch credential reaches nothing Ark is sent.
		for _, s := range []string{fetchSecret, "x-access-token"} {
			if strings.Contains(q.body+q.auth+q.key+q.path, s) {
				t.Errorf("%s %s carried the fetch credential", q.method, q.path)
			}
		}
	}
	var run, art map[string]any
	json.Unmarshal([]byte(reqs[3].body), &run)
	json.Unmarshal([]byte(reqs[4].body), &art)
	writer, _ := run["writer"].(map[string]any)
	if writer["agent_name"] != "rein" || writer["delegated_by"] != testArkHuman || run["base_commit_sha"] != sha {
		t.Errorf("the run was opened as %v", run)
	}
	if meta, _ := run["metadata"].(map[string]any); meta["kind"] != "check" || meta["elk_run"] != "run-1" {
		t.Errorf("the run's metadata is %v", run["metadata"])
	}
	if art["sha256"] != digest || art["name"] != "worker.js" || art["size_bytes"] != float64(len(workerBytes)) {
		t.Errorf("the artifact was recorded as %v", art)
	}
	if got := ark.finished["01M6AMERCA97G5D02P6HEG0001"]; got != "succeeded" {
		t.Errorf("the Ark run finished %q, want succeeded", got)
	}

	// The record, file and deliverable alike.
	wantArt := []Artifact{{Kind: KindWorker, Ref: "01M6AMERCA97G5D02P6HEG0002", Digest: digest, Run: "01M6AMERCA97G5D02P6HEG0001"}}
	if fmt.Sprint(r.rec.Artifacts) != fmt.Sprint(wantArt) {
		t.Fatalf("record artifacts = %+v, want %+v", r.rec.Artifacts, wantArt)
	}
	file := recordFile(t, a, "run-1")
	if !strings.Contains(file, "\"artifacts\": [\n    {\n      \"kind\": \"worker\",\n      \"ref\": \"01M6AMERCA97G5D02P6HEG0002\",") {
		t.Errorf("the record file's artifacts:\n%s", file)
	}
	if !strings.Contains(r.res.Summary, "\n```json\n"+file+"```\n") {
		t.Errorf("the deliverable's block is not the record file's bytes:\n%s", r.res.Summary)
	}
	if !strings.Contains(r.res.Summary, "recorded as Ark artifact `01M6AMERCA97G5D02P6HEG0002`") ||
		!strings.Contains(r.res.Summary, "the queue's ARK_TOKEN") {
		t.Errorf("the deliverable does not say what was recorded:\n%s", r.res.Summary)
	}

	// The credentials stay out of the job and out of everything printed.
	envb, _ := os.ReadFile(envFile)
	if strings.Contains(string(envb), "ARK_TOKEN") || strings.Contains(string(envb), testArkToken) {
		t.Error("act's environment held the Ark credential")
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--env\nREIN_ARTIFACT_DIR="+filepath.Join(a.RecordRoot, "run-1", "artifacts")+"\n") {
		t.Errorf("act was not handed the artifact directory:\n%s", args)
	}
	everything := r.res.Summary + file
	for _, ev := range r.events {
		everything += ev.Text
	}
	for _, s := range []string{testArkToken, fetchSecret} {
		if strings.Contains(everything, s) {
			t.Errorf("%s reached the deliverable, the record or an event", s)
		}
	}

	// The bytes do not stay on the machine once they are in Ark.
	left, _ := filepath.Glob(filepath.Join(a.RecordRoot, "run-1", "artifact*"))
	if len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
}

// The ark CLI's order: the queue's ARK_TOKEN, the daemon's, the keyring under
// "ark" and the remote's host, then ~/.ark/credentials.toml.
func TestResolveArkFindsTheCredentialTheWayArkDoes(t *testing.T) {
	ark := newFakeArk(t)
	dir, _ := buildRepo(t, ark, "")
	host := strings.TrimPrefix(ark.srv.URL, "http://")
	a := buildAdapter(t, "", ark)

	if _, err := a.resolveArk(dir, nil); err == nil || !strings.Contains(err.Error(), "no Ark credential") ||
		!strings.Contains(err.Error(), "ark login") {
		t.Errorf("no credential anywhere: %v", err)
	}

	os.MkdirAll(filepath.Join(a.ArkHome, ".ark"), 0o700)
	os.WriteFile(filepath.Join(a.ArkHome, ".ark", "credentials.toml"),
		[]byte(fmt.Sprintf("[remotes.%q]\ntoken = \"from-file\"\n", host)), 0o600)
	if tg, err := a.resolveArk(dir, nil); err != nil || tg.token != "from-file" || !strings.Contains(tg.source, "credentials.toml") {
		t.Errorf("the credentials file: %+v %v", tg.source, err)
	}

	a.ArkKeyring = oneItem{account: host, value: "from-keyring"}
	if tg, err := a.resolveArk(dir, nil); err != nil || tg.token != "from-keyring" {
		t.Errorf("the keyring outranks the file: %v %v", tg.token, err)
	}
	t.Setenv("ARK_NO_KEYRING", "1")
	if tg, _ := a.resolveArk(dir, nil); tg.token != "from-file" {
		t.Errorf("ARK_NO_KEYRING still read the keyring: %q", tg.token)
	}
	t.Setenv("ARK_NO_KEYRING", "")

	t.Setenv("ARK_TOKEN", "from-daemon")
	if tg, _ := a.resolveArk(dir, nil); tg.token != "from-daemon" || tg.source != "ARK_TOKEN" {
		t.Errorf("ARK_TOKEN outranks the keyring: %q", tg.token)
	}
	if tg, _ := a.resolveArk(dir, map[string]string{"ARK_TOKEN": "from-queue"}); tg.token != "from-queue" {
		t.Errorf("the queue's ARK_TOKEN outranks the daemon's: %q", tg.token)
	}

	// The human the writer acts for: ARK_DELEGATED_BY, else the default
	// actor when that is a human.
	tg, _ := a.resolveArk(dir, nil)
	if tg.delegatedBy != testArkHuman || tg.repo != testArkRepo || tg.remote != ark.srv.URL {
		t.Errorf("target %+v", tg)
	}
	other := "01M5RKDTKW1EHGK35RF363Z25P"
	if tg, _ := a.resolveArk(dir, map[string]string{"ARK_DELEGATED_BY": other}); tg.delegatedBy != other {
		t.Errorf("ARK_DELEGATED_BY: %q", tg.delegatedBy)
	}
	joinArk(t, dir, ark.srv.URL, "agent")
	if _, err := a.resolveArk(dir, nil); err == nil || !strings.Contains(err.Error(), "no human") {
		t.Errorf("an agent default actor: %v", err)
	}
	joinArk(t, dir, "", "human")
	if _, err := a.resolveArk(dir, nil); err == nil || !strings.Contains(err.Error(), "no sync service") {
		t.Errorf("no remote: %v", err)
	}
	if _, err := a.resolveArk(t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "not joined to Ark") {
		t.Errorf("no .ark: %v", err)
	}
}

// Without a usable credential the check records nothing, says why, and still
// passes: a deploy is then refused artifact_required, which is the safe way
// round.
func TestABuildWithNoArkCredentialRecordsNoArtifacts(t *testing.T) {
	ark := newFakeArk(t)
	dir, sha := buildRepo(t, ark, floorWorker)
	act, argsFile, _ := buildAct(t, "act-pass.jsonl")
	a := buildAdapter(t, act, ark)
	r := runCheck(t, a, baseSpec(dir, sha), argsFile)
	if r.rec.Result != ResultPass || r.res.Status != adapter.StatusSucceeded {
		t.Fatalf("a missing credential changed the check: %s %s", r.rec.Result, r.res.Status)
	}
	if len(r.rec.Artifacts) != 0 || strings.Contains(recordFile(t, a, "run-1"), "artifacts") {
		t.Fatalf("artifacts recorded without a credential: %+v", r.rec.Artifacts)
	}
	if n := len(ark.requests()); n != 0 {
		t.Errorf("Ark was called %d times with no credential", n)
	}
	if !strings.Contains(r.res.Summary, "Artifacts: none recorded — this machine holds no Ark credential") ||
		!strings.Contains(r.res.Summary, "Elk Cloud will not deploy this commit") {
		t.Errorf("the deliverable does not say why:\n%s", r.res.Summary)
	}
	if left, _ := filepath.Glob(filepath.Join(a.RecordRoot, "run-1", "artifact*")); len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
}

// A credential with no write grant is found out at the first upload, before
// any run is opened.
func TestABuildWhoseCredentialHasNoWriteGrantOpensNoRun(t *testing.T) {
	ark := newFakeArk(t)
	ark.forbid = true
	dir, sha := buildRepo(t, ark, floorWorker)
	act, argsFile, _ := buildAct(t, "act-pass.jsonl")
	a := buildAdapter(t, act, ark)
	spec := baseSpec(dir, sha)
	spec.Env = map[string]string{"ARK_TOKEN": testArkToken}
	r := runCheck(t, a, spec, argsFile)
	if len(r.rec.Artifacts) != 0 || r.rec.Result != ResultPass {
		t.Fatalf("record %+v", r.rec)
	}
	reqs := ark.requests()
	if len(reqs) != 1 || reqs[0].path != "/v1/artifacts/upload-url" {
		t.Errorf("Ark saw %d requests after a 403; want the one upload-url", len(reqs))
	}
	if !strings.Contains(r.res.Summary, "has no write grant on Ark repository "+testArkRepo) {
		t.Errorf("the deliverable does not name the missing grant:\n%s", r.res.Summary)
	}
}

// Only a passing build records anything.
func TestAFailedBuildRecordsNoArtifacts(t *testing.T) {
	ark := newFakeArk(t)
	dir, sha := buildRepo(t, ark, floorWorker)
	act, argsFile, _ := buildAct(t, "act-fail.jsonl")
	a := buildAdapter(t, act, ark)
	spec := baseSpec(dir, sha)
	spec.Env = map[string]string{"ARK_TOKEN": testArkToken}
	r := runCheck(t, a, spec, argsFile)
	if r.rec.Result != ResultFail || len(r.rec.Artifacts) != 0 {
		t.Fatalf("record %+v", r.rec)
	}
	if n := len(ark.requests()); n != 0 {
		t.Errorf("a failed build called Ark %d times", n)
	}
	if !strings.Contains(r.res.Summary, "the check did not pass") {
		t.Errorf("the deliverable does not say why:\n%s", r.res.Summary)
	}
	if _, err := os.Stat(filepath.Join(a.RecordRoot, "run-1", "artifacts")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the artifact directory was left behind: %v", err)
	}
}

// A declared file the job never wrote is named, and nothing is recorded.
func TestADeclaredFileTheJobDidNotWriteIsNamed(t *testing.T) {
	ark := newFakeArk(t)
	dir, sha := buildRepo(t, ark, "version: 1\nsuites:\n  floor:\n    kind: worker\n    path: dist/missing.js\n")
	act, argsFile, _ := buildAct(t, "act-pass.jsonl")
	a := buildAdapter(t, act, ark)
	spec := baseSpec(dir, sha)
	spec.Env = map[string]string{"ARK_TOKEN": testArkToken}
	r := runCheck(t, a, spec, argsFile)
	if len(r.rec.Artifacts) != 0 {
		t.Fatalf("record %+v", r.rec)
	}
	for _, q := range ark.requests() {
		if strings.Contains(q.path, "/runs") {
			t.Errorf("a run was opened with nothing to record: %s", q.path)
		}
	}
	if !strings.Contains(r.res.Summary, "`dist/missing.js` was not uploaded: the job did not write it to $REIN_ARTIFACT_DIR") {
		t.Errorf("the deliverable does not name the missing file:\n%s", r.res.Summary)
	}
}

// A declaration that will not parse costs the check its artifacts, and the
// job is not even handed the directory.
func TestABadDeclarationIsReportedNotRun(t *testing.T) {
	ark := newFakeArk(t)
	dir, sha := buildRepo(t, ark, "version: 1\nsuites:\n  floor:\n    kind: worker\n    paht: worker.js\n")
	act, argsFile, _ := buildAct(t, "act-pass.jsonl")
	a := buildAdapter(t, act, ark)
	r := runCheck(t, a, baseSpec(dir, sha), argsFile)
	if r.rec.Result != ResultPass || len(r.rec.Artifacts) != 0 {
		t.Fatalf("record %+v", r.rec)
	}
	if hasArgs(r.args, "--env") {
		t.Errorf("act was handed an artifact directory for a declaration that did not parse: %v", r.args)
	}
	if !strings.Contains(r.res.Summary, "floor.paht is not a field") {
		t.Errorf("the deliverable does not name the bad field:\n%s", r.res.Summary)
	}
}

// The container executor mounts the artifact directory beside the git
// directory, in the one --container-options act takes.
func TestTheContainerExecutorMountsTheArtifactDirectory(t *testing.T) {
	dir, sha := buildRepo(t, nil, floorWorker)
	act, argsFile, _ := buildAct(t, "act-pass.jsonl")
	a := buildAdapter(t, act, nil)
	a.Docker = stubScript(t, "docker", "exit 0")
	spec := baseSpec(dir, sha)
	spec.Settings = map[string]string{"executor": "container"}
	r := runCheck(t, a, spec, argsFile)
	art := filepath.Join(a.RecordRoot, "run-1", "artifacts")
	n := 0
	for i, arg := range r.args {
		if arg != "--container-options" {
			continue
		}
		n++
		opts := r.args[i+1]
		if !strings.Contains(opts, ".git:ro") || !strings.HasSuffix(opts, " -v "+art+":"+art) {
			t.Errorf("--container-options %q", opts)
		}
	}
	if n != 1 {
		t.Errorf("--container-options given %d times; act reads one", n)
	}
	if !hasArgs(r.args, "--env", ArtifactDirEnv+"="+art) {
		t.Errorf("no %s in %v", ArtifactDirEnv, r.args)
	}
	// No Ark here: the checkout is not joined.
	if !strings.Contains(r.res.Summary, "not joined to Ark") {
		t.Errorf("the deliverable does not say the checkout has no Ark repository:\n%s", r.res.Summary)
	}
}

// A file the job wrote is checked before Rein reads it: a link out of the
// directory — to a credential on this machine, say — is never uploaded.
func TestStageFileRefusesWhatIsNotTheJobsFile(t *testing.T) {
	art := t.TempDir()
	secret := filepath.Join(t.TempDir(), "credentials.toml")
	os.WriteFile(secret, []byte("token = \"x\""), 0o600)
	os.WriteFile(filepath.Join(art, "worker.js"), []byte(workerBytes), 0o644)
	os.Symlink(secret, filepath.Join(art, "link.js"))
	os.Symlink(filepath.Dir(secret), filepath.Join(art, "sub"))
	os.Mkdir(filepath.Join(art, "dir.js"), 0o755)
	dest := t.TempDir()

	sha, size, err := stageFile(art, "worker.js", filepath.Join(dest, "a"))
	sum := sha256.Sum256([]byte(workerBytes))
	if err != nil || sha != hex.EncodeToString(sum[:]) || size != int64(len(workerBytes)) {
		t.Fatalf("stageFile = %s %d %v", sha, size, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "a")); string(b) != workerBytes {
		t.Error("the staged copy differs from the file")
	}
	for i, rel := range []string{"link.js", "sub/credentials.toml", "dir.js", "missing.js"} {
		if _, _, err := stageFile(art, rel, filepath.Join(dest, fmt.Sprint(i))); err == nil {
			t.Errorf("%s was staged", rel)
		}
	}
}

// The records of a check whose suites declare artifacts must still fit the
// deliverable's block; Validate cannot know the declarations, so the session
// sizes them with each declared artifact on its suite's record.
func TestTheWorstBlockCountsDeclaredArtifacts(t *testing.T) {
	c := Check{Repo: "elk-work/app", SHA: strings.Repeat("a", 40), Workflow: ".github/workflows/b.yml", Job: "build",
		Suites: []string{"build"}, Suite: "build", Provider: "linux-lab"}
	without, with := c.worstRecordBlockWith(nil), c.worstRecordBlockWith([]Declaration{{Suite: "build", Kind: KindWorker, Path: "w.js"}})
	if with <= without {
		t.Fatalf("a declared artifact added nothing to the worst block: %d, %d", without, with)
	}
	if other := c.worstRecordBlockWith([]Declaration{{Suite: "lint", Kind: KindWorker, Path: "w.js"}}); other != without {
		t.Errorf("another suite's declaration counted: %d, want %d", other, without)
	}
	if c.worstRecordBlock() != without {
		t.Error("worstRecordBlock is not worstRecordBlockWith(nil)")
	}
}

// Two suites of one run declaring the same bundle upload it once, and both
// records carry the one ref.
func TestSuitesSharingABundleUploadItOnce(t *testing.T) {
	ark := newFakeArk(t)
	dir, sha := buildRepo(t, ark, "version: 1\nsuites:\n  floor:\n    kind: worker\n    path: worker.js\n"+
		"  deploy-build:\n    kind: worker\n    path: worker.js\n")
	act, argsFile, _ := buildAct(t, "act-pass.jsonl")
	a := buildAdapter(t, act, ark)
	spec := baseSpec(dir, sha)
	spec.Prompt = packetWithSuites(sha, "floor", "lint", "deploy-build")
	spec.Env = map[string]string{"ARK_TOKEN": testArkToken}
	r := runCheck(t, a, spec, argsFile)
	uploads, records := 0, 0
	for _, q := range ark.requests() {
		switch {
		case q.path == "/v1/artifacts/upload-url":
			uploads++
		case strings.HasSuffix(q.path, "/artifacts"):
			records++
		}
	}
	if uploads != 1 || records != 1 {
		t.Errorf("%d uploads and %d artifact records for one bundle; want 1 and 1", uploads, records)
	}
	first, lint, third := recordFileAt(t, a, 0), recordFileAt(t, a, 1), recordFileAt(t, a, 2)
	if fmt.Sprint(first["artifacts"]) != fmt.Sprint(third["artifacts"]) || first["artifacts"] == nil {
		t.Errorf("the two suites' artifacts differ: %v, %v", first["artifacts"], third["artifacts"])
	}
	if _, ok := lint["artifacts"]; ok {
		t.Errorf("a suite that declared nothing carries artifacts: %v", lint["artifacts"])
	}
	if len(r.rec.Artifacts) != 1 {
		t.Errorf("the first record carries %d artifacts", len(r.rec.Artifacts))
	}
}

// A container image is saved from the Docker daemon the job built it on.
func TestAContainerImageIsSavedAndRecorded(t *testing.T) {
	ark := newFakeArk(t)
	dir, sha := buildRepo(t, ark, "version: 1\nsuites:\n  floor:\n    kind: container\n    image: elk-app:ci\n")
	act, argsFile, _ := buildAct(t, "act-pass.jsonl")
	a := buildAdapter(t, act, ark)
	a.Docker = stubScript(t, "docker", `if [ "$1 $2" = "image save" ] && [ "$3" = -o ] && [ "$5" = elk-app:ci ]; then printf 'image-archive' > "$4"; fi`)
	spec := baseSpec(dir, sha)
	spec.Env = map[string]string{"ARK_TOKEN": testArkToken}
	r := runCheck(t, a, spec, argsFile)
	sum := sha256.Sum256([]byte("image-archive"))
	if len(r.rec.Artifacts) != 1 || r.rec.Artifacts[0].Kind != KindContainer || r.rec.Artifacts[0].Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("artifacts %+v\n%s", r.rec.Artifacts, r.res.Summary)
	}
	if hasArgs(r.args, "--env") {
		t.Errorf("an image-only declaration handed the job an artifact directory: %v", r.args)
	}
	for _, q := range ark.requests() {
		if q.method == http.MethodPut && q.contentType != "application/x-tar" {
			t.Errorf("an image archive was sent as %q", q.contentType)
		}
		if strings.HasSuffix(q.path, "/artifacts") && !strings.Contains(q.body, `"name":"elk-app-ci.tar"`) {
			t.Errorf("the archive was recorded as %s", q.body)
		}
	}
}

// A credential the service does not know is named as such.
func TestAnUnknownArkCredentialSaysLogInAgain(t *testing.T) {
	ark := newFakeArk(t)
	dir, sha := buildRepo(t, ark, floorWorker)
	act, argsFile, _ := buildAct(t, "act-pass.jsonl")
	a := buildAdapter(t, act, ark)
	spec := baseSpec(dir, sha)
	spec.Env = map[string]string{"ARK_TOKEN": "arkc_revoked-credential-000"}
	r := runCheck(t, a, spec, argsFile)
	if len(r.rec.Artifacts) != 0 || !strings.Contains(r.res.Summary, "did not accept the credential from the queue's ARK_TOKEN") {
		t.Errorf("artifacts %+v\n%s", r.rec.Artifacts, r.res.Summary)
	}
	if strings.Contains(r.res.Summary, "arkc_revoked-credential-000") {
		t.Error("the refused credential reached the deliverable")
	}
}
