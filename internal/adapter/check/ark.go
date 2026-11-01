package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/elk-work/rein/internal/keyring"
)

// Recording a build's bytes in Ark (ark:rein#100 (01M6AMERCA97G5D02P6HEGCDFZ)).
//
// The bytes become an Ark artifact on an Ark run of the check, in the Ark
// repository the queue's checkout is joined to, through Ark's own routes
// (elk-work/ark#131, ark docs/self-hosting.md "Runs and their artifacts"):
//
//	POST /v1/artifacts/upload-url, PUT the bytes, POST /v1/artifacts/confirm
//	POST /v1/repositories/{repo}/runs                  open the check's run
//	POST /v1/repositories/{repo}/runs/{run}/artifacts  record each blob on it
//	POST /v1/repositories/{repo}/runs/{run}/finish     succeeded, or failed
//
// The credential is the one already on this machine, found the way the ark
// CLI finds it (ark internal/cloud/creds.go): ARK_TOKEN, then the OS keyring
// under service "ark" and the remote's host, then ~/.ark/credentials.toml.
// Elk never issues an Ark credential (ark RFC-0003 Decision 5), and Rein
// mints none: with no credential, or none with a write grant, the check
// records no artifacts and says why, and a deploy is refused for want of one.

// arkConfig is the part of .ark/config.toml Rein reads (ark internal/config).
type arkConfig struct {
	RepositoryID     string `toml:"repository_id"`
	Remote           string `toml:"remote"`
	DefaultActorID   string `toml:"default_actor_id"`
	DefaultActorType string `toml:"default_actor_type"`
}

// arkTarget is where and as whom the check records: the repository, the
// service, the human its writer acts for, and the credential.
type arkTarget struct {
	repo        string
	remote      string
	delegatedBy string
	token       string
	// source names the store the credential came from, for the
	// deliverable: never the credential itself.
	source string
}

var arkIDRE = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// The environment variables of Ark's own that a check honours. They are
// Rein's to use and never the workflow's: act's environment drops them, as it
// drops Stride's, or a queue that mapped ARK_TOKEN would hand the Ark
// credential to every step.
var arkVariables = map[string]bool{"ARK_TOKEN": true, "ARK_DELEGATED_BY": true}

// arkRoot is the checkout whose .ark the worktree's records belong to: the
// worktree's git common directory is that checkout's .git, and a check
// worktree carries no .ark of its own.
func arkRoot(ctx context.Context, git, worktree string) string {
	common := gitOut(ctx, git, worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if common == "" {
		return ""
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return common
}

// resolveArk finds the Ark repository, the delegating human and the
// credential, or says in a sentence why the check cannot record there.
// queueEnv is the queue's scoped secrets, which outrank the daemon's own
// environment exactly as they do for everything else a run is given.
func (a *Adapter) resolveArk(root string, queueEnv map[string]string) (arkTarget, error) {
	if root == "" {
		return arkTarget{}, errors.New("the worktree's checkout could not be found, so neither could its Ark repository")
	}
	var cfg arkConfig
	if _, err := toml.DecodeFile(filepath.Join(root, ".ark", "config.toml"), &cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return arkTarget{}, fmt.Errorf("this machine's checkout of the repository (%s) is not joined to Ark: it has no .ark/config.toml", root)
		}
		return arkTarget{}, fmt.Errorf("%s/.ark/config.toml could not be read", root)
	}
	if !arkIDRE.MatchString(cfg.RepositoryID) {
		return arkTarget{}, fmt.Errorf("%s/.ark/config.toml names no Ark repository id", root)
	}
	remote := strings.TrimSuffix(strings.TrimSpace(cfg.Remote), "/")
	u, err := url.Parse(remote)
	if remote == "" || err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return arkTarget{}, fmt.Errorf("Ark repository %s has no sync service configured on this machine (`ark remote`)", cfg.RepositoryID)
	}
	t := arkTarget{repo: cfg.RepositoryID, remote: remote}

	// The writer acts for a human, as `ark --agent` does: ARK_DELEGATED_BY,
	// else the checkout's default actor when that is a human (ark
	// internal/app/app.go resolveActor).
	lookup := func(name string) string {
		if v := strings.TrimSpace(queueEnv[name]); v != "" {
			return v
		}
		return strings.TrimSpace(os.Getenv(name))
	}
	switch v := lookup("ARK_DELEGATED_BY"); {
	case v != "":
		t.delegatedBy = v
	case cfg.DefaultActorType == "human":
		t.delegatedBy = cfg.DefaultActorID
	}
	if !arkIDRE.MatchString(t.delegatedBy) {
		return arkTarget{}, fmt.Errorf("Ark repository %s names no human for Rein to act for: set ARK_DELEGATED_BY, or default_actor_id to a human actor in .ark/config.toml", cfg.RepositoryID)
	}

	// The credential, in the ark CLI's order (ark docs/v1-spec.md §20).
	host := u.Host
	if v := strings.TrimSpace(queueEnv["ARK_TOKEN"]); v != "" {
		t.token, t.source = v, "the queue's ARK_TOKEN"
		return t, nil
	}
	if v := strings.TrimSpace(os.Getenv("ARK_TOKEN")); v != "" {
		t.token, t.source = v, "ARK_TOKEN"
		return t, nil
	}
	if !arkKeyringDisabled() {
		items := a.ArkKeyring
		if items == nil {
			items = keyring.OSItems{}
		}
		if v, err := items.ReadItem("ark", host); err == nil && strings.TrimSpace(v) != "" {
			t.token, t.source = strings.TrimSpace(v), "this machine's Ark login (keyring)"
			return t, nil
		}
	}
	if v := a.arkFileToken(host); v != "" {
		t.token, t.source = v, "this machine's Ark login (~/.ark/credentials.toml)"
		return t, nil
	}
	return arkTarget{}, fmt.Errorf("this machine holds no Ark credential for %s (no ARK_TOKEN, no `ark login`), and Rein uploads only with one already here", host)
}

// arkKeyringDisabled is ARK_NO_KEYRING, read as ark reads it.
func arkKeyringDisabled() bool {
	v := strings.TrimSpace(os.Getenv("ARK_NO_KEYRING"))
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}

// arkFileToken is the host's token in ark's fallback credentials file, or "".
func (a *Adapter) arkFileToken(host string) string {
	home := a.ArkHome
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = h
	}
	var f struct {
		Remotes map[string]struct {
			Token string `toml:"token"`
		} `toml:"remotes"`
	}
	if _, err := toml.DecodeFile(filepath.Join(home, ".ark", "credentials.toml"), &f); err != nil {
		return ""
	}
	return strings.TrimSpace(f.Remotes[host].Token)
}

// The wire shapes, as ark pkg/api defines them.
type (
	arkWriter struct {
		AgentName   string `json:"agent_name"`
		DelegatedBy string `json:"delegated_by"`
	}
	arkUploadRequest struct {
		RepositoryID string `json:"repository_id"`
		SHA256       string `json:"sha256"`
		SizeBytes    int64  `json:"size_bytes,omitempty"`
		MediaType    string `json:"media_type,omitempty"`
	}
	arkUploadResponse struct {
		URL           string `json:"url"`
		StorageKey    string `json:"storage_key"`
		AlreadyStored bool   `json:"already_stored"`
	}
	arkCreateRun struct {
		Writer        arkWriter       `json:"writer"`
		InputSummary  string          `json:"input_summary,omitempty"`
		BaseCommitSHA string          `json:"base_commit_sha,omitempty"`
		Metadata      json.RawMessage `json:"metadata,omitempty"`
	}
	arkFinishRun struct {
		Writer        arkWriter `json:"writer"`
		Status        string    `json:"status"`
		ResultSummary string    `json:"result_summary,omitempty"`
	}
	arkCreateArtifact struct {
		Writer    arkWriter `json:"writer"`
		Name      string    `json:"name"`
		MediaType string    `json:"media_type,omitempty"`
		SHA256    string    `json:"sha256"`
		SizeBytes int64     `json:"size_bytes"`
	}
	arkRecordResponse struct {
		Record struct {
			RecordID string `json:"record_id"`
			ID       string `json:"id"`
		} `json:"record"`
	}
)

// arkWriterName is the agent Rein writes as. Ark resolves it, with the
// delegating human, to one agent actor in the repository.
const arkWriterName = "rein"

// arkError is a refusal from the service, with its status.
type arkError struct {
	status  int
	code    string
	message string
}

func (e *arkError) Error() string {
	msg := e.message
	if msg == "" {
		msg = http.StatusText(e.status)
	}
	return fmt.Sprintf("%d %s: %s", e.status, e.code, msg)
}

// refused reports a credential the service would not accept, or one with no
// grant for this.
func refused(err error) (status int, ok bool) {
	var ae *arkError
	if errors.As(err, &ae) && (ae.status == http.StatusUnauthorized || ae.status == http.StatusForbidden) {
		return ae.status, true
	}
	return 0, false
}

// arkClient talks to one Ark service with one credential.
type arkClient struct {
	t    arkTarget
	http *http.Client
}

func (a *Adapter) arkClient(t arkTarget) *arkClient {
	c := a.ArkHTTP
	if c == nil {
		c = &http.Client{}
	}
	return &arkClient{t: t, http: c}
}

// call sends one JSON request with the credential; key, when set, is the
// Idempotency-Key the write routes take.
func (c *arkClient) call(ctx context.Context, path, key string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.t.remote+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.t.token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %s", c.t.remote, scrubURLError(err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return &arkError{status: resp.StatusCode, code: e.Code, message: firstLine(e.Message)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// put stores the bytes at the signed URL upload-url returned. The URL is its
// own authority, so the request carries no Ark credential: a signed URL may
// point at object storage, which has no business seeing one.
func (c *arkClient) put(ctx context.Context, signed, mediaType, file string, size int64) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, signed, f)
	if err != nil {
		return errors.New("the service returned an upload URL Rein could not use")
	}
	req.ContentLength = size
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("the upload failed: %s", scrubURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("the upload was refused (%d): %s", resp.StatusCode, firstLine(string(data)))
	}
	return nil
}

// scrubURLError is a transport error without its URL, which for the PUT is a
// signed one: a bearer of its own until it expires.
func scrubURLError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// storeBlob uploads one file and has the service confirm it hashes to sha.
func (c *arkClient) storeBlob(ctx context.Context, b blob) error {
	req := arkUploadRequest{RepositoryID: c.t.repo, SHA256: b.sha256, SizeBytes: b.size, MediaType: b.mediaType}
	var up arkUploadResponse
	if err := c.call(ctx, "/v1/artifacts/upload-url", "", req, &up); err != nil {
		return err
	}
	if !up.AlreadyStored {
		if up.URL == "" {
			return errors.New("the service returned no upload URL")
		}
		if err := c.put(ctx, up.URL, b.mediaType, b.file, b.size); err != nil {
			return err
		}
	}
	return c.call(ctx, "/v1/artifacts/confirm", "", arkUploadRequest{RepositoryID: c.t.repo, SHA256: b.sha256}, nil)
}

func (c *arkClient) writer() arkWriter {
	return arkWriter{AgentName: arkWriterName, DelegatedBy: c.t.delegatedBy}
}

// openRun opens the check's Ark run. key makes a retried claim of the same
// Elk run find the run it already opened rather than open another.
func (c *arkClient) openRun(ctx context.Context, key, summary, sha string, metadata any) (string, error) {
	meta, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	var resp arkRecordResponse
	if err := c.call(ctx, "/v1/repositories/"+url.PathEscape(c.t.repo)+"/runs", key,
		arkCreateRun{Writer: c.writer(), InputSummary: summary, BaseCommitSHA: sha, Metadata: meta}, &resp); err != nil {
		return "", err
	}
	return recordID(resp)
}

// recordArtifact records a confirmed blob as an artifact of the run and
// returns the artifact's record id: the suite record's ref.
func (c *arkClient) recordArtifact(ctx context.Context, run, key string, b blob) (string, error) {
	var resp arkRecordResponse
	if err := c.call(ctx, "/v1/repositories/"+url.PathEscape(c.t.repo)+"/runs/"+url.PathEscape(run)+"/artifacts", key,
		arkCreateArtifact{Writer: c.writer(), Name: b.name, MediaType: b.mediaType, SHA256: b.sha256, SizeBytes: b.size}, &resp); err != nil {
		return "", err
	}
	return recordID(resp)
}

// finishRun finishes the run, once.
func (c *arkClient) finishRun(ctx context.Context, run, key, status, summary string) error {
	return c.call(ctx, "/v1/repositories/"+url.PathEscape(c.t.repo)+"/runs/"+url.PathEscape(run)+"/finish", key,
		arkFinishRun{Writer: c.writer(), Status: status, ResultSummary: summary}, nil)
}

func recordID(r arkRecordResponse) (string, error) {
	id := r.Record.RecordID
	if id == "" {
		id = r.Record.ID
	}
	if !arkIDRE.MatchString(id) {
		return "", errors.New("the service's answer named no record id")
	}
	return id, nil
}
