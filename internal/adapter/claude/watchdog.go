package claude

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The watchdog is a loopback HTTP endpoint that Claude Code's own hooks post
// to, giving Rein an out-of-band signal that does not depend on the agent
// finding time to say anything on stdout.
//
// # Why TCP on every platform, and not a unix socket
//
// A unix socket would need a helper on the far end — `nc -U` is not reliably
// present, and macOS caps sun_path at 104 bytes, which a temp directory can
// blow on its own. Windows needs a loopback listener regardless. One transport
// on all three platforms is less code and fewer ways to be broken on the box
// nobody tested, and `curl` is present on macOS, on modern Windows (curl.exe
// since 1803) and on all but the most minimal Linux images.
//
// The port is kernel-assigned and the URL carries a 128-bit random token that
// the handler checks, so another local process cannot inject fake hook events
// by guessing a port.
//
// # What actually fires, headless
//
// Measured on 2.1.251: under `claude -p` the **Stop** hook fires and the
// **Notification** hook does not, because every notification it reports —
// permission prompts, idle prompts — is an interactive state a `-p` run never
// reaches. Notification is registered anyway: it costs nothing, it is the
// contract's named source of EventIdle, and it starts working the day Rein
// drives an attached or `--bg` session. A watchdog that never hears anything is
// a supported outcome; the stream is the primary signal.

// hookPayload is what a Claude Code hook writes to its stdin, forwarded here
// verbatim by curl.
type hookPayload struct {
	HookEventName        string `json:"hook_event_name"`
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path"`
	CWD                  string `json:"cwd"`
	PermissionMode       string `json:"permission_mode"`
	Message              string `json:"message"`
	NotificationType     string `json:"notification_type"`
	LastAssistantMessage string `json:"last_assistant_message"`
	StopHookActive       bool   `json:"stop_hook_active"`

	// Raw is the payload as received, for Event.Raw.
	Raw json.RawMessage `json:"-"`
}

// maxHookBody bounds a single hook post. `last_assistant_message` can be long;
// a megabyte is generous and still bounded.
const maxHookBody = 1 << 20

type watchdog struct {
	ln        net.Listener
	srv       *http.Server
	url       string
	out       chan<- hookPayload
	closeOnce sync.Once
}

// newWatchdog binds a loopback listener and starts serving. A failure to bind
// is returned rather than fatal: the caller carries on without hooks.
func newWatchdog(out chan<- hookPayload) (*watchdog, error) {
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("claude: generating the watchdog token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("claude: binding the watchdog listener: %w", err)
	}

	w := &watchdog{
		ln:  ln,
		out: out,
		url: fmt.Sprintf("http://%s/%s", ln.Addr().String(), token),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/"+token, w.handle)
	w.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = w.srv.Serve(ln) }()
	return w, nil
}

func (w *watchdog) handle(rw http.ResponseWriter, req *http.Request) {
	defer func() { _ = req.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(req.Body, maxHookBody))
	if err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}
	p.Raw = json.RawMessage(body)

	// Never block the agent: a hook that cannot be delivered is dropped. The
	// hook process is waiting on this response, and Claude Code is waiting on
	// the hook.
	select {
	case w.out <- p:
	default:
	}
	rw.WriteHeader(http.StatusNoContent)
}

// writeSettings writes the `--settings` file registering the hooks, and returns
// its path.
//
// These settings are ADDITIVE — Claude Code still loads the developer's own
// user and project settings, so their hooks keep running alongside these. That
// is deliberate: Rein drives the developer's own Claude Code, not a sanitised
// copy of it.
func (w *watchdog) writeSettings(dir string) (string, error) {
	post := fmt.Sprintf("curl -sS -m 5 -X POST --data-binary @- %s", w.url)
	entry := []map[string]any{{
		"hooks": []map[string]any{{"type": "command", "command": post}},
	}}

	settings := map[string]any{
		"hooks": map[string]any{
			"Notification": entry,
			"Stop":         entry,
		},
	}
	blob, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", fmt.Errorf("claude: encoding the watchdog settings: %w", err)
	}

	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return "", fmt.Errorf("claude: writing the watchdog settings: %w", err)
	}
	return path, nil
}

func (w *watchdog) close() {
	w.closeOnce.Do(func() {
		if w.srv != nil {
			_ = w.srv.Close()
		}
		if w.ln != nil {
			_ = w.ln.Close()
		}
	})
}
