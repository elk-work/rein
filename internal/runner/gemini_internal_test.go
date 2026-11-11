package runner

import (
	"context"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/adapter/fake"
	geminiadapter "github.com/elk-work/rein/internal/adapter/gemini"
)

type versionedFake struct {
	*fake.Adapter
	asked int
}

func (v *versionedFake) ProgramVersion(context.Context) string {
	v.asked++
	if geminiadapter.AntigravityOn() {
		return "1.5.5 (Antigravity)"
	}
	return "0.64.0 (Gemini CLI)"
}

// The program version names which Google program runs, so Elk hears the new
// one at the next heartbeat after the switch flips, not up to an hour later.
func TestAGeminiQueuesProgramVersionFollowsTheSwitch(t *testing.T) {
	geminiadapter.ResetAntigravity()
	t.Cleanup(geminiadapter.ResetAntigravity)
	g := &versionedFake{Adapter: fake.New()}
	g.Kind = geminiadapter.Kind
	adapter.Unregister(geminiadapter.Kind)
	if err := adapter.Register(g); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adapter.Unregister(geminiadapter.Kind)
		adapter.MustRegister(geminiadapter.New())
	})
	qr := testQueueRunner(testRunner(fullMachine()))
	qr.q.AgentKind = geminiadapter.Kind

	read := func() string {
		qr.recheckProgramVersion(context.Background())
		qr.telMu.Lock()
		defer qr.telMu.Unlock()
		return qr.tel.programVersion
	}
	if v := read(); v != "0.64.0 (Gemini CLI)" {
		t.Fatalf("first reading %q", v)
	}
	read()
	if g.asked != 1 {
		t.Fatalf("asked %d times within the hour", g.asked)
	}
	geminiadapter.SetAntigravity(qr.workspace+"\x00"+qr.q.Name, true)
	if v := read(); v != "1.5.5 (Antigravity)" {
		t.Fatalf("after the switch turned on: %q", v)
	}
	geminiadapter.SetAntigravity(qr.workspace+"\x00"+qr.q.Name, false)
	if v := read(); v != "0.64.0 (Gemini CLI)" {
		t.Fatalf("after the switch turned off: %q", v)
	}
}
