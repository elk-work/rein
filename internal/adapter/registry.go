package adapter

import (
	"fmt"
	"sort"
	"sync"
)

// The registry maps an agent kind to the adapter that drives it. It is what
// turns `agent_kind = "claude"` in config.toml into a running Claude Code
// session without the run loop importing internal/adapter/claude.
//
// Adapters register themselves from an init function in their own package;
// the binary decides which exist by which packages it imports. That import is
// the whole wiring:
//
//	import _ "github.com/elk-work/rein/internal/adapter/claude"
//
// It also means a build can honestly answer "which agents can this machine
// drive" from [Kinds], and that internal/adapter/fake is present only in the
// binaries that import it.

var registry struct {
	sync.RWMutex
	byKind map[string]Adapter
}

// Register adds an adapter under its [Adapter.Name]. It rejects a duplicate
// kind, a name that disagrees with the manifest, and a manifest that does not
// [Manifest.Validate] — a malformed declaration is caught at startup, where a
// person is watching, rather than at dispatch, where one is not.
func Register(a Adapter) error {
	if a == nil {
		return fmt.Errorf("adapter: Register(nil)")
	}
	kind := a.Name()
	m := a.Manifest()
	if err := m.Validate(); err != nil {
		return err
	}
	if kind != m.Kind {
		return fmt.Errorf("adapter: Name() is %q but manifest kind is %q", kind, m.Kind)
	}

	registry.Lock()
	defer registry.Unlock()
	if registry.byKind == nil {
		registry.byKind = map[string]Adapter{}
	}
	if _, dup := registry.byKind[kind]; dup {
		return fmt.Errorf("adapter: %q is already registered", kind)
	}
	registry.byKind[kind] = a
	return nil
}

// MustRegister is [Register] for an init function, where returning an error
// has nowhere to go. It panics on failure.
func MustRegister(a Adapter) {
	if err := Register(a); err != nil {
		panic(err)
	}
}

// Unregister removes an adapter. It exists for tests; a daemon never calls it.
func Unregister(kind string) {
	registry.Lock()
	defer registry.Unlock()
	delete(registry.byKind, kind)
}

// Lookup returns the adapter for an agent kind.
func Lookup(kind string) (Adapter, bool) {
	registry.RLock()
	defer registry.RUnlock()
	a, ok := registry.byKind[kind]
	return a, ok
}

// Kinds returns every registered agent kind, sorted. This is what a machine
// can drive, before any check of what is installed on it — [Adapter.Preflight]
// answers that.
func Kinds() []string {
	registry.RLock()
	defer registry.RUnlock()
	out := make([]string, 0, len(registry.byKind))
	for k := range registry.byKind {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Manifests returns every registered adapter's manifest, keyed by kind — what
// `rein enrol` reports to Elk as this executor's declared capabilities
// (elk docs/rein.md §5b).
func Manifests() map[string]Manifest {
	registry.RLock()
	defer registry.RUnlock()
	out := make(map[string]Manifest, len(registry.byKind))
	for k, a := range registry.byKind {
		out[k] = a.Manifest()
	}
	return out
}

// UnknownKindError reports a kind with no adapter, naming the kinds there are
// so the message is usable without a second command.
type UnknownKindError struct {
	Kind  string
	Known []string
}

func (e *UnknownKindError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("adapter: no adapter for %q; none are registered in this build", e.Kind)
	}
	return fmt.Sprintf("adapter: no adapter for %q; this build has %v", e.Kind, e.Known)
}

// Get is [Lookup] with an error instead of a boolean, for callers that are
// going to report the failure rather than branch on it.
func Get(kind string) (Adapter, error) {
	if a, ok := Lookup(kind); ok {
		return a, nil
	}
	return nil, &UnknownKindError{Kind: kind, Known: Kinds()}
}
