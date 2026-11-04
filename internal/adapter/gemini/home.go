package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The private home every Antigravity CLI session runs under.
//
// # Why there has to be one
//
// agy switches to the Gemini API key only when its settings file says
// `"modelProvider": "gemini"`, and it reads that file from exactly one place:
// $HOME/.gemini/antigravity-cli/settings.json (agy 1.3.4; no flag selects
// another file, and ANTIGRAVITY_CONFIG_REL_DIR was tried and ignored). Writing
// that line into the person's own settings would move their own interactive
// `agy` onto their key, billed per token, without their knowing. So Rein gives
// agy a home of its own, under Rein's config directory, and writes the line
// there.
//
// The same home is what keeps the session off a Google sign-in: with
// modelProvider set and no key, agy refuses to start ("GEMINI_API_KEY
// environment variable is not set") rather than signing in, and the person's
// own ~/.gemini — their sign-in, their plugins, skills and MCP servers — is not
// in it at all.
//
// # What else is in it
//
// Everything else at the top of the person's real home, as a symlink: the
// agent's shell still needs git's identity, `gh`'s login, SSH, the toolchain
// caches under ~/go, ~/.cargo or ~/Library, and a copy of each would be wrong
// as well as large. Left out are the agent configuration directories — of
// this vendor and of the others — so no personal agent setup reaches the run.
//
// # Why it is kept
//
// One home per machine, not per run, because agy keeps its conversations in
// it and resume (`--conversation`) needs them on the next run. [Adapter.ReapRun]
// removes a run's conversation once the run has aged out.

// excluded are the top-level names of the real home that are never linked.
var excluded = map[string]bool{
	".gemini":             true, // Antigravity and Gemini CLI: sign-in, settings, plugins, MCP servers
	".antigravity":        true,
	".antigravity-plugin": true,
	".agents":             true, // agy discovers skills and workflows here
	".claude":             true,
	".claude.json":        true,
	".codex":              true,
	".grok":               true,
	".cursor":             true,
}

// settingsRel is agy's settings file, relative to its home.
var settingsRel = filepath.Join(".gemini", "antigravity-cli", "settings.json")

type home struct {
	dir  string // the private home
	root string // its parent: the adapter's own directory
	real string // the person's home
}

// prepareHome builds or refreshes the private home. It is idempotent and safe
// to run from two sessions at once.
func (a *Adapter) prepareHome() (*home, error) {
	root, err := a.root()
	if err != nil {
		return nil, err
	}
	real, err := a.realHome()
	if err != nil {
		return nil, fmt.Errorf("cannot locate the home directory: %w", err)
	}
	h := &home{dir: filepath.Join(root, "home"), root: root, real: real}
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating the private home for Antigravity CLI: %w", err)
	}
	if err := h.link(); err != nil {
		return nil, err
	}
	if err := h.writeSettings(); err != nil {
		return nil, err
	}
	return h, nil
}

// link mirrors the real home's top level as symlinks. An entry agy itself
// made in the private home is never replaced; a link whose target has gone is
// removed.
func (h *home) link() error {
	entries, err := os.ReadDir(h.real)
	if err != nil {
		return fmt.Errorf("reading %s: %w", h.real, err)
	}
	// Rein's own directory, when it sits in the home, would make the private
	// home contain itself.
	selfTop := topLevelUnder(h.real, h.root)
	for _, e := range entries {
		name := e.Name()
		if excluded[name] || name == selfTop {
			continue
		}
		target := filepath.Join(h.real, name)
		link := filepath.Join(h.dir, name)
		if info, err := os.Lstat(link); err == nil {
			if info.Mode()&os.ModeSymlink == 0 {
				continue // agy's own, or the person's own — never touched
			}
			if got, err := os.Readlink(link); err == nil && got == target {
				continue
			}
			_ = os.Remove(link)
		}
		if err := os.Symlink(target, link); err != nil && !os.IsExist(err) {
			return fmt.Errorf("linking %s into the private home: %w", target, err)
		}
	}
	// Drop links to things that are no longer there.
	mine, err := os.ReadDir(h.dir)
	if err != nil {
		return err
	}
	for _, e := range mine {
		link := filepath.Join(h.dir, e.Name())
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if excluded[e.Name()] {
			_ = os.Remove(link)
			continue
		}
		if _, err := os.Stat(link); os.IsNotExist(err) {
			_ = os.Remove(link)
		}
	}
	return nil
}

// topLevelUnder is the first path element of child below parent, or "" when
// child is not inside parent.
func topLevelUnder(parent, child string) string {
	rel, err := filepath.Rel(parent, child)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return ""
	}
	return strings.SplitN(rel, string(os.PathSeparator), 2)[0]
}

// writeSettings sets `"modelProvider": "gemini"` in the private home's agy
// settings, keeping whatever else agy has written there, and writes the file
// only when that changes it.
func (h *home) writeSettings() error {
	path := filepath.Join(h.dir, settingsRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating Antigravity CLI's settings directory: %w", err)
	}
	settings := map[string]any{}
	old, err := os.ReadFile(path)
	if err == nil && len(bytes.TrimSpace(old)) > 0 {
		if err := json.Unmarshal(old, &settings); err != nil {
			// A file Rein cannot read is replaced rather than trusted: what
			// matters is that key mode is on.
			settings = map[string]any{}
		}
	}
	if settings["modelProvider"] == "gemini" {
		return nil
	}
	settings["modelProvider"] = "gemini"
	blob, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.json")
	if err != nil {
		return fmt.Errorf("writing Antigravity CLI's settings: %w", err)
	}
	_, werr := tmp.Write(append(blob, '\n'))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing Antigravity CLI's settings: %v", firstErr(werr, cerr))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing Antigravity CLI's settings: %w", err)
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// strippedPrefixes are variables the child never inherits, whatever a queue
// says: agy's own switches, any of which could move the session onto a Google
// sign-in or Google Cloud billing (AGY_ADC_AUTH, AGY_ACCOUNT) or send the key
// to another host (GOOGLE_GEMINI_BASE_URL).
var strippedPrefixes = []string{"AGY_", "ANTIGRAVITY_", "GOOGLE_GEMINI_", "GOOGLE_GENAI_"}

// childEnv is the session's environment: base (the scoped inheritance), the
// spec's own variables, then the private home and the key, which nothing
// before them can override.
func (h *home) childEnv(base []string, over map[string]string, key string) []string {
	out := make([]string, 0, len(base)+len(over)+2)
	keep := func(name string) bool {
		if name == "HOME" || name == KeyName {
			return false
		}
		for _, p := range strippedPrefixes {
			if strings.HasPrefix(name, p) {
				return false
			}
		}
		_, shadowed := over[name]
		return !shadowed
	}
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 && keep(kv[:i]) {
			out = append(out, kv)
		}
	}
	for k, v := range over {
		if k == "HOME" || k == KeyName {
			continue
		}
		stripped := false
		for _, p := range strippedPrefixes {
			if strings.HasPrefix(k, p) {
				stripped = true
			}
		}
		if !stripped {
			out = append(out, k+"="+v)
		}
	}
	out = append(out, "HOME="+h.dir, KeyName+"="+key)
	return out
}
