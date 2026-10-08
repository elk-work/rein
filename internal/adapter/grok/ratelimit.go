package grok

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/adapter"
)

// A Grok subscription that has run out, recognised after the fact
// (ark:rein#40).
//
// Grok exposes no window: no rate-limit event on the ACP stream, no usage
// read, nothing in `_meta`. What it does have is a hook. A turn that ends on an
// API error fires `StopFailure`, and its payload carries `error`, the
// runtime's own classification of the failure — `rate_limit`,
// `authentication_failed`, `invalid_request`, `server_error`,
// `max_output_tokens`, `unknown` (~/.grok/docs/user-guide/10-hooks.md, grok
// 1.0.13). That classification is the signal: the adapter does not have to
// guess from an error string what kind of failure a turn was.
//
// So every session's private GROK_HOME gets one hook of Rein's own, which
// appends the StopFailure payload to a file in that home, and the session
// reads the file when the turn ends. The home is a directory Rein built and
// removes, and its `hooks/` was already a real, empty directory of Rein's
// making (home.go), so this adds a hook to a set that was otherwise empty —
// the developer's own hooks still do not reach the run.
//
// Hooks in `$GROK_HOME/hooks/*.json` are the global scope, which Grok always
// trusts, and the agent-mode documentation says hooks apply there as they do
// anywhere else. The command is POSIX shell, so on Windows no hook is
// installed and the session falls back to recognising a rate limit in the
// error text alone — weaker, and said so in docs/adapter-grok.md.

const (
	// stopFailureHookName is the hook file Rein writes into a private home.
	stopFailureHookName = "rein-stop-failure.json"

	// stopFailureLogName is where that hook appends each payload.
	stopFailureLogName = "rein-stop-failure.jsonl"

	// hookSettle bounds how long a finishing session waits for the StopFailure
	// report. Grok dispatches turn-end hooks off the turn, through one worker,
	// and at teardown waits half a second for them before dropping what is
	// left — so the report can land a moment after the prompt call has
	// already returned. The wait is only paid by a session that has a hook,
	// which a test's in-process peer never does.
	hookSettle = 3 * time.Second
)

// hookEvents are the hook events Rein's hook subscribes to. Only StopFailure
// in production; the integration test adds `Stop`, which fires on every
// completed turn, to prove the hook is discovered and run under ACP without
// needing an API error to happen on cue.
var hookEvents = []string{"StopFailure"}

// stopFailure is the part of a StopFailure payload Rein reads. Reports of any
// other event are decoded with an empty Error and ignored by [rateLimitFrom].
type stopFailure struct {
	Event        string `json:"hook_event_name"`
	Error        string `json:"error"`
	ErrorDetails string `json:"errorDetails"`
}

// hookSupported reports whether this platform gets the StopFailure hook.
func hookSupported() bool { return runtime.GOOS != "windows" }

// installStopFailureHook writes the hook into a private home's `hooks/` and
// returns the path it appends to.
//
// `cat >>` rather than anything cleverer: the payload arrives on stdin as JSON
// and is kept verbatim, and several payloads concatenated are still a stream
// a [json.Decoder] reads one value at a time.
func installStopFailureHook(home string) (string, error) {
	logPath := filepath.Join(home, stopFailureLogName)
	events := map[string]any{}
	for _, ev := range hookEvents {
		events[ev] = []any{map[string]any{
			"hooks": []any{map[string]any{
				"type":    "command",
				"command": "cat >> " + shellQuote(logPath),
			}},
		}}
	}
	hook := map[string]any{"hooks": events}
	b, err := json.MarshalIndent(hook, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(home, "hooks", stopFailureHookName), b, 0o600); err != nil {
		return "", fmt.Errorf("grok: writing Rein's StopFailure hook: %w", err)
	}
	return logPath, nil
}

// shellQuote single-quotes a path for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readStopFailures parses whatever the hook has appended. A missing file is
// the ordinary case — the turn did not fail — and an unreadable tail is
// ignored rather than failing a session over a diagnostic.
func readStopFailures(path string) []stopFailure {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []stopFailure
	dec := json.NewDecoder(f)
	for {
		var sf stopFailure
		// io.EOF is the end; anything else is a payload cut short by a hook
		// killed at teardown. Either way, what decoded before it counts.
		if err := dec.Decode(&sf); err != nil {
			return out
		}
		out = append(out, sf)
	}
}

// rateLimitErrorRE is the fallback when there is no hook report: an error
// text that says, in any of the ways an API says it, that the account is out
// of requests. Deliberately narrow — a false positive parks a queue for a
// day.
var rateLimitErrorRE = regexp.MustCompile(`(?i)rate[ _-]?limit|too many requests|usage limit|\b429\b`)

// rateLimitFrom is the session's verdict at the end of a turn: exhausted when
// the hook classified a failure as `rate_limit`, or — with no hook report at
// all — when the turn's error text says so. Nil when neither did: Grok said
// nothing about the subscription, and nothing is claimed about it.
//
// ExhaustedUntil stays zero. Grok never says when it will serve again, so the
// run loop applies the fallback the person stated (a weekly reset in
// config.toml) or 24 hours.
func rateLimitFrom(fails []stopFailure, errText string, now time.Time) *adapter.RateLimit {
	hit := false
	for _, f := range fails {
		if f.Error == "rate_limit" {
			hit = true
		}
	}
	if !hit && len(fails) == 0 && errText != "" && rateLimitErrorRE.MatchString(errText) {
		hit = true
	}
	if !hit {
		return nil
	}
	return &adapter.RateLimit{
		Status:    "rate_limit",
		Source:    adapter.SourceGrokStopFailure,
		SampledAt: now.UTC(),
		Exhausted: true,
	}
}
