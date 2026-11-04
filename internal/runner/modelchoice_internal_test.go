package runner

import (
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/elk"
)

// TestTheModelChangeIsLoggedOnceAndClearingItRestoresConfig walks one queue
// through the beats a person changing the model in Elk produces: set, the
// same again, cleared. Each change is one line; a steady state is none.
func TestTheModelChangeIsLoggedOnceAndClearingItRestoresConfig(t *testing.T) {
	var out strings.Builder
	r := testRunner(fullMachine())
	r.opts.Out = &out
	r.opts.StateDir = t.TempDir()
	qr := testQueueRunner(r)
	qr.q.Model, qr.q.Effort = "claude-sonnet-5-5", "medium"

	qr.logModelChoice(true)
	qr.absorbElkModel(&elk.Heartbeat{}) // an Elk that sets nothing
	if n := strings.Count(out.String(), "changed:"); n != 0 {
		t.Fatalf("a beat that changed nothing logged a change:\n%s", out.String())
	}
	if s, ok := ReadElkModelState(r.opts.StateDir, "mac-claude"); !ok || s.Model != "" || s.Effort != "" {
		t.Errorf("the first beat should write the state, empty: %+v %v", s, ok)
	}

	set := &elk.Heartbeat{Model: ptr("claude-opus-5-5"), Effort: ptr("high")}
	qr.absorbElkModel(set)
	qr.absorbElkModel(set)
	qr.absorbElkModel(set)
	if n := strings.Count(out.String(), "changed: model claude-opus-5-5 (Elk), effort high (Elk)"); n != 1 {
		t.Errorf("the change was logged %d times, want once:\n%s", n, out.String())
	}
	if c := qr.modelChoice(); c.Model != "claude-opus-5-5" || c.Effort != "high" {
		t.Errorf("choice = %+v", c)
	}
	if s, ok := ReadElkModelState(r.opts.StateDir, "mac-claude"); !ok || s.Model != "claude-opus-5-5" || s.Effort != "high" {
		t.Errorf("rein status would not see Elk's choice: %+v %v", s, ok)
	}
	// The fleet reading names the model the next run will use.
	if m := qr.sessionReading().Model; m != "claude-opus-5-5" {
		t.Errorf("the idle reading names model %q", m)
	}

	// Cleared in Elk: config.toml applies again, said once.
	qr.absorbElkModel(&elk.Heartbeat{})
	if !strings.Contains(out.String(), "changed: model claude-sonnet-5-5 (config.toml), effort medium (config.toml)") {
		t.Errorf("clearing Elk's choice did not say the config applies again:\n%s", out.String())
	}
	if c := qr.modelChoice(); c.ModelFrom != FromConfig || c.EffortFrom != FromConfig {
		t.Errorf("choice after clearing = %+v", c)
	}
}

// TestARefusedValueIsLoggedOnce: Elk keeps sending a value Rein will not use
// on every beat, and the log says so once.
func TestARefusedValueIsLoggedOnce(t *testing.T) {
	var out strings.Builder
	r := testRunner(fullMachine())
	r.opts.Out = &out
	qr := testQueueRunner(r)
	qr.q.Effort = "low"

	bad := &elk.Heartbeat{Effort: ptr("Very High"), Unreadable: []string{"model: 5"}}
	for i := 0; i < 3; i++ {
		qr.absorbElkModel(bad)
	}
	if n := strings.Count(out.String(), "not using what Elk set"); n != 1 {
		t.Errorf("the refusal was logged %d times, want once:\n%s", n, out.String())
	}
	for _, want := range []string{`effort "Very High"`, "could not read model: 5"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, out.String())
		}
	}
	if c := qr.modelChoice(); c.Effort != "low" || c.EffortFrom != FromConfig {
		t.Errorf("a refused effort replaced the config's: %+v", c)
	}
}
