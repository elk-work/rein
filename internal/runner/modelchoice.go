package runner

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
)

// Which model and effort a queue's runs use (ark:rein#106).
//
// A person sets them for a queue in Elk, and Elk sends them on every reply to
// `heartbeat_executor` as structured content. **Elk wins; config.toml is the
// fallback; the CLI's own default is the last resort.** Each of the two is
// resolved on its own, so Elk may set the model and leave the effort to the
// config.
//
// Every successful beat replaces what Elk last set with exactly what this beat
// says, so a person who clears the model in Elk gets the config's back on the
// next beat. A failed beat changes nothing: it carries no news, and a network
// blip must not flip the model of the next run. An Elk that sends no
// structured content therefore sets nothing, and the queue behaves exactly as
// it did before Elk could set either.
//
// Elk's values are held in memory and never written into config.toml. That is
// deliberate: the settings path (settings.go) rewrites the file and restarts
// the service, while a model chosen in Elk only has to reach the next run. The
// serve loop beats before it claims, so the first run after a restart already
// sees Elk's choice.

// Where a model or effort came from.
const (
	FromElk        = "Elk"
	FromConfig     = "config.toml"
	FromCLIDefault = "CLI default"
)

// ModelChoice is the model and effort a queue's next run uses, and where each
// came from.
type ModelChoice struct {
	Model, ModelFrom   string
	Effort, EffortFrom string
}

// ResolveModel applies the precedence: a non-empty value Elk set, else the
// queue's config.toml value, else the CLI's default (an empty value, which
// adapters pass as no flag at all). elkModel and elkEffort must already have
// passed the shape check.
func ResolveModel(q config.Queue, elkModel, elkEffort string) ModelChoice {
	var c ModelChoice
	c.Model, c.ModelFrom = pick(elkModel, q.Model)
	c.Effort, c.EffortFrom = pick(elkEffort, q.Effort)
	return c
}

func pick(fromElk, fromConfig string) (string, string) {
	switch {
	case fromElk != "":
		return fromElk, FromElk
	case fromConfig != "":
		return fromConfig, FromConfig
	}
	return "", FromCLIDefault
}

// String is the choice as the log and `rein status` print it:
// "model claude-opus-5-5 (Elk), effort high (config.toml)".
func (c ModelChoice) String() string {
	return "model " + describeChoice(c.Model, c.ModelFrom) + ", effort " + describeChoice(c.Effort, c.EffortFrom)
}

func describeChoice(v, from string) string {
	if v == "" {
		return "the " + from
	}
	return v + " (" + from + ")"
}

// elkModelState is what Elk last set for one queue. Guarded by
// queueRunner.telMu: the serve loop's beat and the telemetry tick's beat both
// write it, and drive.go and the fleet reading read it.
type elkModelState struct {
	model, effort string
	// logged is the effective choice last written to the log, so a change is
	// logged once and a steady state never.
	logged string
	// refused is the last set of refusals logged, so a value Elk keeps
	// sending that Rein will not use is reported once, not every beat.
	refused string
	// saved is whether the state file has been written since start-up: the
	// first beat writes it whatever it says, so a file a previous run left
	// behind cannot outlive this one's first answer.
	saved bool
}

// modelChoice is the model and effort the next run on this queue uses.
func (qr *queueRunner) modelChoice() ModelChoice {
	qr.telMu.Lock()
	defer qr.telMu.Unlock()
	return ResolveModel(qr.q, qr.elkModel.model, qr.elkModel.effort)
}

// logModelChoice writes the effective choice to the log — always at start-up,
// and afterwards only when it changed. It keeps its own record of what it
// last said rather than leaning on the heartbeat reply's dedupe: Elk's text
// need not change when its structured content does.
func (qr *queueRunner) logModelChoice(always bool) {
	qr.telMu.Lock()
	c := ResolveModel(qr.q, qr.elkModel.model, qr.elkModel.effort)
	line := c.String()
	changed := line != qr.elkModel.logged
	qr.elkModel.logged = line
	qr.telMu.Unlock()
	switch {
	case always:
		qr.logf("%s", line)
	case changed:
		qr.logf("model and effort for the next run changed: %s", line)
	}
}

// absorbElkModel takes what one successful beat said about the model and
// effort. A value that fails the same shape check config.toml's would is
// refused and logged, and the queue uses its config value instead.
func (qr *queueRunner) absorbElkModel(hb *elk.Heartbeat) {
	model, effort := deref(hb.Model), deref(hb.Effort)
	var refused []string
	if model != "" && !config.ValidModel(model) {
		refused = append(refused, fmt.Sprintf("model %q is not a model id", model))
		model = ""
	}
	if effort != "" && !config.ValidEffort(effort) {
		refused = append(refused, fmt.Sprintf("effort %q is not one lower-case word such as low, medium or high", effort))
		effort = ""
	}
	for _, u := range hb.Unreadable {
		if strings.HasPrefix(u, "model:") || strings.HasPrefix(u, "effort:") {
			refused = append(refused, "could not read "+u)
		}
	}
	sort.Strings(refused)
	why := strings.Join(refused, "; ")

	qr.telMu.Lock()
	moved := model != qr.elkModel.model || effort != qr.elkModel.effort
	persist := moved || !qr.elkModel.saved
	qr.elkModel.model, qr.elkModel.effort = model, effort
	qr.elkModel.saved = true
	newRefusal := why != qr.elkModel.refused
	qr.elkModel.refused = why
	qr.telMu.Unlock()

	if newRefusal && why != "" {
		qr.logf("not using what Elk set for this queue: %s; config.toml applies instead", why)
	}
	if persist {
		qr.saveElkModel(model, effort)
	}
	qr.logModelChoice(false)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// ElkModelState is what the running service last heard Elk set for a queue,
// as `rein status` reads it from the state directory. Empty fields mean Elk
// set nothing, and config.toml applies.
type ElkModelState struct {
	Model  string    `json:"model,omitempty"`
	Effort string    `json:"effort,omitempty"`
	At     time.Time `json:"at"`
}

// Since says how long the service has held this answer from Elk: the file is
// written when the answer changes, and on the first beat after start-up.
func (s ElkModelState) Since(now time.Time) string {
	if s.At.IsZero() {
		return "—"
	}
	return "since " + ago(now.Sub(s.At))
}

// elkModelFile is where one queue's state lives under a state directory.
func elkModelFile(stateDir, queue string) string {
	return filepath.Join(stateDir, "model", url.PathEscape(queue)+".json")
}

// ReadElkModelState reads one queue's state. It reports false when there is
// none — the queue has never had a beat answered under this Rein home.
func ReadElkModelState(stateDir, queue string) (ElkModelState, bool) {
	var s ElkModelState
	b, err := os.ReadFile(elkModelFile(stateDir, queue))
	if err != nil || json.Unmarshal(b, &s) != nil {
		return ElkModelState{}, false
	}
	return s, true
}

// saveElkModel writes the state for `rein status`. Best effort, like the
// subscription state: a runner that cannot write it still applies Elk's
// choice, and only the status page is the poorer.
func (qr *queueRunner) saveElkModel(model, effort string) {
	dir := qr.r.opts.StateDir
	if dir == "" {
		return
	}
	path := elkModelFile(dir, qr.r.opts.Config.QueueLabel(qr.q))
	b, err := json.MarshalIndent(ElkModelState{Model: model, Effort: effort, At: time.Now().UTC()}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}
