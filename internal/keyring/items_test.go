package keyring_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gokeyring "github.com/zalando/go-keyring"

	"github.com/elk-work/rein/internal/keyring"
)

// The keychain items a scoped queue's secrets map names (ark:rein#48).

const itemValue = "ITEMVALUE_0123456789abcdef"

func TestFileItemsReadsByServiceAndAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), keyring.SecretsFileName)
	if err := os.WriteFile(path, []byte(`{"acme-posthog/rein":"`+itemValue+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := keyring.NewFileItems(path)
	v, err := f.ReadItem("acme-posthog", "rein")
	if err != nil || v != itemValue {
		t.Fatalf("ReadItem = %v; want the stored value", err)
	}
	_, err = f.ReadItem("acme-posthog", "other")
	if !errors.Is(err, keyring.ErrItemNotFound) {
		t.Errorf("a missing account: %v, want ErrItemNotFound", err)
	}
	if err != nil && strings.Contains(err.Error(), itemValue) {
		t.Error("an error carried a value")
	}
}

func TestFileItemsCorruptFileNamesNoContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), keyring.SecretsFileName)
	if err := os.WriteFile(path, []byte(`{"svc/rein": "`+itemValue), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := keyring.NewFileItems(path).ReadItem("svc", "rein")
	if err == nil {
		t.Fatal("a corrupt file read cleanly")
	}
	if strings.Contains(err.Error(), "ITEMVALUE") {
		t.Errorf("the parse error quoted the file: %v", err)
	}
}

func TestItemReadersRefuseReinsOwnServices(t *testing.T) {
	gokeyring.MockInit()
	for _, r := range []keyring.ItemReader{keyring.OSItems{}, keyring.NewFileItems(filepath.Join(t.TempDir(), "x.json"))} {
		for _, svc := range []string{keyring.Service, "elk-connector-url"} {
			if _, err := r.ReadItem(svc, "Elk Scout/mac-claude"); err == nil || !strings.Contains(err.Error(), "Rein's own") {
				t.Errorf("%s reader read service %q: %v", r.Name(), svc, err)
			}
		}
		if _, err := r.ReadItem("", "rein"); err == nil {
			t.Errorf("%s reader accepted an empty service", r.Name())
		}
		if _, err := r.ReadItem("svc", ""); err == nil {
			t.Errorf("%s reader accepted an empty account", r.Name())
		}
	}
}

func TestOSItemsReadsTheKeychain(t *testing.T) {
	// go-keyring's in-memory provider, so the test never touches a real
	// keychain — and proves the service and account reach it as given.
	gokeyring.MockInit()
	if err := gokeyring.Set("acme-posthog", "rein", itemValue); err != nil {
		t.Fatal(err)
	}
	v, err := keyring.OSItems{}.ReadItem("acme-posthog", "rein")
	if err != nil || v != itemValue {
		t.Fatalf("ReadItem: %v", err)
	}
	_, err = keyring.OSItems{}.ReadItem("acme-posthog", "nobody")
	if !errors.Is(err, keyring.ErrItemNotFound) || !strings.Contains(err.Error(), `"acme-posthog"`) {
		t.Errorf("a missing item: %v, want ErrItemNotFound naming the service", err)
	}
}

func TestOpenItemsSelectsBackend(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(keyring.EnvBackend, "file")
	r, err := keyring.OpenItems(dir)
	if err != nil || r.Name() != "file" {
		t.Fatalf("OpenItems(file) = %v, %v", r, err)
	}
	t.Setenv(keyring.EnvBackend, "")
	if r, err = keyring.OpenItems(dir); err != nil || r.Name() != "os" {
		t.Errorf("default items backend = %v, %v; want os", r, err)
	}
	t.Setenv(keyring.EnvBackend, "sqlite")
	if _, err := keyring.OpenItems(dir); err == nil {
		t.Error("OpenItems accepted an unknown backend")
	}
}
