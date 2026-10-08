package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
	keyring "github.com/zalando/go-keyring"
)

func resolveWranglerConnector(ctx context.Context, account string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return keyring.Get("elk-connector-url", account)
}

func validConnectorURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && !strings.ContainsAny(s, "\r\n")
}

func (s *session) redact(text string) string {
	if s.connectorURL == "" {
		return text
	}
	text = strings.ReplaceAll(text, s.connectorURL, "[secret]")
	// JSON may escape ampersands and other characters in a URL.
	blob, _ := json.Marshal(s.connectorURL)
	escaped := string(blob[1 : len(blob)-1])
	text = strings.ReplaceAll(text, escaped, "[secret]")
	return strings.ReplaceAll(text, strings.ReplaceAll(escaped, "/", `\/`), "[secret]")
}

// Scrub every field at the event boundary, including raw payloads and errors.
func (s *session) redactEvent(ev adapter.Event) adapter.Event {
	if s.connectorURL == "" {
		return ev
	}
	blob, err := json.Marshal(ev)
	if err != nil {
		return adapter.Event{Kind: adapter.EventProgress, Text: "Wrangler event could not be safely serialized"}
	}
	var safe adapter.Event
	if json.Unmarshal([]byte(s.redact(string(blob))), &safe) != nil {
		return adapter.Event{Kind: adapter.EventProgress, Text: "Wrangler event could not be safely serialized"}
	}
	if ev.Err != nil {
		safe.Err = ev.Err
		if scrubbed := s.redact(ev.Err.Error()); scrubbed != ev.Err.Error() {
			safe.Err = errors.New(scrubbed)
		}
	}
	return safe
}

// redactError preserves error identity unless it contains the connector.
func (s *session) redactError(err error) error {
	if err == nil {
		return nil
	}
	if safe := s.redact(err.Error()); safe != err.Error() {
		return errors.New(safe)
	}
	return err
}
