package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
)

// The stream-json shapes agy 1.3.4 prints, one object per line, keyed by
// "event". Google documents the flag but not the shapes, so these are the
// ones observed on 2026-11-04 against a stand-in for the Gemini API
// (testdata/*.jsonl are those captures):
//
//	{"event":"init","conversation_id":"…","init":{"cwd":"…","tools":[…],"permission_mode":"request-review"}}
//	{"event":"step_update","step_update":{"conversation_id":"…","step_index":1,"state":"DONE",
//	    "step_type":"agent_response","text_delta":"…","usage":{…}}}
//	{"event":"step_update","step_update":{…,"state":"ACTIVE","step_type":"tool","tool_name":"write_file",
//	    "tool_info":{"name":"write_file","parameters":{…}}}}
//	{"event":"step_update","step_update":{…,"state":"ERROR","step_type":"tool",…,
//	    "tool_info":{…,"error":{"type":"TOOL_ERROR","message":"…"}}}}
//	{"event":"result","result":{"conversation_id":"…","status":"SUCCESS"|"ERROR","response":"…",
//	    "error":"…","usage":{…},"denied_actions":[{"action":"write_file",…}],"finished_reason":"…"}}
//
// Step usage is per step; the result's usage is the turn's total.

type line struct {
	Event          string          `json:"event"`
	ConversationID string          `json:"conversation_id"`
	Init           *initInfo       `json:"init"`
	StepUpdate     *stepUpdate     `json:"step_update"`
	Result         *resultInfo     `json:"result"`
	Raw            json.RawMessage `json:"-"`
}

type initInfo struct {
	Cwd            string   `json:"cwd"`
	Tools          []string `json:"tools"`
	PermissionMode string   `json:"permission_mode"`
}

type stepUpdate struct {
	ConversationID string     `json:"conversation_id"`
	StepIndex      int        `json:"step_index"`
	State          string     `json:"state"`
	StepType       string     `json:"step_type"`
	TextDelta      string     `json:"text_delta"`
	ToolName       string     `json:"tool_name"`
	ToolInfo       *toolInfo  `json:"tool_info"`
	Usage          *usageInfo `json:"usage"`
}

type toolInfo struct {
	Name       string          `json:"name"`
	Parameters json.RawMessage `json:"parameters"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type usageInfo struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ThinkingTokens  int64 `json:"thinking_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
}

// toAdapter counts thinking as output, which is how Google bills it.
func (u usageInfo) toAdapter(model string) adapter.Usage {
	return adapter.Usage{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens + u.ThinkingTokens,
		CacheReadTokens: u.CacheReadTokens,
		Model:           model,
	}
}

func (u usageInfo) zero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.ThinkingTokens == 0 && u.CacheReadTokens == 0
}

type resultInfo struct {
	ConversationID string     `json:"conversation_id"`
	Status         string     `json:"status"`
	Response       string     `json:"response"`
	Error          string     `json:"error"`
	NumTurns       int        `json:"num_turns"`
	Usage          *usageInfo `json:"usage"`
	FinishedReason string     `json:"finished_reason"`
	DeniedActions  []struct {
		Action      string `json:"action"`
		DisplayName string `json:"display_name"`
	} `json:"denied_actions"`
}

// decoded is what one line means to the session.
type decoded struct {
	events         []adapter.Event
	conversationID string
	result         *resultInfo
}

// decode turns one stdout line into events. model labels usage; agy does not
// say which model answered.
func decode(raw []byte, model string) (decoded, error) {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return decoded{}, err
	}
	rawCopy := json.RawMessage(append([]byte(nil), raw...))
	var d decoded
	switch l.Event {
	case "init":
		d.conversationID = l.ConversationID
		text := "Antigravity CLI started conversation " + l.ConversationID
		if l.Init != nil && l.Init.PermissionMode != "" {
			text += " (permission mode " + l.Init.PermissionMode + ")"
		}
		d.events = append(d.events, adapter.Event{Kind: adapter.EventProgress, Text: text, Raw: rawCopy})
	case "step_update":
		if l.StepUpdate == nil {
			break
		}
		d.conversationID = l.StepUpdate.ConversationID
		d.events = append(d.events, stepEvents(l.StepUpdate, rawCopy, model)...)
	case "result":
		if l.Result == nil {
			return d, fmt.Errorf("a result line without a result")
		}
		d.conversationID = l.Result.ConversationID
		d.result = l.Result
		if len(l.Result.DeniedActions) > 0 {
			names := make([]string, 0, len(l.Result.DeniedActions))
			for _, a := range l.Result.DeniedActions {
				names = append(names, a.Action)
			}
			d.events = append(d.events, adapter.Event{Kind: adapter.EventProgress, Raw: rawCopy,
				Text: "Antigravity CLI refused actions this run's permission mode does not allow: " + strings.Join(names, ", ")})
		}
	default:
		d.events = append(d.events, adapter.Event{Kind: adapter.EventProgress, Text: "agy: " + l.Event, Raw: rawCopy})
	}
	return d, nil
}

func stepEvents(s *stepUpdate, raw json.RawMessage, model string) []adapter.Event {
	var evs []adapter.Event
	switch s.StepType {
	case "user_input":
		// Our own prompt, echoed back.
		return nil
	case "agent_response":
		if s.TextDelta != "" {
			evs = append(evs, adapter.Event{Kind: adapter.EventText, Text: s.TextDelta, Raw: raw})
		}
	case "tool":
		name := s.ToolName
		if name == "" && s.ToolInfo != nil {
			name = s.ToolInfo.Name
		}
		switch s.State {
		case "ACTIVE":
			tu := &adapter.ToolUse{ID: fmt.Sprintf("%s:%d", s.ConversationID, s.StepIndex), Name: name}
			if s.ToolInfo != nil {
				tu.Input = s.ToolInfo.Parameters
			}
			evs = append(evs, adapter.Event{Kind: adapter.EventToolUse, Tool: tu, Raw: raw})
		case "ERROR":
			msg := "failed"
			if s.ToolInfo != nil && s.ToolInfo.Error != nil && s.ToolInfo.Error.Message != "" {
				msg = s.ToolInfo.Error.Message
			}
			evs = append(evs, adapter.Event{Kind: adapter.EventProgress, Text: "tool " + name + ": " + msg, Raw: raw})
		}
	case "error_message":
		evs = append(evs, adapter.Event{Kind: adapter.EventProgress, Text: "Antigravity CLI reported an error", Raw: raw})
	default:
		if s.StepType != "" {
			evs = append(evs, adapter.Event{Kind: adapter.EventProgress, Text: "agy step: " + s.StepType, Raw: raw})
		}
	}
	// Usage arrives on finished steps; each is an increment.
	if s.State == "DONE" && s.Usage != nil && !s.Usage.zero() {
		u := s.Usage.toAdapter(model)
		evs = append(evs, adapter.Event{Kind: adapter.EventUsage, Usage: &u})
	}
	return evs
}
