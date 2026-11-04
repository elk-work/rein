package gemini

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/elk-work/rein/internal/adapter"
)

// The stream-json lines Gemini CLI 0.64 prints with `--output-format
// stream-json`, one object per line, keyed by "type" (observed 2026-11-04
// against a stand-in for the Gemini API; testdata/*.jsonl are those captures,
// paths and ids rewritten):
//
//	{"type":"init","session_id":"…","model":"gemini-2.5-pro"}
//	{"type":"message","role":"user","content":"…"}
//	{"type":"message","role":"assistant","content":"…","delta":true}
//	{"type":"tool_use","tool_name":"write_file","tool_id":"…","parameters":{…}}
//	{"type":"tool_result","tool_id":"…","status":"success"|"error","output":"…","error":{"type":"…","message":"…"}}
//	{"type":"result","status":"success"|"error","error":{…},"stats":{…,"models":{"<model>":{"input_tokens":…,"output_tokens":…,"cached":…,"input":…}}}}

type line struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"session_id"`
	Model      string          `json:"model"`
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolName   string          `json:"tool_name"`
	ToolID     string          `json:"tool_id"`
	Parameters json.RawMessage `json:"parameters"`
	Status     string          `json:"status"`
	Output     string          `json:"output"`
	Message    string          `json:"message"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Stats *struct {
		Models map[string]modelStats `json:"models"`
	} `json:"stats"`
}

type modelStats struct {
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Cached       int64  `json:"cached"`
	Input        *int64 `json:"input"`
}

// toAdapter splits the prompt into what was read fresh and what came from
// Gemini's cache, as Elk prices them.
func (m modelStats) toAdapter(model string) adapter.Usage {
	fresh := m.InputTokens - m.Cached
	if m.Input != nil {
		fresh = *m.Input
	}
	if fresh < 0 {
		fresh = 0
	}
	return adapter.Usage{InputTokens: fresh, OutputTokens: m.OutputTokens, CacheReadTokens: m.Cached, Model: model}
}

// turnResult is a result line, as the session needs it.
type turnResult struct {
	ok    bool
	error string
	usage []adapter.Usage // one per model
}

// decoded is what one line means to the session.
type decoded struct {
	events    []adapter.Event
	sessionID string
	model     string
	result    *turnResult
}

func decode(raw []byte) (decoded, error) {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return decoded{}, err
	}
	rawCopy := json.RawMessage(append([]byte(nil), raw...))
	var d decoded
	switch l.Type {
	case "init":
		d.sessionID, d.model = l.SessionID, l.Model
		d.events = append(d.events, adapter.Event{Kind: adapter.EventProgress, Raw: rawCopy,
			Text: fmt.Sprintf("Gemini CLI started session %s on %s", l.SessionID, l.Model)})
	case "message":
		if l.Role == "assistant" && l.Content != "" {
			d.events = append(d.events, adapter.Event{Kind: adapter.EventText, Text: l.Content, Raw: rawCopy})
		}
	case "tool_use":
		d.events = append(d.events, adapter.Event{Kind: adapter.EventToolUse, Raw: rawCopy,
			Tool: &adapter.ToolUse{ID: l.ToolID, Name: l.ToolName, Input: l.Parameters}})
	case "tool_result":
		if l.Status == "error" {
			msg := l.Output
			if l.Error != nil && l.Error.Message != "" {
				msg = l.Error.Message
			}
			d.events = append(d.events, adapter.Event{Kind: adapter.EventProgress, Raw: rawCopy,
				Text: "tool " + l.ToolID + " failed: " + firstLine(msg)})
		}
	case "result":
		r := &turnResult{ok: l.Status == "success"}
		if l.Error != nil {
			r.error = l.Error.Message
		}
		if !r.ok && r.error == "" {
			r.error = "status " + l.Status
		}
		if l.Stats != nil {
			names := make([]string, 0, len(l.Stats.Models))
			for name := range l.Stats.Models {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				u := l.Stats.Models[name].toAdapter(name)
				if u.InputTokens+u.OutputTokens+u.CacheReadTokens == 0 {
					continue
				}
				r.usage = append(r.usage, u)
				uu := u
				d.events = append(d.events, adapter.Event{Kind: adapter.EventUsage, Usage: &uu})
			}
		}
		d.result = r
	default:
		text := "gemini: " + l.Type
		if l.Message != "" {
			text += ": " + firstLine(l.Message)
		}
		d.events = append(d.events, adapter.Event{Kind: adapter.EventProgress, Text: text, Raw: rawCopy})
	}
	return d, nil
}
