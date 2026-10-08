package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
)

// A run-private `$CODEX_HOME`, which is how a Rein session is kept from
// inheriting the developer's own agent configuration.
//
// # Why this exists
//
// On 2026-08-29 a dogfood run on this Mac submitted its own deliverable. The
// agent had picked up the Elk MCP connector out of the developer's personal
// config, called `submit_deliverable` **under his identity** mid-run, and Elk
// reviewed that submission; Rein then submitted again and the run ended in a
// state the review sweep ignores (`ark:rein#21`). The Codex adapter had the
// same exposure, verified rather than assumed: a probe passing one MCP server
// through `thread/start` saw four connect — the one Rein asked for, plus
// `elk`, `node_repl` and `codex_apps` from `~/.codex/config.toml`.
//
// A session must see **only** [adapter.RunSpec.MCPServers]. Nothing weaker is
// safe, because the inherited server is not merely extra capability — it is
// capability *as somebody else*.
//
// # Why a private home rather than an override
//
// `codex app-server -c mcp_servers={}` does not work: config overrides
// deep-merge, so an empty table changes nothing and all three servers still
// loaded. Disabling them one by one would mean reading the developer's
// `config.toml` for names and hoping none is added mid-run. `CODEX_HOME` is
// the switch that actually exists — Codex reads its entire configuration from
// there, so a home with no `config.toml` has no MCP servers, no hooks, no
// personal model pin, and nothing else the packet did not ask for.
//
// # What is linked in, from where, and why
//
// The private home starts with exactly two links.
//
//   - `auth.json`, from the developer's Codex home — the login. Rein does not
//     read, mint, refresh or replay a credential (elk `docs/rein.md` §3); a
//     symlink means the *vendor binary* opens its own credential file at a
//     different path, which is a different act from Rein handling the token.
//   - `sessions/`, from **Rein's own** Codex history, `$REIN_HOME/codex/` —
//     the rollout transcripts. Without it `Result.TranscriptPath` would point
//     into a directory this package is about to delete, and `thread/resume`
//     could not find the thread a previous session created. Resume is a
//     declared capability; it has to keep working.
//
// Everything else — `config.toml` above all — is simply absent, and whatever
// Codex writes during the session (its state DB, the thread-history
// projection, memories, logs) is thrown away with the directory.
//
// # Why Rein's history and not the developer's (ark:rein#38)
//
// Through v0.2.1 `sessions/` linked the developer's `~/.codex/sessions`. That is
// what stopped Codex runs starting. A home that has rollouts and no state DB
// makes `codex app-server` build the DB — an index over **every rollout under
// `sessions/`** — *before it answers `initialize`*. On the developer's Mac that
// was 1.9 GB / 64,066 rollouts: 39 s (0.154.0) to 62 s (0.159.0) in a
// terminal, 49 s under launchd on 2026-08-31, and past the 90 s startup
// timeout by 2026-09-29, when mac-codex stopped starting at all.
//
// Linking the developer's state DB as well made `initialize` answer in a
// second and broke resume: Codex records each thread's rollout path as
// `$CODEX_HOME/sessions/…` without resolving the link, so a shared index fills
// with paths into private homes that no longer exist ("no rollout found for
// thread id") — in the developer's own index, too. A fresh index per session is
// right; indexing somebody else's two gigabytes to build it is not. Rein's own
// history holds only Rein's sessions, so the index costs what Rein has run.
type privateHome struct {
	// dir is the run-private CODEX_HOME.
	dir string
	// real is the developer's Codex home: the login comes from here, and
	// nothing else does.
	real string
	// history is Rein's own Codex history; dir/sessions links to its sessions.
	history string
}

// historyDir is where Rein keeps the Codex sessions its own runs create:
// `$REIN_HOME/codex`, beside everything else Rein owns.
func historyDir() (string, error) {
	home, err := config.Dir()
	if err != nil {
		return "", fmt.Errorf("codex: locating the Rein home for Codex history: %w", err)
	}
	return filepath.Join(home, "codex"), nil
}

// realCodexHome is where Codex keeps the developer's configuration and login.
func realCodexHome() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codex: cannot locate the home directory: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

// newPrivateHome builds one. It fails rather than falling back to the real
// home: a session that cannot be isolated must not start, because the failure
// mode is a run acting as the developer.
func newPrivateHome(runID string) (*privateHome, error) { return newHome(runID, true) }

// newHome builds a private home, with Rein's Codex history linked in when
// withHistory is set. A session needs it — for its transcript and for resume.
// A headroom read (headroom.go) does not, and without it Codex has nothing to
// index before it answers initialize.
func newHome(runID string, withHistory bool) (*privateHome, error) {
	real, err := realCodexHome()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(real, "auth.json")); err != nil {
		return nil, fmt.Errorf("%w: no Codex login at %s/auth.json; run `codex login`",
			adapter.ErrPreflight, real)
	}
	links := map[string]string{"auth.json": filepath.Join(real, "auth.json")}
	var history string
	if withHistory {
		history, err = historyDir()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Join(history, "sessions"), 0o700); err != nil {
			return nil, fmt.Errorf("codex: creating Rein's Codex history at %s: %w", history, err)
		}
		links["sessions"] = filepath.Join(history, "sessions")
	}

	dir, err := os.MkdirTemp("", "rein-codex-home-"+sanitizeRunID(runID))
	if err != nil {
		return nil, fmt.Errorf("codex: creating a private CODEX_HOME: %w", err)
	}
	h := &privateHome{dir: dir, real: real, history: history}

	for link, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			h.remove()
			return nil, fmt.Errorf("codex: linking %s into the private home: %w", target, err)
		}
	}
	return h, nil
}

// startupHint is what a start-up that timed out before `initialize` was
// answered should say about the one start-up cost this package decides:
// the size of the history it hands Codex to index.
func (h *privateHome) startupHint() string {
	if h == nil || h.history == "" {
		return ""
	}
	return fmt.Sprintf("codex app-server builds a fresh index over %s before it answers initialize; "+
		"a large history there is the known cause of a slow start (ark:rein#38)", filepath.Join(h.history, "sessions"))
}

// env returns the variables that point Codex at the private home.
func (h *privateHome) env() map[string]string {
	return map[string]string{"CODEX_HOME": h.dir}
}

// transcriptPath rewrites a path Codex reported inside the private home to
// where the file really lives. Codex names the rollout file relative to its
// own `$CODEX_HOME`, and the file itself is in Rein's history through the
// `sessions` symlink — so the path it reports is correct only until this
// directory is removed, and wrong in a way nobody would guess afterwards.
func (h *privateHome) transcriptPath(p string) string {
	if h == nil || p == "" || h.history == "" || !strings.HasPrefix(p, h.dir+string(os.PathSeparator)) {
		return p
	}
	return filepath.Join(h.history, strings.TrimPrefix(p, h.dir+string(os.PathSeparator)))
}

// remove deletes the private home — unless the login stopped being a symlink.
//
// Codex may rotate its own token, and an atomic write (temp file, rename)
// would replace the link with a real file holding a **newer** credential than
// the one in the real home. Deleting that would log the developer out, and
// copying it back would mean this package handling a token, which it must
// never do. So it does neither: it leaves the directory alone and reports
// where it is, which is a mess a person can fix in one `mv`.
func (h *privateHome) remove() {
	if h == nil || h.dir == "" {
		return
	}
	if warning := h.rotatedLogin(); warning != "" {
		return
	}
	_ = os.RemoveAll(h.dir)
}

// rotatedLogin reports a warning when the private home holds a login that is
// no longer a link to the real one, and "" when there is nothing wrong.
func (h *privateHome) rotatedLogin() string {
	if h == nil || h.dir == "" {
		return ""
	}
	link := filepath.Join(h.dir, "auth.json")
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	return fmt.Sprintf("codex rotated its login inside the run-private home; "+
		"%s holds a newer credential than %s/auth.json and was left in place — "+
		"move it across by hand (rein never reads a credential)", link, h.real)
}

// sanitizeRunID keeps a run id usable as part of a directory name, so an
// abandoned private home says which run left it behind.
func sanitizeRunID(runID string) string {
	var b strings.Builder
	for _, r := range runID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
		if b.Len() >= 40 {
			break
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String() + "-"
}
