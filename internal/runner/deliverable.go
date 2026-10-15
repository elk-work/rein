package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/worktree"
)

// The deliverable gate (ark:rein#83).
//
// A deliverable is what a person reads in Elk, usually on a phone, and Elk's
// review reads nothing else. Until this gate, Rein submitted whatever the agent
// said last. On 2026-10-14 that was "The pull request's checks are still
// running. I'll merge it once they pass." — and, when the review sent it back,
// two empty messages, after which the run ended stuck with the work merged and
// nobody told what was left (a migration and a deploy).
//
// So before any ready submit Rein reads the agent's account. One that is empty,
// too short to say what was done and what is left, announces work still to
// come, or repeats the deliverable a review just sent back is not submitted:
// the session is resumed once, told plainly to write the deliverable. If that
// still produces nothing usable, Rein writes an honest one from the run's own
// record — pull requests, commits, commands — with the agent's last words kept
// beneath it, so a person is never handed a blank.

// minDeliverableRunes is the shortest account Rein accepts as a deliverable.
// Two sentences — what was done, what is left — plus a link do not fit in
// less.
const minDeliverableRunes = 200

// futureWorkMaxRunes bounds the "announces work still to come" check: a long
// account that mentions what it will do next is a summary with a to-do list,
// not a promise standing in for one.
const futureWorkMaxRunes = 600

// futureWork matches an ending that promises work instead of reporting it.
var futureWork = regexp.MustCompile(`(?i)\bI['’]ll\b|\bI will\b|\bI'm waiting\b|\bwaiting (for|on) (the |its |it\b|them\b|that\b|CI\b|checks?\b)|` +
	`\bstill running\b|\bonce (they|it|the|CI|checks?)\b.{0,40}\b(pass|green|finish|land|merge)`)

// weakDeliverable says why text is not a deliverable, or "" when it is. prev
// is the deliverable a review sent back, or "" outside a revision round.
func weakDeliverable(text, prev string) string {
	t := strings.TrimSpace(text)
	n := utf8.RuneCountInString(t)
	switch {
	case t == "":
		return "your last message was empty"
	case prev != "" && t == strings.TrimSpace(prev):
		return "your last message repeats, word for word, the deliverable Elk's review sent back"
	case n < minDeliverableRunes:
		return fmt.Sprintf("your last message was only %d characters, too short to say what was done and what is left", n)
	case n < futureWorkMaxRunes && futureWork.MatchString(t):
		return "your last message announces work still to come instead of reporting what was done"
	}
	return ""
}

// unusable is the second-chance test: after Rein has asked explicitly, an
// account long enough to be one is taken as the agent wrote it. Only an empty,
// short or repeated one is replaced.
func unusable(text, prev string) bool {
	t := strings.TrimSpace(text)
	return t == "" || utf8.RuneCountInString(t) < minDeliverableRunes ||
		(prev != "" && t == strings.TrimSpace(prev))
}

// deliverableDirective is what a resumed session is told when its account was
// not a deliverable. verdict is Elk's review text in a revision round.
func deliverableDirective(why, verdict string) string {
	var b strings.Builder
	b.WriteString("Rein: this run is not finished until you write its deliverable, and " + why + ".\n\n")
	b.WriteString("Do not start new work and do not wait on anything in the background. Reply now with the " +
		"deliverable itself, as your final message:\n\n" +
		"1. First, in plain English: what you did, and what is left. Name anything you started that has not " +
		"finished — checks still running, a merge, a migration or a deploy still to do — and who or what " +
		"has to do it.\n" +
		"2. Then links: the pull requests, commits and anything else a person needs to check the work.\n\n" +
		"That message is what the person who asked will read, usually on a phone, and it is all Elk's review reads.")
	if strings.TrimSpace(verdict) != "" {
		b.WriteString("\n\nIt must also answer what Elk's review asked for, in its own words:\n\n" + blockquote(verdict) +
			"\n\nYour reply replaces your previous deliverable entirely, so write the whole thing, not only the change.")
	}
	return b.String()
}

// runRecord is what Rein saw the agent do across every session of one run:
// the evidence an honest fallback deliverable is written from.
type runRecord struct {
	commands []string
	prs      []string
}

func (r *runRecord) add(out watchOutcome) {
	r.commands = append(r.commands, out.commands...)
	for _, u := range out.prs {
		if !containsString(r.prs, u) {
			r.prs = append(r.prs, u)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var (
	prURL = regexp.MustCompile(`https://github\.com/[\w.-]+/[\w.-]+/pull/\d+`)
	// `gh pr <verb> 2171 -R elk-work/scout`, or the flag first.
	ghPRNumRepo = regexp.MustCompile(`\bgh pr (?:merge|checks|view|edit|ready|review|comment) (\d+)\b[^\n|;&]*?(?:-R|--repo)[ =]([\w.-]+/[\w.-]+)`)
	ghPRRepoNum = regexp.MustCompile(`\bgh pr (?:merge|checks|view|edit|ready|review|comment)\b[^\n|;&]*?(?:-R|--repo)[ =]([\w.-]+/[\w.-]+)[^\n|;&]*? (\d+)\b`)
)

// prRefs finds pull requests named in s: full URLs, and `gh pr` commands
// that address one by number and repository.
func prRefs(s string) []string {
	var out []string
	add := func(u string) {
		if !containsString(out, u) {
			out = append(out, u)
		}
	}
	for _, u := range prURL.FindAllString(s, -1) {
		add(u)
	}
	for _, m := range ghPRNumRepo.FindAllStringSubmatch(s, -1) {
		add("https://github.com/" + m[2] + "/pull/" + m[1])
	}
	for _, m := range ghPRRepoNum.FindAllStringSubmatch(s, -1) {
		add("https://github.com/" + m[1] + "/pull/" + m[2])
	}
	return out
}

// toolCommand is the shell command a tool call ran, or "" when it ran none.
func toolCommand(t *adapter.ToolUse) string {
	if t == nil || len(t.Input) == 0 {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(t.Input, &in) != nil {
		return ""
	}
	return strings.TrimSpace(in.Command)
}

// secureDeliverable holds a finished session's account to the gate, resuming
// it once when it fails. It returns the session result to submit — the
// resumed one when there was one — with a fallback written in when even that
// produced nothing usable. done means the run has already been reported
// terminally by a session that went wrong, exactly as for runSession.
func (qr *queueRunner) secureDeliverable(ctx context.Context, a adapter.Adapter, spec adapter.RunSpec,
	wo *elk.WorkOrder, wt *worktree.Worktree, repo RepoResolution, res sessionResult,
	verdict, prev string, rec *runRecord, pending *elk.Usage) (sessionResult, error) {

	if res.out.result != nil && !res.out.result.OK() {
		// Submitted as stuck, with its own explanation; not a summary problem.
		return res, nil
	}
	why := weakDeliverable(agentAccount(res.out), prev)
	if why == "" {
		return res, nil
	}
	first := res
	if a.Manifest().Has(adapter.CapResume) && res.sessionID != "" {
		qr.logf("run %s: not submitting yet: %s; asking the agent once for its deliverable", wo.RunID, plainWhy(why))
		qr.log.Runner(runlog.KindNote, "not submitting yet: %s; asking the agent once for its deliverable", plainWhy(why))
		_ = qr.report(ctx, wo.RunID, elk.ProgressStep,
			"The agent stopped without a summary a person could read ("+plainWhy(why)+"). "+
				"Rein asked it once more to write one before handing the run in.", pending)

		next := spec
		next.ResumeID = res.sessionID
		next.Prompt = deliverableDirective(why, verdict)
		again, err := qr.runSession(ctx, a, next, wo, wt, repo, pending)
		if err != nil || again.done {
			return again, err
		}
		rec.add(again.out)
		if again.out.result != nil && !again.out.result.OK() {
			return again, nil
		}
		if !unusable(agentAccount(again.out), prev) {
			qr.log.Runner(runlog.KindNote, "the agent wrote its deliverable when asked")
			return again, nil
		}
		res = again
	}

	// Still nothing a person can read. Rein writes the account itself, from
	// what the run did, and says so.
	qr.logf("run %s: the agent produced no usable deliverable; Rein is writing one from the run's record", wo.RunID)
	qr.log.Runner(runlog.KindNote, "the agent produced no usable deliverable; Rein wrote one from the run's record")
	last := agentAccount(res.out)
	if last == "" {
		last = agentAccount(first.out)
	}
	res.out.deliverable = qr.fallbackDeliverable(ctx, wt, why, last, rec)
	return res, nil
}

// plainWhy turns a reason addressed to the agent into one about it.
func plainWhy(why string) string {
	r := strings.NewReplacer("your last message", "its last message", "you ", "it ")
	return r.Replace(why)
}

// fallbackDeliverable is Rein's own account of a run whose agent would not
// write one. It leads with that fact, then gives what the run's record shows,
// then keeps whatever the agent did say, last, as it said it.
func (qr *queueRunner) fallbackDeliverable(ctx context.Context, wt *worktree.Worktree, why, agentText string,
	rec *runRecord) string {

	var b strings.Builder
	b.WriteString("The agent finished without writing a summary, so Rein wrote this one from the run's own record. ")
	b.WriteString("It shows what was done, not why, and cannot say what is left: check the pull requests below " +
		"before accepting, and look for follow-up steps such as a migration or a deploy.\n\n")
	fmt.Fprintf(&b, "Why: %s, and asking it again did not produce one.\n\n", plainWhy(why))

	prs := append([]string(nil), rec.prs...)
	var st LandingState
	if wt != nil && qr.r.opts.Landing != nil {
		st = qr.r.opts.Landing.Inspect(ctx, wt)
	}
	var lines []string
	for _, pr := range st.PRs {
		lines = append(lines, fmt.Sprintf("- %s — %s", prRef(pr), prState(pr)))
		prs = removeString(prs, pr.URL)
	}
	for _, u := range prs {
		lines = append(lines, "- "+u)
	}
	b.WriteString("**Pull requests**\n\n")
	if len(lines) == 0 {
		b.WriteString("- None found, from the run branch or in the agent's commands.\n")
	} else {
		b.WriteString(strings.Join(lines, "\n") + "\n")
	}
	if wt != nil {
		b.WriteString("\n**Run branch**\n\n")
		fmt.Fprintf(&b, "- `%s`", wt.Branch)
		if st.CommitsKnown {
			fmt.Fprintf(&b, ", %d commit(s) beyond its base", st.Commits)
		}
		if st.PushKnown {
			if st.Pushed {
				b.WriteString(", pushed")
			} else {
				b.WriteString(", not pushed")
			}
		}
		b.WriteString("\n")
	}
	if cmds := lastN(rec.commands, 10); len(cmds) > 0 {
		b.WriteString("\n**The agent's last commands**\n\n")
		for _, c := range cmds {
			fmt.Fprintf(&b, "- `%s`\n", strings.ReplaceAll(truncate(firstLine(c), 160), "`", "'"))
		}
	}
	b.WriteString("\n### What the agent said last\n\n")
	if strings.TrimSpace(agentText) == "" {
		b.WriteString("Nothing.")
	} else {
		b.WriteString(blockquote(truncate(agentText, 4000)))
	}
	return b.String()
}

func removeString(list []string, s string) []string {
	out := list[:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func lastN(list []string, n int) []string {
	if len(list) <= n {
		return list
	}
	return list[len(list)-n:]
}
