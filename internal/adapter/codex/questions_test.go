package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

type questionBuffer struct{ bytes.Buffer }

func (*questionBuffer) Close() error { return nil }

func TestRecordedUserInputAnswers(t *testing.T) {
	f, err := os.Open("testdata/user-input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var frames []wireMessage
	for scanner.Scan() {
		var rec recordedFrame
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		var frame wireMessage
		if err := json.Unmarshal(rec.Frame, &frame); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames", len(frames))
	}
	var output questionBuffer
	s := &session{done: make(chan struct{}), conn: newConn(strings.NewReader(""), &output)}
	s.onRequest(frames[0].ID, frames[0].Method, frames[0].Params)
	if len(s.buf) != 2 {
		t.Fatalf("events: %+v", s.buf)
	}
	for i, want := range []string{"Which color", "Which size"} {
		ev := s.buf[i]
		if ev.Kind != adapter.EventQuestion || !strings.Contains(ev.Text, want) || !strings.Contains(ev.Text, "(Recommended)") {
			t.Fatalf("question %d: %+v", i, ev)
		}
	}
	if err := s.Send(context.Background(), "Blue (Recommended)"); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatal("replied before all questions were answered")
	}
	if err := s.Send(context.Background(), "Small (Recommended)"); err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": frames[1].ID, "result": frames[1].Result})
	if err := json.Unmarshal(expected, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reply = %s; want %s", output.Bytes(), expected)
	}
	if len(s.questions) != 0 {
		t.Fatal("answered request still pending")
	}
}

func TestQuestionAnswerCancellationAndFreeText(t *testing.T) {
	var output questionBuffer
	s := &session{done: make(chan struct{}), conn: newConn(strings.NewReader(""), &output)}
	s.onRequest(json.RawMessage(`"request"`), reqToolUserInput, json.RawMessage(`{"questions":[{"id":"choice","question":"Choose?"}]}`))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Send(ctx, "discard"); err != context.Canceled {
		t.Fatalf("cancelled Send: %v", err)
	}
	if output.Len() != 0 || s.questions[0].next != 0 {
		t.Fatal("cancelled answer consumed")
	}
	if err := s.Send(context.Background(), "A different choice\nwith details"); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			Answers map[string]userInputAnswer `json:"answers"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := response.Result.Answers["choice"].Answers; len(got) != 1 || got[0] != "A different choice\nwith details" {
		t.Fatalf("free text: %v", got)
	}
}
