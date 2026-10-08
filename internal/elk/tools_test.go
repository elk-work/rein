package elk_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/elk/elktest"
)

// The work order as Elk renders it (elk-mcp/queue.ts formatClaim). Trimmed to
// the sections Rein parses, with the headings verbatim — the parse is the only
// input to the fail-closed capability check, so the literals are the test.
const workOrder = "Claimed Elk run `run-77` (contract v2).\n" +
	"\n" +
	"**Direction:** Port the due-date sheet to Windows\n" +
	"**Action:** The WinUI composer has no due picker.\n" +
	"**Action id:** `act-42` — owner: Alex · due 2026-09-02\n" +
	"**Requested by:** Sam\n" +
	"**Camp:** Windows parity\n" +
	"\n" +
	"## Handoff packet\n" +
	"\n" +
	"Bring the picker across.\n" +
	"\n" +
	"### Required capabilities — preflight BEFORE starting\n" +
	"Your environment must already provide every capability below (Elk names them, never holds the values):\n" +
	"- git\n" +
	"- file_edit\n" +
	"Fail fast: if ANY capability is missing, do no work — finish immediately with `submit_deliverable` status `stuck`.\n" +
	"\n" +
	"## Reporting contract\n" +
	"- Report progress with `report_progress` (run_id `run-77`) at meaningful steps.\n"

func TestClaimRunParsesTheWorkOrder(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Text("claim_run", workOrder)
	c := newClient(t, s)

	wo, err := c.ClaimRun(context.Background(), elk.ClaimRequest{Workspace: "Elk Scout", Queue: "mac-claude"})
	if err != nil {
		t.Fatal(err)
	}
	if wo.RunID != "run-77" {
		t.Errorf("run id = %q", wo.RunID)
	}
	if wo.ActionID != "act-42" {
		t.Errorf("action id = %q", wo.ActionID)
	}
	if wo.Direction != "Port the due-date sheet to Windows" {
		t.Errorf("direction = %q", wo.Direction)
	}
	if got := strings.Join(wo.RequiredCapabilities, ","); got != "git,file_edit" {
		t.Errorf("required capabilities = %q — this list is what the fail-closed check runs on", got)
	}
	if wo.Text != workOrder {
		t.Error("the work order text must reach the agent verbatim")
	}
	call := s.CallsTo("claim_run")[0]
	if call.Arg("queue") != "mac-claude" || call.Arg("workspace") != "Elk Scout" {
		t.Errorf("args = %#v", call.Args)
	}
}

func TestClaimRunOnAnEmptyQueue(t *testing.T) {
	for _, text := range []string{
		`Queue "mac-claude" is clear: no runs waiting in "Elk Scout".`,
		`Queue is clear: no runs waiting for a connector agent in "Elk Scout".`,
		"Run `run-1` is not claimable (status `running` — already picked up, finished, or cancelled).",
		"Nothing left to claim in group `grp-1` — its runs are already picked up, finished, or cancelled.",
	} {
		t.Run(text[:20], func(t *testing.T) {
			s := elktest.New(t, testToken)
			s.Text("claim_run", text)
			c := newClient(t, s)

			_, err := c.ClaimRun(context.Background(), elk.ClaimRequest{Queue: "mac-claude"})
			if !errors.Is(err, elk.ErrNoWork) {
				t.Fatalf("err = %v, want ErrNoWork — Elk reports these as plain text, not isError", err)
			}
		})
	}
}

func TestReportProgressCancellationIsNotAnIsError(t *testing.T) {
	// The whole point: Elk answers a cancelled run with plain text and no
	// isError, deliberately, so that a heartbeat "must always stay open". A
	// client that only checks isError keeps working on dropped work.
	for _, tc := range []struct {
		name      string
		text      string
		cancelled bool
		status    string
	}{
		{"cancelled", "Not recorded: The user cancelled this run — STOP work on it now.", true, "cancelled"},
		{"terminal", "Not recorded: This run is `done`, not running.", false, "done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := elktest.New(t, testToken)
			s.Text("report_progress", tc.text)
			c := newClient(t, s)

			_, err := c.ReportProgress(context.Background(), elk.Progress{RunID: "run-77", Body: "still going"})
			if !errors.Is(err, elk.ErrCancelled) {
				t.Fatalf("err = %v, want ErrCancelled", err)
			}
			var nr *elk.NotRunningError
			if !errors.As(err, &nr) {
				t.Fatalf("err = %v, want *NotRunningError", err)
			}
			if nr.Cancelled != tc.cancelled {
				t.Errorf("Cancelled = %v, want %v", nr.Cancelled, tc.cancelled)
			}
			if nr.Status != tc.status {
				t.Errorf("Status = %q, want %q", nr.Status, tc.status)
			}
			if nr.RunID != "run-77" {
				t.Errorf("RunID = %q", nr.RunID)
			}
		})
	}
}

func TestReportProgressHappyPath(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Text("report_progress", "Progress recorded on `run-77`. Lease refreshed. Usage recorded.")
	c := newClient(t, s)

	_, err := c.ReportProgress(context.Background(), elk.Progress{
		RunID: "run-77", Body: "wrote the picker", Kind: elk.ProgressTool,
		Usage: &elk.Usage{InputTokens: 1200, OutputTokens: 340, Model: "claude-opus-5", Provider: "anthropic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := s.CallsTo("report_progress")[0].Args
	if args["kind"] != "tool" {
		t.Errorf("kind = %v", args["kind"])
	}
	u, ok := args["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage = %#v", args["usage"])
	}
	// The human half is always sent, and always zero: nobody typed anything
	// at a queue-driven run, and Elk tells a reported zero from an omission.
	for _, k := range []string{"human_prompt_tokens", "human_prompts"} {
		if _, present := u[k]; !present {
			t.Errorf("usage is missing %s", k)
		}
	}
	if u["model"] != "claude-opus-5" || u["provider"] != "anthropic" {
		t.Errorf("usage = %#v", u)
	}
}

func TestUnmeasurableUsageIsOmittedNotSentEmpty(t *testing.T) {
	// Elk drops a malformed usage with a note on an otherwise successful
	// reply. Omitting is honest; sending a nameless zero is a number someone
	// budgets from.
	s := elktest.New(t, testToken)
	s.Text("report_progress", "Progress recorded on `run-77`. Lease refreshed.")
	c := newClient(t, s)

	_, err := c.ReportProgress(context.Background(), elk.Progress{
		RunID: "run-77", Body: "thinking", Usage: &elk.Usage{InputTokens: 5, OutputTokens: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, sent := s.CallsTo("report_progress")[0].Args["usage"]; sent {
		t.Error("a usage with no model or provider was sent anyway")
	}
}

func TestSubmitDeliverableReadsBackTheCoercedStatus(t *testing.T) {
	// `done` becomes `ready` when the caller does not own the List item — and
	// Rein never owns it. A client that assumes its own status is wrong about
	// what Elk recorded.
	s := elktest.New(t, testToken)
	s.Text("submit_deliverable", "Deliverable saved: run `run-77` is `ready`. "+
		"You don't own this List item, so the item's owner will review it in Elk (Accept / Tweak / Re-run).")
	c := newClient(t, s)

	out, err := c.SubmitDeliverable(context.Background(), elk.Submission{
		RunID: "run-77", Deliverable: "# Done\n\nShipped.", Status: elk.StatusDone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != elk.StatusReady {
		t.Errorf("status = %q, want ready — Elk coerced it and the client must say so", out.Status)
	}
}

func TestSubmitDeliverableCancelledAtTheLastMoment(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Text("submit_deliverable", "Not saved: the user cancelled it mid-run. Nothing was persisted.")
	c := newClient(t, s)

	_, err := c.SubmitDeliverable(context.Background(), elk.Submission{
		RunID: "run-77", Deliverable: "x", Status: elk.StatusReady,
	})
	if !errors.Is(err, elk.ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
}

func TestSubmitDeliverableValidatesLocally(t *testing.T) {
	s := elktest.New(t, testToken)
	c := newClient(t, s)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		sub  elk.Submission
	}{
		{"no status", elk.Submission{RunID: "r", Deliverable: "x"}},
		{"cancelled is not an agent's to set", elk.Submission{RunID: "r", Deliverable: "x", Status: "cancelled"}},
		{"http link", elk.Submission{RunID: "r", Deliverable: "x", Status: elk.StatusReady,
			Links: []elk.Link{{Title: "PR", URL: "http://example.test"}}}},
		{"untitled link", elk.Submission{RunID: "r", Deliverable: "x", Status: elk.StatusReady,
			Links: []elk.Link{{URL: "https://example.test"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.SubmitDeliverable(ctx, tc.sub); err == nil {
				t.Error("accepted")
			}
		})
	}
	if n := len(s.Calls()); n != 0 {
		t.Errorf("%d calls reached the server; local validation should have stopped them", n)
	}
}

func TestConnectExecutorOrdinaryPath(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Text("connect_executor", `Created named queue "mac-claude" in workspace "Elk Scout" (executor `+
		"`exec-1111`). Pull its work with `claim_run(queue: \"mac-claude\")`. "+
		"Recorded host `abc`, kind `claude`, 8 declared capabilities.")
	c := newClient(t, s)

	res, err := c.ConnectExecutor(context.Background(), elk.ConnectRequest{
		Workspace: "Elk Scout", DisplayName: "mac-claude", Queue: "mac-claude",
		HostID: "abc", AgentKind: "claude", DeclaredCapabilities: []string{"git", "shell"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Queue != "mac-claude" || res.ExecutorID != "exec-1111" {
		t.Errorf("queue=%q executor=%q", res.Queue, res.ExecutorID)
	}
	if res.Token != "" {
		t.Error("no token is returned on the ordinary path")
	}
	args := s.CallsTo("connect_executor")[0].Args
	for _, k := range []string{"host_id", "agent_kind", "declared_capabilities"} {
		if _, ok := args[k]; !ok {
			t.Errorf("connect_executor did not pass %s", k)
		}
	}
}

func TestConnectExecutorClaimExchangeKeepsTheTokenOutOfTheText(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Text("connect_executor", "Connected invited agent queue \"mac-codex\" (executor `exec-2222`). "+
		"Replace the one-time claim URL programmatically with this durable connector URL: "+
		"https://api.elk.work/functions/v1/elk-mcp/durable-secret-9. Do not print, log, or repeat it. "+
		"Do not call `connect_executor` again. Resume by calling `claim_run(queue: \"mac-codex\")`.")
	c := newClient(t, s)

	res, err := c.ConnectExecutor(context.Background(), elk.ConnectRequest{
		DisplayName: "mac-codex", AgentKind: "codex", ClaimCode: "claim-1", SelfConfigure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Token != "durable-secret-9" {
		t.Errorf("token = %q", res.Token)
	}
	if res.Queue != "mac-codex" {
		t.Errorf("queue = %q — the claim carries its own queue and Elk's answer wins", res.Queue)
	}
	if strings.Contains(res.Text, "durable-secret-9") {
		t.Errorf("the credential is still in the printable text:\n%s", res.Text)
	}
	args := s.CallsTo("connect_executor")[0].Args
	if args["claim_code"] != "claim-1" || args["self_configure"] != true {
		t.Errorf("args = %#v", args)
	}
}

// `shared` (Elk's scout migration 0201) is opt-in and Elk's own default is
// private, so the key must be ABSENT unless it was asked for — an Elk older
// than 0201 has to see no argument it does not recognise.
func TestConnectExecutorSendsSharedOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name    string
		req     elk.ConnectRequest
		wantKey bool
	}{
		{
			name: "unset sends no key at all",
			req: elk.ConnectRequest{
				DisplayName: "mac-claude", Queue: "mac-claude", AgentKind: "claude",
			},
		},
		{
			name: "set sends shared: true",
			req: elk.ConnectRequest{
				DisplayName: "mac-claude", Queue: "mac-claude", AgentKind: "claude", Shared: true,
			},
			wantKey: true,
		},
		{
			// Elk ignores it on a claim exchange and says so in the reply.
			// Rein sends it anyway, so that sentence reaches whoever asked for
			// it rather than the flag disappearing without a word.
			name: "set alongside a claim code is still sent",
			req: elk.ConnectRequest{
				DisplayName: "mac-claude", Queue: "mac-claude", AgentKind: "claude",
				ClaimCode: "claim-1", SelfConfigure: true, Shared: true,
			},
			wantKey: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := elktest.New(t, testToken)
			s.Text("connect_executor", `Created named queue "mac-claude" in workspace "Elk Scout" (executor `+
				"`exec-1111`). Pull its work with `claim_run(queue: \"mac-claude\")`.")
			c := newClient(t, s)

			if _, err := c.ConnectExecutor(context.Background(), tc.req); err != nil {
				t.Fatal(err)
			}
			args := s.CallsTo("connect_executor")[0].Args
			got, ok := args["shared"]
			if ok != tc.wantKey {
				t.Fatalf("shared present = %v, want %v (args = %#v)", ok, tc.wantKey, args)
			}
			if ok && got != true {
				t.Errorf("shared = %#v, want true", got)
			}
		})
	}
}

func TestConnectExecutorRefusesLocallyBeforeSpendingAClaim(t *testing.T) {
	// The claim-code path consumes the invite before it binds anything, so a
	// name one character too long or a misspelled kind must never reach Elk.
	s := elktest.New(t, testToken)
	c := newClient(t, s)
	ctx := context.Background()

	if _, err := c.ConnectExecutor(ctx, elk.ConnectRequest{
		DisplayName: "d", Queue: "mac-claude-13", AgentKind: "claude", ClaimCode: "claim-1",
	}); err == nil || !strings.Contains(err.Error(), "refused, not shortened") {
		t.Errorf("err = %v, want a refusal naming the cap", err)
	}
	if _, err := c.ConnectExecutor(ctx, elk.ConnectRequest{
		DisplayName: "d", AgentKind: "Claude", ClaimCode: "claim-1",
	}); err == nil {
		t.Error("a misspelled agent kind was accepted")
	}
	if n := len(s.Calls()); n != 0 {
		t.Errorf("%d calls reached the server; a spent claim is not recoverable", n)
	}
}

func TestTierRefusalIsTyped(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Reply("connect_executor", elktest.Reply{IsError: true, Text: "Refused connect_executor: it requires " +
		"the interactive connector-token tier, but this token holds run-scoped. Use a suitably scoped " +
		"token or ask the person present to make this call."})
	c := newClient(t, s)

	_, err := c.ConnectExecutor(context.Background(), elk.ConnectRequest{DisplayName: "mac-claude", AgentKind: "claude"})
	var tier *elk.TierRefusalError
	if !errors.As(err, &tier) {
		t.Fatalf("err = %v, want *TierRefusalError", err)
	}
	if tier.Tool != "connect_executor" || tier.Required != "interactive" || tier.Held != "run-scoped" {
		t.Errorf("%+v", tier)
	}
}

func TestHeartbeatCarriesTheQueueDepth(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Text("heartbeat_executor", `Heartbeat recorded for queue "mac-claude". You are online for the next `+
		"15 minutes. 3 runs waiting — claim with `claim_run(queue: \"mac-claude\")`.")
	c := newClient(t, s)

	hb, err := c.HeartbeatExecutor(context.Background(), elk.HeartbeatRequest{
		Workspace: "Elk Scout", Queue: "mac-claude", HostID: "abc", AgentKind: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	if hb.Queue != "mac-claude" || hb.Waiting != 3 {
		t.Errorf("queue=%q waiting=%d", hb.Queue, hb.Waiting)
	}
}

// TestHeartbeatCarriesTheFleetReading checks the two halves reach the call as
// OBJECTS, which is the first of Elk's three rules — an array or a scalar is
// refused with a message.
func TestHeartbeatCarriesTheFleetReading(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Text("heartbeat_executor", `Heartbeat recorded for queue "mac-claude". 0 runs waiting.`)
	c := newClient(t, s)

	if _, err := c.HeartbeatExecutor(context.Background(), elk.HeartbeatRequest{
		Queue: "mac-claude",
		Telemetry: elk.Telemetry{
			Host:    &elk.HostReading{OSKind: "darwin", Arch: "arm64", CPUCount: 12},
			Session: &elk.SessionReading{State: "idle"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	calls := s.CallsTo("heartbeat_executor")
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	host, ok := calls[0].Args["host"].(map[string]any)
	if !ok {
		t.Fatalf("host arrived as %T, not an object", calls[0].Args["host"])
	}
	if host["os_kind"] != "darwin" {
		t.Errorf("host = %v", host)
	}
	session, ok := calls[0].Args["session"].(map[string]any)
	if !ok {
		t.Fatalf("session arrived as %T, not an object", calls[0].Args["session"])
	}
	if session["state"] != "idle" {
		t.Errorf("session = %v", session)
	}
}

// TestHeartbeatWithoutAReadingSendsNeitherHalf: a runner with telemetry
// switched off, or one whose reading is empty, must send an ordinary beat and
// not two empty objects Elk would discard anyway.
func TestHeartbeatWithoutAReadingSendsNeitherHalf(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Text("heartbeat_executor", `Heartbeat recorded for queue "mac-claude". 0 runs waiting.`)
	c := newClient(t, s)

	if _, err := c.HeartbeatExecutor(context.Background(),
		elk.HeartbeatRequest{Queue: "mac-claude"}); err != nil {
		t.Fatal(err)
	}
	args := s.CallsTo("heartbeat_executor")[0].Args
	for _, key := range []string{"host", "session"} {
		if _, ok := args[key]; ok {
			t.Errorf("%q was sent on a beat with no reading: %v", key, args)
		}
	}
}

func TestHeartbeatOnAnElkThatHasNotShippedIt(t *testing.T) {
	s := elktest.New(t, testToken)
	s.Reply("heartbeat_executor", elktest.UnknownTool("heartbeat_executor"))
	c := newClient(t, s)

	_, err := c.HeartbeatExecutor(context.Background(), elk.HeartbeatRequest{Queue: "mac-claude"})
	var unknown *elk.UnknownToolError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want *UnknownToolError so a caller can carry on", err)
	}
}

func TestUsageAdd(t *testing.T) {
	var total elk.Usage
	total.Add(elk.Usage{InputTokens: 10, OutputTokens: 2, Model: "m", Provider: "p"})
	total.Add(elk.Usage{InputTokens: 5, OutputTokens: 3, CacheReadTokens: 7})
	if total.InputTokens != 15 || total.OutputTokens != 5 || total.CacheReadTokens != 7 {
		t.Errorf("%+v", total)
	}
	if total.Model != "m" || total.Provider != "p" {
		t.Errorf("a later delta without a model must not blank the one we know: %+v", total)
	}
	if !total.Valid() {
		t.Error("Valid = false")
	}
}

func TestHeartbeatAgentStatusContract(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprint(present), func(t *testing.T) {
			s := elktest.New(t, testToken)
			s.Text("heartbeat_executor", `Heartbeat recorded for queue "mac-claude". 0 runs waiting.`)
			host := &elk.HostReading{OSKind: "darwin", Arch: "arm64"}
			if present {
				host.MachineName = "Workshop Mac"
				host.PreflightState = "ready"
			}
			_, err := newClient(t, s).HeartbeatExecutor(context.Background(), elk.HeartbeatRequest{Queue: "mac-claude", Telemetry: elk.Telemetry{Host: host}})
			if err != nil {
				t.Fatal(err)
			}
			got := s.CallsTo("heartbeat_executor")[0].Args["host"].(map[string]any)
			for key, want := range map[string]string{"machine_name": "Workshop Mac", "preflight_state": "ready"} {
				value, exists := got[key]
				if exists != present || (present && value != want) {
					t.Errorf("host[%s] = %#v, exists %v", key, value, exists)
				}
			}
		})
	}
}
