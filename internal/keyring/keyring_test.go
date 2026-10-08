package keyring_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/keyring"
)

func TestFileStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	s := keyring.NewFileStore(filepath.Join(dir, keyring.FileName))

	if _, err := s.Get("elk-scout", "mac-claude"); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("Get on an empty store = %v, want ErrNotFound", err)
	}

	if err := s.Set("elk-scout", "mac-claude", "tok-1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get("elk-scout", "mac-claude")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "tok-1" {
		t.Fatalf("Get = %q, want tok-1", got)
	}

	// Replacing one token must not disturb another, and (workspace, queue) is
	// the whole key: the same queue name in two workspaces is two tokens.
	if err := s.Set("signal", "mac-claude", "tok-2"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("elk-scout", "mac-claude", "tok-3"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, _ := s.Get("signal", "mac-claude"); got != "tok-2" {
		t.Errorf("signal/mac-claude = %q, want tok-2 — a replace leaked across workspaces", got)
	}
	if got, _ := s.Get("elk-scout", "mac-claude"); got != "tok-3" {
		t.Errorf("elk-scout/mac-claude = %q, want tok-3", got)
	}

	keys, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("List returned %d keys, want 2: %v", len(keys), keys)
	}
	if keys[0].String() != "elk-scout/mac-claude" || keys[1].String() != "signal/mac-claude" {
		t.Errorf("List is not sorted: %v", keys)
	}

	if err := s.Delete("elk-scout", "mac-claude"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("elk-scout", "mac-claude"); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	// Revocation is idempotent: deleting again is not an error.
	if err := s.Delete("elk-scout", "mac-claude"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if got, _ := s.Get("signal", "mac-claude"); got != "tok-2" {
		t.Errorf("Delete removed the wrong key; signal/mac-claude = %q", got)
	}
}

func TestFileStorePersistsAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), keyring.FileName)
	if err := keyring.NewFileStore(path).Set("elk-scout", "mac-codex", "tok"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := keyring.NewFileStore(path).Get("elk-scout", "mac-codex")
	if err != nil {
		t.Fatalf("Get from a second instance: %v", err)
	}
	if got != "tok" {
		t.Fatalf("Get = %q, want tok", got)
	}
}

func TestFileStorePermissions(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("POSIX permissions")
	}
	path := filepath.Join(t.TempDir(), "sub", keyring.FileName)
	s := keyring.NewFileStore(path)
	if err := s.Set("elk-scout", "mac-claude", "tok"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The fallback holds tokens in plaintext; 0600 is the only thing standing
	// between them and every other account on the box.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %v, want 0600", perm)
	}
	if info, err := os.Stat(filepath.Dir(path)); err == nil {
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("token dir mode = %v, want 0700", perm)
		}
	}
}

func TestFileStoreRejectsBadInput(t *testing.T) {
	s := keyring.NewFileStore(filepath.Join(t.TempDir(), keyring.FileName))
	for _, tc := range []struct{ name, ws, queue, tok string }{
		{"empty workspace", "", "mac-claude", "tok"},
		{"empty queue", "elk-scout", "", "tok"},
		{"empty token", "elk-scout", "mac-claude", ""},
		{"slash in workspace", "elk/scout", "mac-claude", "tok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Set(tc.ws, tc.queue, tc.tok); err == nil {
				t.Fatal("Set accepted it")
			}
		})
	}
}

func TestFileStoreCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), keyring.FileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := keyring.NewFileStore(path).Get("elk-scout", "mac-claude")
	if err == nil {
		t.Fatal("Get accepted a corrupt store")
	}
	// A corrupt store must not read as "no token": that would send `rein enrol`
	// off to mint a second one and leave the first live.
	if errors.Is(err, keyring.ErrNotFound) {
		t.Fatal("a corrupt store reported ErrNotFound")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file: %v", err)
	}
}

func TestKeyRoundtrip(t *testing.T) {
	k := keyring.Key{Workspace: "elk-scout", Queue: "mac-claude"}
	if k.String() != "elk-scout/mac-claude" {
		t.Fatalf("String = %q", k.String())
	}
	got, ok := keyring.ParseKey(k.String())
	if !ok || got != k {
		t.Fatalf("ParseKey = %v, %v; want %v, true", got, ok, k)
	}
	for _, bad := range []string{"", "no-slash", "/queue", "workspace/"} {
		if _, ok := keyring.ParseKey(bad); ok {
			t.Errorf("ParseKey(%q) reported ok", bad)
		}
	}
}

func TestOpenSelectsBackend(t *testing.T) {
	dir := t.TempDir()

	t.Setenv(keyring.EnvBackend, "file")
	s, err := keyring.Open(dir)
	if err != nil {
		t.Fatalf("Open(file): %v", err)
	}
	if s.Name() != "file" {
		t.Errorf("Name = %q, want file", s.Name())
	}
	fs, ok := s.(*keyring.FileStore)
	if !ok {
		t.Fatalf("Open(file) returned %T", s)
	}
	if want := filepath.Join(dir, keyring.FileName); fs.Path() != want {
		t.Errorf("Path = %q, want %q", fs.Path(), want)
	}

	t.Setenv(keyring.EnvBackend, "")
	s, err = keyring.Open(dir)
	if err != nil {
		t.Fatalf("Open(default): %v", err)
	}
	if s.Name() != "os" {
		t.Errorf("default backend = %q, want os — the fallback must never be silent", s.Name())
	}

	t.Setenv(keyring.EnvBackend, "sqlite")
	if _, err := keyring.Open(dir); err == nil {
		t.Error("Open accepted an unknown backend")
	}
}

func TestStoreInterface(t *testing.T) {
	var _ keyring.Store = keyring.OSStore{}
	var _ keyring.Store = keyring.NewFileStore("x")
}
