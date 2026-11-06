package gemini

import "testing"

func TestThinkingIsWhatTotalHoldsBeyondPromptAndAnswer(t *testing.T) {
	input := int64(60)
	u := modelStats{TotalTokens: 150, InputTokens: 100, OutputTokens: 30, Cached: 40, Input: &input}.toAdapter("gemini-3.8-flash")
	if u.InputTokens != 60 || u.CacheReadTokens != 40 || u.OutputTokens != 30 || u.ReasoningTokens != 20 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestATotalBelowTheOtherCountsIsNoThinking(t *testing.T) {
	u := modelStats{InputTokens: 100, OutputTokens: 30}.toAdapter("gemini-3.8-flash")
	if u.ReasoningTokens != 0 {
		t.Fatalf("reasoning = %d, want 0 when the stream has no total", u.ReasoningTokens)
	}
}
