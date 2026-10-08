package claude

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func TestWatchdogReceivesAHookPost(t *testing.T) {
	out := make(chan hookPayload, 4)
	wd, err := newWatchdog(out)
	if err != nil {
		t.Fatalf("newWatchdog: %v", err)
	}
	defer wd.close()

	body := readFile(t, "testdata/stop_hook.json")
	resp, err := http.Post(wd.url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("posting a hook payload: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}

	select {
	case p := <-out:
		if p.HookEventName != "Stop" {
			t.Errorf("hook_event_name = %q, want Stop", p.HookEventName)
		}
		if p.TranscriptPath == "" {
			t.Error("the transcript path was dropped; it is the reason to read this hook at all")
		}
		if p.LastAssistantMessage == "" {
			t.Error("last_assistant_message was dropped")
		}
		if len(p.Raw) == 0 {
			t.Error("the raw payload was not kept")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the watchdog delivered nothing")
	}
}

// TestWatchdogRejectsAGuessedPath closes the one hole a loopback TCP listener
// has that a unix socket does not: another local process posting fake events.
func TestWatchdogRejectsAGuessedPath(t *testing.T) {
	out := make(chan hookPayload, 1)
	wd, err := newWatchdog(out)
	if err != nil {
		t.Fatalf("newWatchdog: %v", err)
	}
	defer wd.close()

	// Same host and port, wrong token.
	base := wd.url[:strings.LastIndex(wd.url, "/")]
	resp, err := http.Post(base+"/0123456789abcdef0123456789abcdef", "application/json",
		strings.NewReader(`{"hook_event_name":"Stop"}`))
	if err != nil {
		t.Fatalf("posting: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unguessed token", resp.StatusCode)
	}
	select {
	case p := <-out:
		t.Fatalf("an untokenised post was delivered: %+v", p)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWatchdogSettingsRegisterBothHooks(t *testing.T) {
	out := make(chan hookPayload, 1)
	wd, err := newWatchdog(out)
	if err != nil {
		t.Fatalf("newWatchdog: %v", err)
	}
	defer wd.close()

	path, err := wd.writeSettings(t.TempDir())
	if err != nil {
		t.Fatalf("writeSettings: %v", err)
	}

	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &settings); err != nil {
		t.Fatalf("the settings file is not valid JSON: %v", err)
	}

	for _, name := range []string{"Notification", "Stop"} {
		entries, ok := settings.Hooks[name]
		if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Fatalf("hook %q is not registered", name)
		}
		h := entries[0].Hooks[0]
		if h.Type != "command" {
			t.Errorf("hook %q type = %q, want command", name, h.Type)
		}
		if !strings.Contains(h.Command, wd.url) {
			t.Errorf("hook %q does not post to the listener: %q", name, h.Command)
		}
	}
}

// TestWatchdogHandlesAFloodWithoutBlocking pins the rule that the watchdog must
// never wedge the agent: Claude Code waits on the hook, and the hook waits on
// this handler.
func TestWatchdogHandlesAFloodWithoutBlocking(t *testing.T) {
	out := make(chan hookPayload) // unbuffered and never read
	wd, err := newWatchdog(out)
	if err != nil {
		t.Fatalf("newWatchdog: %v", err)
	}
	defer wd.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 5 {
			resp, err := http.Post(wd.url, "application/json",
				bytes.NewReader([]byte(`{"hook_event_name":"Notification"}`)))
			if err != nil {
				return
			}
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler blocked on an undeliverable payload")
	}
}

func TestHookEventMapping(t *testing.T) {
	tests := []struct {
		name    string
		payload hookPayload
		want    adapter.EventKind
		emit    bool
	}{{
		name:    "idle prompt is the watchdog's signal",
		payload: hookPayload{HookEventName: "Notification", NotificationType: "idle_prompt"},
		want:    adapter.EventIdle,
		emit:    true,
	}, {
		name:    "needing input is idle too",
		payload: hookPayload{HookEventName: "Notification", NotificationType: "agent_needs_input"},
		want:    adapter.EventIdle,
		emit:    true,
	}, {
		name: "a permission prompt is progress, not a request",
		payload: hookPayload{HookEventName: "Notification", NotificationType: "permission_prompt",
			Message: "Claude needs permission to run git push"},
		want: adapter.EventProgress,
		emit: true,
	}, {
		name:    "an untyped notification is not guessed at",
		payload: hookPayload{HookEventName: "Notification", Message: "something happened"},
		want:    adapter.EventProgress,
		emit:    true,
	}, {
		name:    "an unknown hook is ignored",
		payload: hookPayload{HookEventName: "PreToolUse"},
		emit:    false,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{}
			ev, emit := s.hookEvent(tc.payload)
			if emit != tc.emit {
				t.Fatalf("emit = %v, want %v", emit, tc.emit)
			}
			if emit && ev.Kind != tc.want {
				t.Errorf("kind = %q, want %q", ev.Kind, tc.want)
			}
		})
	}
}

// TestStopHookIsRecordedNotReported pins a deliberate choice: Stop fires
// immediately before the result line, so reporting it as idle would tell the
// watchdog that a finishing session had stalled. What it is for is the
// transcript path, which the stream never carries.
func TestStopHookIsRecordedNotReported(t *testing.T) {
	s := &Session{}
	var p hookPayload
	if err := json.Unmarshal([]byte(readFile(t, "testdata/stop_hook.json")), &p); err != nil {
		t.Fatalf("decoding the recorded Stop payload: %v", err)
	}

	_, emit := s.hookEvent(p)
	if emit {
		t.Error("the Stop hook emitted an event; it should only be recorded")
	}
	if s.transcriptPath != p.TranscriptPath || s.transcriptPath == "" {
		t.Errorf("transcript path = %q, want %q", s.transcriptPath, p.TranscriptPath)
	}
}
