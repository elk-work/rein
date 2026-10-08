package elk

import (
	"encoding/json"
	"fmt"
	"time"
)

// Fleet telemetry: the two optional objects `heartbeat_executor` carries
// alongside the beat itself.
//
// Elk stores both **verbatim as jsonb** and keeps them 24 hours; it does not
// validate their contents beyond three rules, and those three rules are what
// the types in this file exist to make unbreakable in Go:
//
//   - Each half must be a JSON **object**. An array or a scalar is refused
//     with a message. Both halves here are structs, so that cannot happen.
//   - The two together must serialise to no more than [MaxTelemetryBytes].
//     [Telemetry.Fit] trims to that in a documented order rather than letting
//     the server refuse the reading.
//   - An **empty object counts as no reading and stores nothing**, so there is
//     no point sending one. [Telemetry.put] drops an empty half.
//
// And one rule Elk cannot enforce but the whole design depends on:
//
// **Omit a key you could not measure; never send zero for it.** Elk renders
// "could not measure" and "zero" differently, and they mean opposite things —
// a Windows box has no load average at all, while a load average of 0.0 is a
// completely idle machine. Every measured number below is therefore a POINTER
// with `omitempty`: nil is "no reading", and a pointer to zero is a reading of
// zero, which survives the encoding because a non-nil pointer is never empty.
// The same reasoning makes [SessionReading.MCPServers] a pointer to a slice:
// `[]` is "asked, and there are none" — the true answer for a Rein-driven
// Claude run — and an omitted key is "never asked".
//
// A count that cannot legitimately be zero (a CPU count, a slot ceiling) is a
// plain int, because for those omitempty and "unmeasured" agree.

// MaxTelemetryBytes is Elk's cap on the two halves' combined serialised size.
// Rein measures both encodings and trims rather than sending something the
// server will refuse.
const MaxTelemetryBytes = 16384

// Telemetry is one beat's pair of readings. Either half may be nil.
type Telemetry struct {
	Host    *HostReading
	Session *SessionReading
}

// HostReading describes the machine. Every field is best-effort: a platform
// that cannot measure something leaves it out, which is what lets a Windows
// runner report no load average without claiming a load average of zero.
type HostReading struct {
	// Agent-status contract: labels only, omitted until known. These are
	// independent of the durable host_id and session state.
	MachineName    string `json:"machine_name,omitempty"`
	PreflightState string `json:"preflight_state,omitempty"`

	// OS is the human-readable name and version — "macOS 26.1",
	// "Ubuntu 24.04.1 LTS", "Windows 11 (build 22631)".
	OS string `json:"os,omitempty"`

	// OSKind and Arch are Go's own GOOS and GOARCH. They are the two fields
	// that are always known, because they are compiled in.
	OSKind string `json:"os_kind"`
	Arch   string `json:"arch"`

	CPUModel string `json:"cpu_model,omitempty"`

	// CPUCount is logical CPUs. A plain int: it is never legitimately zero, so
	// omitempty and "not measured" mean the same thing here.
	CPUCount int `json:"cpu_count,omitempty"`

	// Load1 is the one-minute load average. A pointer because 0.0 is a real
	// and useful reading, and because Windows has no such number at all.
	Load1 *float64 `json:"load1,omitempty"`

	MemTotalBytes *uint64 `json:"mem_total_bytes,omitempty"`
	MemFreeBytes  *uint64 `json:"mem_free_bytes,omitempty"`

	// The disk halves describe the filesystem holding the work directory —
	// where worktrees land — not "/". On a Mac with an external scratch disk
	// those are different volumes with different answers.
	DiskTotalBytes *uint64 `json:"disk_total_bytes,omitempty"`
	DiskFreeBytes  *uint64 `json:"disk_free_bytes,omitempty"`

	Runner *RunnerReading `json:"runner,omitempty"`
}

// RunnerReading is Rein's own account of itself: what it is, and how much of
// the machine it is currently allowed to use.
type RunnerReading struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`

	// SlotsTotal is how many runs may be in flight right now — the smaller of
	// the configured ceiling and what the machine can currently afford.
	SlotsTotal int `json:"slots_total"`

	// SlotsUsed is how many are. NO omitempty: zero used is the most common
	// reading there is, and dropping it would make an idle runner look
	// unmeasured.
	SlotsUsed int `json:"slots_used"`

	// SlotsLimitedBy names what held SlotsTotal below the ceiling — "memory",
	// "disk", or "memory and disk". Omitted means nothing did: the runner is
	// at its configured cap. That is a real distinction and not a missing
	// measurement, which is the one place in this file where an omitted key
	// means "no" rather than "unknown".
	SlotsLimitedBy string `json:"slots_limited_by,omitempty"`
}

// SessionReading describes the agent session this queue is driving, or the
// absence of one.
type SessionReading struct {
	// State is the one field that is always present, because "there is no
	// session" is itself the answer. See [runner.SessionState] for the values.
	State string `json:"state"`

	RunID string `json:"run_id,omitempty"`
	Model string `json:"model,omitempty"`

	// Land is this queue's landing policy — "merge", "pr" or "branch"
	// (ark:rein#47) — so the executor row can say, before anything is
	// dispatched to it, whether a run here merges its own work. It is
	// configuration, not a measurement: always known, and on every beat,
	// idle or running. Empty only from a caller that built no queue at all.
	Land string `json:"land,omitempty"`

	// ContextUsedTokens is how full the model's context is: the prompt the
	// most recent request actually carried. ContextWindowTokens is what the
	// model can hold. Both pointers — an adapter that does not report them
	// leaves them out rather than rendering a session as 0% full.
	ContextUsedTokens   *int64 `json:"context_used_tokens,omitempty"`
	ContextWindowTokens *int64 `json:"context_window_tokens,omitempty"`

	// Limits is the subscription windows the vendor volunteered, keyed by its
	// own names — `five_hour`, `seven_day`. Elk renders those two; anything
	// else the vendor reports is stored and ignored, which is better than a
	// curated list that goes stale.
	Limits map[string]RateLimitWindow `json:"limits,omitempty"`

	// RateLimit is the rest of what a rate-limit event says: whether the
	// account is inside its allowance, which window is binding, and the
	// overage state. Kept beside Limits rather than inside it so `limits`
	// stays a clean map of windows.
	//
	// Since ark:rein#40 both are reported on an IDLE beat too: the last
	// reading is carried forward between runs, and `rate_limit.sampled_at`
	// says how old it is.
	RateLimit *RateLimitState `json:"rate_limit,omitempty"`

	// ExhaustedUntil is when this queue will claim again: its subscription
	// has said no, and Rein does not claim on it before this time
	// (ark:rein#40). RFC3339, and absent while the queue has room — so a
	// router reads "no key" as "not held". It rides EVERY beat while it
	// holds, because Elk keeps each beat as its own row and reads the newest.
	ExhaustedUntil string `json:"exhausted_until,omitempty"`

	// ExhaustedReason is one line saying why, and on what basis the time was
	// chosen — the vendor's own reset, the person's stated weekly reset, or a
	// fallback. Present exactly when ExhaustedUntil is.
	ExhaustedReason string `json:"exhausted_reason,omitempty"`

	// MCPServers is the session's MCP inventory. A POINTER to a slice, so
	// that an empty list and a missing measurement stay different claims —
	// see the package note above. For a Rein-driven Claude run the true value
	// is `[]`, because every run is started with `--strict-mcp-config` and an
	// explicit empty server set.
	MCPServers *[]MCPServerReading `json:"mcp_servers,omitempty"`

	// Tokens is what this RUN has spent so far, across every turn, review
	// round and stall-restart it took. Absent until something has been spent.
	Tokens *TokenReading `json:"tokens,omitempty"`

	// StartedAt is when the session began, so a reader can age it without
	// trusting the beat's own arrival time.
	StartedAt string `json:"started_at,omitempty"`
}

// RateLimitWindow is one subscription window.
type RateLimitWindow struct {
	// Utilization is 0..1. NO omitempty: a freshly reset window really is at
	// zero, and that is worth seeing.
	Utilization float64 `json:"utilization"`

	// ResetsAt is RFC3339, or absent when the vendor did not say.
	ResetsAt string `json:"resets_at,omitempty"`

	// WindowMinutes is the window's length where the vendor states one —
	// Codex does. Absent otherwise.
	WindowMinutes int64 `json:"window_minutes,omitempty"`
}

// RateLimitState is the scalar half of a rate-limit event.
type RateLimitState struct {
	Status          string `json:"status,omitempty"`
	Type            string `json:"type,omitempty"`
	ResetsAt        string `json:"resets_at,omitempty"`
	UsingOverage    *bool  `json:"using_overage,omitempty"`
	OverageStatus   string `json:"overage_status,omitempty"`
	OverageResetsAt string `json:"overage_resets_at,omitempty"`

	// Source is where the reading came from: `claude_rate_limit_event`,
	// `codex_account_rate_limits` or `grok_stop_failure`.
	Source string `json:"source,omitempty"`

	// SampledAt is when it was taken. A carried-forward reading can be hours
	// old, and this is how a reader tells.
	SampledAt string `json:"sampled_at,omitempty"`

	// Exhausted is whether the queue is holding off claiming right now. A
	// pointer so that an explicit false — "read, and there is room" — survives
	// the encoding.
	Exhausted *bool `json:"exhausted,omitempty"`
}

// MCPServerReading is one MCP server the session has connected, as the vendor
// named it.
type MCPServerReading struct {
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
}

// TokenReading is a run's cumulative spend. None of the four carries
// omitempty: zero output tokens on a run that has only read files is a real
// measurement, and this whole object is omitted when nothing has been spent.
type TokenReading struct {
	In         int64 `json:"in"`
	Out        int64 `json:"out"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// RFC3339 renders a time for a reading, or "" for the zero time. Every
// timestamp in a reading goes through this, so an unset time is an omitted
// key rather than "0001-01-01T00:00:00Z".
func RFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Empty reports whether a host reading says nothing. GOOS and GOARCH are
// always set, so this is only ever true of a zero value — but the check is
// here so a caller cannot accidentally send the empty object Elk discards.
func (h *HostReading) Empty() bool {
	return h == nil || (h.OSKind == "" && h.Arch == "" && h.OS == "" &&
		h.CPUCount == 0 && h.Load1 == nil && h.MemTotalBytes == nil &&
		h.MemFreeBytes == nil && h.DiskTotalBytes == nil && h.DiskFreeBytes == nil &&
		h.Runner == nil)
}

// Empty reports whether a session reading says nothing. A state is always set
// by [runner], so an empty one means the caller built nothing at all.
func (s *SessionReading) Empty() bool { return s == nil || s.State == "" }

// Size is the two halves' combined serialised size, and the error any half
// that will not encode produced.
func (t Telemetry) Size() (int, error) {
	n := 0
	for _, half := range []any{halfOrNil(t.Host), halfOrNil(t.Session)} {
		if half == nil {
			continue
		}
		blob, err := json.Marshal(half)
		if err != nil {
			return 0, err
		}
		n += len(blob)
	}
	return n, nil
}

// halfOrNil normalises a typed nil pointer to an untyped nil, so the size and
// the put both agree about what "absent" means.
func halfOrNil(v any) any {
	switch h := v.(type) {
	case *HostReading:
		if h.Empty() {
			return nil
		}
	case *SessionReading:
		if h.Empty() {
			return nil
		}
	}
	return v
}

// Fit trims a reading to [MaxTelemetryBytes], in a fixed order, and says what
// it dropped. It edits the halves in place, so build a reading per beat rather
// than keeping one around — which is what [runner] does.
//
// The order is by how much a field can grow and how little it is worth per
// byte. Nothing here is expected to fire — a full reading is a few hundred
// bytes — but the cap is the server's, so a reading that has somehow grown
// past it must lose its bulkiest part rather than be refused whole. A refused
// reading is a hole in the fleet view; a trimmed one is not.
func (t *Telemetry) Fit() (dropped []string, err error) {
	over := func() (bool, error) {
		n, err := t.Size()
		if err != nil {
			return false, err
		}
		return n > MaxTelemetryBytes, nil
	}

	// 1. The MCP inventory: unbounded in principle (a developer's own
	//    ~/.claude.json has run to dozens of servers) and the least load-bearing
	//    thing in the reading.
	// 2. The rest of the session half: bounded, but larger than the host half
	//    and reconstructible from the run's own log.
	// 3. The host half. If this is still over the cap something is very wrong;
	//    dropping it leaves an ordinary beat, which still counts the machine
	//    alive.
	steps := []struct {
		what string
		drop func()
	}{
		{"mcp_servers", func() {
			if t.Session != nil {
				t.Session.MCPServers = nil
			}
		}},
		{"session", func() { t.Session = nil }},
		{"host", func() { t.Host = nil }},
	}

	for _, step := range steps {
		big, err := over()
		if err != nil {
			return dropped, err
		}
		if !big {
			return dropped, nil
		}
		step.drop()
		dropped = append(dropped, step.what)
	}

	big, err := over()
	if err != nil {
		return dropped, err
	}
	if big {
		return dropped, fmt.Errorf("elk: telemetry is over the %d-byte cap with nothing left to drop", MaxTelemetryBytes)
	}
	return dropped, nil
}

// put adds whichever halves are worth sending to the call's arguments. An
// empty half is left out: Elk treats an empty object as no reading at all, so
// sending one is a round trip that stores nothing.
func (t Telemetry) put(args map[string]any) {
	if !t.Host.Empty() {
		args["host"] = t.Host
	}
	if !t.Session.Empty() {
		args["session"] = t.Session
	}
}
