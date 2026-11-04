package runner_test

import (
	"sync"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk/elktest"
	"github.com/elk-work/rein/internal/runner"
)

// Model Elk's releasing-event contract at the HTTP boundary. In particular,
// a retract is not a poll: unread answers must remain available, while a reply
// written after withdrawal is direction and must not reopen the question.
func TestAskWithdrawalTriggersAndAnswerOrder(t *testing.T) {
	for _, trigger := range []string{"output", "attach"} {
		for _, ordering := range []string{"answer-first", "retract-first", "no-question"} {
			if trigger == "output" && ordering == "no-question" {
				continue
			}
			t.Run(trigger+"/"+ordering, func(t *testing.T) {
				h := newHarness(t)
				h.elk.Text("claim_run", order("run-1"))
				var mu sync.Mutex
				pending, unread, direction := false, false, false
				questions, retracts := 0, 0
				h.elk.Handle("report_progress", func(args map[string]any) elktest.Reply {
					mu.Lock()
					defer mu.Unlock()
					if args["kind"] == "question" {
						questions++
						pending = true
						if ordering == "answer-first" {
							pending, unread = false, true
						}
					}
					return elktest.Reply{}
				})
				h.elk.Handle("ask_elk", func(args map[string]any) elktest.Reply {
					mu.Lock()
					defer mu.Unlock()
					if args["run_id"] != "run-1" {
						t.Error("wrong run")
					}
					if args["retract"] == nil || args["question"] != nil {
						t.Error("withdrawal must neither poll nor publish another question")
						unread = false
					}
					retracts++
					pending = false
					if ordering == "retract-first" {
						direction = true
					}
					return elktest.Reply{Text: "No outstanding question."}
				})
				h.agent.Delay = 20 * time.Millisecond
				if ordering != "no-question" {
					h.agent.Script = []adapter.Event{{Kind: adapter.EventQuestion, Text: "Which option?"}}
				}
				// Idle and usage must leave the ask outstanding. For attach, keep the
				// session idle until the control socket itself has withdrawn it.
				for i := 0; i < 20; i++ {
					h.agent.Script = append(h.agent.Script, adapter.Event{Kind: adapter.EventIdle})
				}
				h.agent.Script = append(h.agent.Script, adapter.Event{Kind: adapter.EventUsage})
				if trigger == "output" {
					h.agent.Script = append(h.agent.Script, adapter.Event{Kind: adapter.EventText, Text: "Continuing with the session answer."})
				}
				h.agent.Script = append(h.agent.Script, adapter.Event{Kind: adapter.EventText, Text: "Finished."})
				home, ln := listen(t)
				logs := openRunLogs(t)
				done := make(chan error, 1)
				go func() { done <- h.run(runner.Options{Logs: logs, ControlListener: ln}) }()
				if ordering != "no-question" {
					waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return questions == 1 }, "question publication")
				}
				if trigger == "attach" {
					c, _ := attachToLive(t, home)
					mu.Lock()
					n := retracts
					mu.Unlock()
					if n != 1 {
						t.Errorf("attach returned before withdrawal: %d calls", n)
					}
					if err := c.Input("Use the session answer"); err != nil {
						t.Fatal(err)
					}
					c.Close()
				}
				waitForRun(t, done, h.log)
				mu.Lock()
				defer mu.Unlock()
				if pending || retracts != 1 {
					t.Errorf("pending=%v retracts=%d; want false, 1", pending, retracts)
				}
				if ordering == "answer-first" && !unread {
					t.Error("earlier reply was consumed")
				}
				if ordering == "retract-first" && !direction {
					t.Error("late reply lost")
				}
			})
		}
	}
}
