//go:build !windows

package claude

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
)

// These tests are ark:rein#83, the second half of the Oct 14 run: Elk's review
// sent it back, Rein resumed the session, and both resumes submitted an empty
// deliverable. testdata/resume_stopped.jsonl is a real recording (claude
// 2.1.296, 2026-10-15) of a --resume of a session that had left a background
// shell running: Claude Code flushes the stale task_notification, writes a
// result line for a turn with no work in it, and only then runs the prompt.

// TestSessionIgnoresEmptyTurnOnResume is the fix. The stub behaves as claude
// does: the whole recording, then it waits for stdin to close. Before
// ark:rein#83 the session took the empty result, closed stdin, and the run
// submitted nothing.
func TestSessionIgnoresEmptyTurnOnResume(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "resume_stopped.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sess := startBackgroundStub(t, "cat "+fixture+"\ncat > /dev/null\nexit 0", &Adapter{})
	evs := drain(t, sess)

	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !strings.HasPrefix(res.Summary, "The background sleep was stopped") {
		t.Errorf("summary = %q; the session ended on the empty turn before the agent's own", res.Summary)
	}
	if res.Status != adapter.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", res.Status)
	}
	var terminals int
	for _, ev := range evs {
		if ev.Kind.Terminal() {
			terminals++
		}
	}
	if terminals != 1 {
		t.Errorf("terminal events = %d, want 1", terminals)
	}
}

// TestSessionEmptyTurnStandsAlone: if the empty turn is all there is, it is
// still the session's result — the run loop then asks for a summary, rather
// than this adapter inventing a failure.
func TestSessionEmptyTurnStandsAlone(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join("testdata", "resume_stopped.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var head []byte
	for _, l := range bytes.SplitAfter(blob, []byte("\n")) {
		head = append(head, l...)
		if bytes.Contains(l, []byte(`"num_turns":0`)) {
			break
		}
	}
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, head, 0o600); err != nil {
		t.Fatal(err)
	}
	sess := startBackgroundStub(t, "cat "+path+"\nexit 0", &Adapter{})
	drain(t, sess)

	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Status != adapter.StatusSucceeded || res.Summary != "" {
		t.Errorf("result = %q/%q, want the empty turn, succeeded", res.Status, res.Summary)
	}
}

// TestDecoderMarksEmptyTurn pins the classification against the recording: the
// first result is an empty turn, the second is the agent's.
func TestDecoderMarksEmptyTurn(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join("testdata", "resume_stopped.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	d := &decoder{}
	var empties []bool
	for _, l := range bytes.Split(blob, []byte("\n")) {
		if _, res, _ := d.decode(l); res != nil {
			empties = append(empties, d.emptyTurn)
		}
	}
	if len(empties) != 2 || !empties[0] || empties[1] {
		t.Errorf("empty turn at each result = %v, want [true false]", empties)
	}
	for _, name := range []string{"hello.jsonl", "multiturn.jsonl", "background.jsonl", "tools.jsonl"} {
		blob, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		d := &decoder{}
		for _, l := range bytes.Split(blob, []byte("\n")) {
			if _, res, _ := d.decode(l); res != nil && d.emptyTurn {
				t.Errorf("%s: a real turn's result was read as an empty turn", name)
			}
		}
	}
}

// TestHostedSessionIgnoresEmptyTurnOnResume is the same recording on a hosted
// run, which signs in with the workspace's API key and resumes the same way
// when Rein asks for a summary or Elk's review sends the run back. The
// recording's start-up line is rewritten to the API key a hosted session
// reports; nothing else changes. The empty turn must not end it here either.
func TestHostedSessionIgnoresEmptyTurnOnResume(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join("testdata", "resume_stopped.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	blob = bytes.ReplaceAll(blob, []byte(`"apiKeySource":"none"`), []byte(`"apiKeySource":"ANTHROPIC_API_KEY"`))
	path := filepath.Join(t.TempDir(), "resume_hosted.jsonl")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "claude-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ncat "+path+"\ncat > /dev/null\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Binary: stub, DisableWatchdog: true}
	sess, err := a.Start(t.Context(), adapter.RunSpec{
		WorktreeDir:    t.TempDir(),
		Prompt:         "write the deliverable",
		ResumeID:       "1a66706e-1690-436f-bc29-0f8a97675784",
		PermissionMode: adapter.PermissionFull,
		Hosted:         true,
		AgentKind:      "claude",
		Env:            map[string]string{"ANTHROPIC_API_KEY": "hosted-model-test-key"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain(t, sess)

	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !strings.HasPrefix(res.Summary, "The background sleep was stopped") {
		t.Errorf("summary = %q; the hosted session ended on the empty turn before the agent's own", res.Summary)
	}
	if res.Status != adapter.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", res.Status)
	}
}
