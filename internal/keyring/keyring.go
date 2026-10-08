// Package keyring stores the run-scoped Elk tokens Rein enrols with.
//
// One token per (workspace, queue): a machine that serves mac-claude and
// mac-codex in two workspaces holds four, and revoking one must not touch the
// others. The OS keychain is the store — Keychain on macOS, Credential Manager
// on Windows, Secret Service on Linux — reached through
// github.com/zalando/go-keyring, under the service name "rein".
//
// A file-backed [Store] exists for CI and tests, where no keychain daemon is
// running and an unlocked one would be a worse answer than an explicit file.
// It is never chosen silently for a real run: [Open] uses it only when
// REIN_KEYRING=file says so. A headless Linux box with no Secret Service gets
// an error naming the variable, not a quiet downgrade to plaintext on disk —
// the failure mode that turns "the token is in the keychain" from a fact into a
// hope.
package keyring

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

// Service is the keychain service name every Rein secret is filed under.
const Service = "rein"

// EnvBackend selects the backend. "file" forces the file-backed fallback;
// "os" (or empty) uses the OS keychain.
const EnvBackend = "REIN_KEYRING"

// FileName is the file-backed store's basename inside the Rein home.
const FileName = "tokens.json"

// ErrNotFound reports that no token is stored for a (workspace, queue).
var ErrNotFound = errors.New("keyring: no token stored")

// Store is the token store. Implementations are safe for concurrent use.
type Store interface {
	// Get returns the token for a (workspace, queue), or a wrapped
	// [ErrNotFound].
	Get(workspace, queue string) (string, error)
	// Set stores (or replaces) the token for a (workspace, queue).
	Set(workspace, queue, token string) error
	// Delete removes the token for a (workspace, queue). Deleting one that is
	// not there is not an error — revocation should be idempotent.
	Delete(workspace, queue string) error
	// List returns the (workspace, queue) pairs that have a token, sorted. It
	// never returns the tokens themselves; `rein status` needs to say which
	// queues are enrolled without reading any secret.
	List() ([]Key, error)
	// Name identifies the backend for diagnostics: "os" or "file".
	Name() string
}

// Key identifies one stored token.
type Key struct {
	Workspace string
	Queue     string
}

// String renders the key as it is stored in the keychain: "<workspace>/<queue>".
func (k Key) String() string { return k.Workspace + "/" + k.Queue }

// ParseKey is the inverse of [Key.String].
func ParseKey(s string) (Key, bool) {
	ws, q, ok := strings.Cut(s, "/")
	if !ok || ws == "" || q == "" {
		return Key{}, false
	}
	return Key{Workspace: ws, Queue: q}, true
}

func validate(workspace, queue string) (Key, error) {
	if workspace == "" {
		return Key{}, errors.New("keyring: workspace is required")
	}
	if queue == "" {
		return Key{}, errors.New("keyring: queue is required")
	}
	if strings.Contains(workspace, "/") {
		return Key{}, fmt.Errorf("keyring: workspace %q must not contain %q", workspace, "/")
	}
	return Key{Workspace: workspace, Queue: queue}, nil
}

// Open returns the [Store] this environment asks for. dir is the Rein home,
// used only by the file backend.
func Open(dir string) (Store, error) {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvBackend))); v {
	case "", "os", "keychain":
		return OSStore{}, nil
	case "file":
		return NewFileStore(filepath.Join(dir, FileName)), nil
	default:
		return nil, fmt.Errorf("keyring: %s=%q: want \"os\" or \"file\"", EnvBackend, v)
	}
}

// OSStore is the OS keychain backend.
type OSStore struct{}

// Name implements [Store].
func (OSStore) Name() string { return "os" }

// Get implements [Store].
func (OSStore) Get(workspace, queue string) (string, error) {
	k, err := validate(workspace, queue)
	if err != nil {
		return "", err
	}
	tok, err := keyring.Get(Service, k.String())
	if errors.Is(err, keyring.ErrNotFound) {
		return "", fmt.Errorf("%w for %s", ErrNotFound, k)
	}
	if err != nil {
		return "", fmt.Errorf("keyring: get %s: %w", k, err)
	}
	return tok, nil
}

// Set implements [Store].
func (OSStore) Set(workspace, queue, token string) error {
	k, err := validate(workspace, queue)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("keyring: refusing to store an empty token")
	}
	if err := keyring.Set(Service, k.String(), token); err != nil {
		return fmt.Errorf("keyring: set %s: %w", k, err)
	}
	return nil
}

// Delete implements [Store].
func (OSStore) Delete(workspace, queue string) error {
	k, err := validate(workspace, queue)
	if err != nil {
		return err
	}
	err = keyring.Delete(Service, k.String())
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("keyring: delete %s: %w", k, err)
}

// List implements [Store]. The OS keychain APIs go-keyring wraps have no
// enumeration, so the caller's config is the index: `rein status` walks
// config.Queues and probes each. Returning an empty list rather than an error
// keeps that seam honest — this store knows nothing it was not asked about.
func (OSStore) List() ([]Key, error) { return nil, nil }

// FileStore is the file-backed fallback: a 0600 JSON object of
// "<workspace>/<queue>" to token, at a fixed path. Plaintext, deliberately and
// only where a keychain does not exist — see the package doc.
type FileStore struct {
	path string
	mu   sync.Mutex
}

// NewFileStore returns a [FileStore] at path. The file is created on first
// write.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

// Path returns the file the store reads and writes.
func (f *FileStore) Path() string { return f.path }

// Name implements [Store].
func (f *FileStore) Name() string { return "file" }

func (f *FileStore) load() (map[string]string, error) {
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("keyring: read %s: %w", f.path, err)
	}
	m := map[string]string{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("keyring: parse %s: %w", f.path, err)
		}
	}
	return m, nil
}

func (f *FileStore) store(m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("keyring: create %s: %w", filepath.Dir(f.path), err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("keyring: encode: %w", err)
	}
	if err := os.WriteFile(f.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("keyring: write %s: %w", f.path, err)
	}
	return nil
}

// Get implements [Store].
func (f *FileStore) Get(workspace, queue string) (string, error) {
	k, err := validate(workspace, queue)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return "", err
	}
	tok, ok := m[k.String()]
	if !ok {
		return "", fmt.Errorf("%w for %s", ErrNotFound, k)
	}
	return tok, nil
}

// Set implements [Store].
func (f *FileStore) Set(workspace, queue, token string) error {
	k, err := validate(workspace, queue)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("keyring: refusing to store an empty token")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return err
	}
	m[k.String()] = token
	return f.store(m)
}

// Delete implements [Store].
func (f *FileStore) Delete(workspace, queue string) error {
	k, err := validate(workspace, queue)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return err
	}
	if _, ok := m[k.String()]; !ok {
		return nil
	}
	delete(m, k.String())
	return f.store(m)
}

// List implements [Store].
func (f *FileStore) List() ([]Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return nil, err
	}
	keys := make([]Key, 0, len(m))
	for s := range m {
		if k, ok := ParseKey(s); ok {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Workspace != keys[j].Workspace {
			return keys[i].Workspace < keys[j].Workspace
		}
		return keys[i].Queue < keys[j].Queue
	})
	return keys, nil
}

// ---------------------------------------------------------------------------
// Queue secrets (ark:rein#48).
//
// A scoped queue's config maps an environment variable NAME to a keychain item
// the person created themselves; Rein reads that item at the start of each run
// and hands the value to that run's agent process and nothing else. These are
// not Rein's own tokens, so they are not under [Service]: each item is
// addressed by the service and account the person gave it.

// SecretsFileName is the file-backed item store's basename inside the Rein
// home, used only under REIN_KEYRING=file — the same switch, and the same
// reason, as [FileName].
const SecretsFileName = "secrets.json"

// ErrItemNotFound reports that no keychain item exists at a service and
// account.
var ErrItemNotFound = errors.New("keyring: no such keychain item")

// reservedItemServices are the keychain services a queue's secrets map may
// never name: Rein's own per-queue Elk tokens, and a Wrangler queue's owner
// connector (internal/adapter/claude). Either one handed to a run would be a
// credential for a different queue, or a different person, than the run's own.
var reservedItemServices = map[string]bool{Service: true, "elk-connector-url": true}

// ReservedItemService reports whether a secrets map may not name this service.
func ReservedItemService(service string) bool { return reservedItemServices[service] }

// ItemReader reads the keychain items a scoped queue's secrets map names.
// Implementations are safe for concurrent use. It has no write and no list:
// the person creates the items, and the config is the only index of them.
type ItemReader interface {
	// ReadItem returns the value stored at (service, account), or a wrapped
	// [ErrItemNotFound]. An error never carries the value.
	ReadItem(service, account string) (string, error)
	// Name identifies the backend for diagnostics: "os" or "file".
	Name() string
}

// OpenItems returns the [ItemReader] this environment asks for, chosen by
// REIN_KEYRING exactly as [Open] chooses the token store.
func OpenItems(dir string) (ItemReader, error) {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvBackend))); v {
	case "", "os", "keychain":
		return OSItems{}, nil
	case "file":
		return NewFileItems(filepath.Join(dir, SecretsFileName)), nil
	default:
		return nil, fmt.Errorf("keyring: %s=%q: want \"os\" or \"file\"", EnvBackend, v)
	}
}

func checkItem(service, account string) error {
	switch {
	case service == "":
		return errors.New("keyring: an item needs a service")
	case account == "":
		return errors.New("keyring: an item needs an account")
	case ReservedItemService(service):
		return fmt.Errorf("keyring: service %q holds Rein's own credentials and is not readable as a queue secret", service)
	}
	return nil
}

// OSItems reads items from the OS keychain: Keychain on macOS, Credential
// Manager on Windows, Secret Service on Linux.
type OSItems struct{}

// Name implements [ItemReader].
func (OSItems) Name() string { return "os" }

// ReadItem implements [ItemReader].
func (OSItems) ReadItem(service, account string) (string, error) {
	if err := checkItem(service, account); err != nil {
		return "", err
	}
	v, err := keyring.Get(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", fmt.Errorf("%w: service %q, account %q", ErrItemNotFound, service, account)
	}
	if err != nil {
		// go-keyring's own error is the backend's exit status or D-Bus fault,
		// never the value — but it is not ours to vouch for, so only its type
		// of failure is passed on, not its text.
		return "", fmt.Errorf("keyring: reading service %q, account %q failed (the keychain refused or is locked)", service, account)
	}
	return v, nil
}

// FileItems is the file-backed fallback: a 0600 JSON object of
// "<service>/<account>" to value. Plaintext, and only for CI and tests — see
// the package doc.
type FileItems struct {
	path string
	mu   sync.Mutex
}

// NewFileItems returns a [FileItems] reading path.
func NewFileItems(path string) *FileItems { return &FileItems{path: path} }

// Name implements [ItemReader].
func (f *FileItems) Name() string { return "file" }

// ReadItem implements [ItemReader].
func (f *FileItems) ReadItem(service, account string) (string, error) {
	if err := checkItem(service, account); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := os.ReadFile(f.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("keyring: read %s: %w", f.path, err)
	}
	m := map[string]string{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			return "", fmt.Errorf("keyring: parse %s: not a JSON object of strings", f.path)
		}
	}
	v, ok := m[service+"/"+account]
	if !ok {
		return "", fmt.Errorf("%w: service %q, account %q", ErrItemNotFound, service, account)
	}
	return v, nil
}
