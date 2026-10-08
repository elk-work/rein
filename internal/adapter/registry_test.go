package adapter_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

// stub is a minimal Adapter for registry tests: it registers and does nothing
// else. The behavioural double is internal/adapter/fake.
type stub struct {
	kind string
	m    adapter.Manifest
}

func (s stub) Name() string                    { return s.kind }
func (s stub) Manifest() adapter.Manifest      { return s.m }
func (s stub) Preflight(context.Context) error { return nil }
func (s stub) Start(context.Context, adapter.RunSpec) (adapter.Session, error) {
	return nil, adapter.ErrNotSupported
}

func newStub(kind string) stub {
	m := full()
	m.Kind = kind
	return stub{kind: kind, m: m}
}

// register adds an adapter and removes it when the test ends, so the tests do
// not depend on each other's ordering.
func register(t *testing.T, a adapter.Adapter) {
	t.Helper()
	if err := adapter.Register(a); err != nil {
		t.Fatalf("Register(%s): %v", a.Name(), err)
	}
	t.Cleanup(func() { adapter.Unregister(a.Name()) })
}

func TestRegisterAndLookup(t *testing.T) {
	register(t, newStub("reg-lookup"))

	got, ok := adapter.Lookup("reg-lookup")
	if !ok {
		t.Fatal("Lookup returned false for a registered kind")
	}
	if got.Name() != "reg-lookup" {
		t.Fatalf("Lookup returned %q", got.Name())
	}
	if _, ok := adapter.Lookup("reg-nothing"); ok {
		t.Fatal("Lookup returned true for an unregistered kind")
	}
}

func TestRegisterRejectsDuplicate(t *testing.T) {
	register(t, newStub("reg-dup"))
	err := adapter.Register(newStub("reg-dup"))
	if err == nil {
		t.Fatal("Register accepted a duplicate kind")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("err = %v", err)
	}
}

func TestRegisterRejectsInvalidManifest(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    adapter.Adapter
		want string
	}{
		{"nil", nil, "Register(nil)"},
		{
			"unknown capability",
			stub{kind: "reg-bad-cap", m: adapter.Manifest{
				Kind:         "reg-bad-cap",
				Capabilities: map[adapter.Capability]adapter.Support{"telepathy": adapter.SupportYes},
				Platforms:    []adapter.Platform{adapter.AnyArch(runtime.GOOS)},
			}},
			"unknown capability",
		},
		{
			"no platforms",
			stub{kind: "reg-no-plat", m: adapter.Manifest{Kind: "reg-no-plat"}},
			"declares no platforms",
		},
		{
			"name disagrees with the manifest",
			stub{kind: "reg-a", m: func() adapter.Manifest { m := full(); m.Kind = "reg-b"; return m }()},
			"manifest kind is",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := adapter.Register(tc.a)
			if err == nil {
				if tc.a != nil {
					adapter.Unregister(tc.a.Name())
				}
				t.Fatal("Register accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestMustRegisterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustRegister did not panic on an invalid adapter")
		}
	}()
	adapter.MustRegister(stub{kind: "reg-panic", m: adapter.Manifest{Kind: "reg-panic"}})
}

func TestKindsSortedAndComplete(t *testing.T) {
	register(t, newStub("reg-zulu"))
	register(t, newStub("reg-alpha"))

	kinds := adapter.Kinds()
	var alpha, zulu int = -1, -1
	for i, k := range kinds {
		switch k {
		case "reg-alpha":
			alpha = i
		case "reg-zulu":
			zulu = i
		}
	}
	if alpha < 0 || zulu < 0 {
		t.Fatalf("Kinds = %v, missing one of the registered kinds", kinds)
	}
	if alpha > zulu {
		t.Errorf("Kinds is not sorted: %v", kinds)
	}
}

func TestManifests(t *testing.T) {
	register(t, newStub("reg-manifests"))
	ms := adapter.Manifests()
	m, ok := ms["reg-manifests"]
	if !ok {
		t.Fatal("Manifests is missing a registered adapter")
	}
	if !m.Has(adapter.CapGit) {
		t.Error("the manifest came back without its capabilities")
	}
}

func TestGetNamesWhatExists(t *testing.T) {
	register(t, newStub("reg-get"))

	if _, err := adapter.Get("reg-get"); err != nil {
		t.Fatalf("Get = %v, want nil", err)
	}

	_, err := adapter.Get("teleporter")
	if err == nil {
		t.Fatal("Get accepted an unregistered kind")
	}
	var uke *adapter.UnknownKindError
	if !errors.As(err, &uke) {
		t.Fatalf("err is %T, want *UnknownKindError", err)
	}
	if uke.Kind != "teleporter" {
		t.Errorf("Kind = %q", uke.Kind)
	}
	// The message has to be usable without a second command: it names what
	// this build does have.
	if !strings.Contains(err.Error(), "reg-get") {
		t.Errorf("the message does not list the registered kinds: %v", err)
	}
}

func TestRegistryIsConcurrencySafe(t *testing.T) {
	register(t, newStub("reg-race"))
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				adapter.Lookup("reg-race")
				adapter.Kinds()
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
