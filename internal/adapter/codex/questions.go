package codex

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/elk-work/rein/internal/adapter"
)

type userInputQuestion struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	Options  []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

type userInputParams struct {
	Questions []userInputQuestion `json:"questions"`
}

type userInputAnswer struct {
	Answers []string `json:"answers"`
}

type pendingQuestions struct {
	id        json.RawMessage
	questions []userInputQuestion
	answers   map[string]userInputAnswer
	next      int
}

func (s *session) questionsRequested(id, params json.RawMessage) {
	var p userInputParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.declineRequest(id, reqToolUserInput, codeRequestFailed, "rein: invalid question parameters")
		return
	}
	seen := map[string]bool{}
	for _, q := range p.Questions {
		if q.ID == "" || q.Question == "" || seen[q.ID] {
			s.declineRequest(id, reqToolUserInput, codeRequestFailed, "rein: missing or duplicate question identifier or missing question text")
			return
		}
		seen[q.ID] = true
	}
	pending := &pendingQuestions{id: id, questions: p.Questions, answers: map[string]userInputAnswer{}}
	if len(p.Questions) == 0 {
		_ = s.conn.reply(id, struct {
			Answers map[string]userInputAnswer `json:"answers"`
		}{pending.answers})
		return
	}
	s.mu.Lock()
	s.questions = append(s.questions, pending)
	s.mu.Unlock()
	for _, q := range p.Questions {
		var text strings.Builder
		text.WriteString(q.Question)
		for _, option := range q.Options {
			fmt.Fprintf(&text, "\n- %s", option.Label)
			if option.Description != "" {
				fmt.Fprintf(&text, ": %s", option.Description)
			}
		}
		s.emit(adapter.Event{Kind: adapter.EventQuestion, Text: text.String(), Raw: params})
	}
}
