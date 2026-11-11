package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
)

// The stream-json lines agy 1.5.5 prints with `--output-format stream-json`,
// keyed by "event" (captured 2026-11-10 against a stand-in for the Gemini API;
// testdata/agy-*.jsonl are those captures, paths and ids rewritten):
//
//	{"event":"init","conversation_id":"…","init":{"cwd":"…","tools":[…],"permission_mode":"request-review"}}
//	{"event":"step_update","step_update":{"step_type":"agent_response","state":"DONE","text_delta":"…","usage":{…}}}
//	{"event":"step_update","step_update":{"step_type":"tool","state":"ACTIVE"|"DONE","tool_name":"…","tool_info":{"name":"…","parameters":{…},"output":"…"}}}
//	{"event":"result","result":{"status":"SUCCESS"|"ERROR","response":"…","error":"…","usage":{…},"denied_actions":[{"action":"write_file",…}]}}

type agyLine struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	Init           *struct {
		PermissionMode string `json:"permission_mode"`
	} `json:"init"`
	Step *struct {
		StepIndex int    `json:"step_index"`
		State     string `json:"state"`
		StepType  string `json:"step_type"`
		TextDelta string `json:"text_delta"`
		ToolName  string `json:"tool_name"`
		Model     string `json:"model"`
		ToolInfo  *struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
			Output     string          `json:"output"`
		} `json:"tool_info"`
	} `json:"step_update"`
	Result *struct {
		ConversationID string    `json:"conversation_id"`
		Status         string    `json:"status"`
		Response       string    `json:"response"`
		Error          string    `json:"error"`
		Usage          *agyUsage `json:"usage"`
		DeniedActions  []struct {
			Action      string `json:"action"`
			DisplayName string `json:"display_name"`
		} `json:"denied_actions"`
	} `json:"result"`
}

type agyUsage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ThinkingTokens  int64 `json:"thinking_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
}

// toAdapter maps agy's totals. input_tokens is taken as the fresh prompt and
// cache reads as their own count; the two are reported apart. Thinking is
// billed at the output rate and counted apart from output, as Gemini reports
// it.
func (u agyUsage) toAdapter() adapter.Usage {
	return adapter.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens, ReasoningTokens: u.ThinkingTokens}
}

func decodeAgy(raw []byte) (decoded, error) {
	var l agyLine
	if err := json.Unmarshal(raw, &l); err != nil {
		return decoded{}, err
	}
	rawCopy := json.RawMessage(append([]byte(nil), raw...))
	var d decoded
	switch l.Event {
	case "init":
		if l.ConversationID != "" {
			d.sessionID = agyResumePrefix + l.ConversationID
		}
		d.events = append(d.events, adapter.Event{Kind: adapter.EventProgress, Raw: rawCopy,
			Text: "Antigravity started conversation " + l.ConversationID + " on the Google key"})
	case "step_update":
		st := l.Step
		if st == nil {
			break
		}
		if st.Model != "" {
			d.model = st.Model
		}
		switch {
		case st.StepType == "agent_response" && st.TextDelta != "":
			d.events = append(d.events, adapter.Event{Kind: adapter.EventText, Text: st.TextDelta, Raw: rawCopy})
		case st.StepType == "tool" && st.State == "ACTIVE" && st.ToolInfo != nil:
			d.events = append(d.events, adapter.Event{Kind: adapter.EventToolUse, Raw: rawCopy,
				Tool: &adapter.ToolUse{ID: fmt.Sprint(st.StepIndex), Name: st.ToolInfo.Name, Input: st.ToolInfo.Parameters}})
		case st.StepType == "error_message":
			d.events = append(d.events, adapter.Event{Kind: adapter.EventProgress, Raw: rawCopy,
				Text: "Antigravity reported an error"})
		}
	case "result":
		r := l.Result
		if r == nil {
			return d, fmt.Errorf("a result line with no result")
		}
		if r.ConversationID != "" {
			d.sessionID = agyResumePrefix + r.ConversationID
		}
		tr := &turnResult{ok: r.Status == "SUCCESS"}
		if !tr.ok {
			tr.error = r.Error
			if tr.error == "" {
				tr.error = "status " + r.Status
			}
		}
		// A refused action ends agy's turn with status SUCCESS, an empty
		// response and the action in denied_actions (measured). That is not
		// the work done: report it as a failure that names the action.
		if len(r.DeniedActions) > 0 {
			names := make([]string, 0, len(r.DeniedActions))
			for _, a := range r.DeniedActions {
				names = append(names, a.Action)
			}
			tr.ok = false
			tr.denied = names
			tr.error = "Antigravity refused " + strings.Join(names, ", ") +
				" because it would reach outside what this run may do, and stopped"
		}
		if r.Usage != nil {
			u := r.Usage.toAdapter()
			if u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.ReasoningTokens > 0 {
				tr.usage = append(tr.usage, u)
				uu := u
				d.events = append(d.events, adapter.Event{Kind: adapter.EventUsage, Usage: &uu})
			}
		}
		if tr.ok && strings.TrimSpace(r.Response) != "" {
			tr.response = r.Response
		}
		d.result = tr
	default:
		d.events = append(d.events, adapter.Event{Kind: adapter.EventProgress, Raw: rawCopy, Text: "antigravity: " + l.Event})
	}
	return d, nil
}
