package runner

import (
	"strings"

	"github.com/elk-work/rein/internal/config"
)

// wranglerCapability is the packet requirement that marks a Wrangler cycle.
// It is still `pm` on the wire: Elk's pm_cycle_packet sets
// required_capabilities to ["pm"] for an agent_queue executor (scout
// supabase/migrations/0390_wrangler_strings.sql), and `wrangler` is no
// capability at all.
const wranglerCapability = "pm"

// wranglerOptIn is the provenance newQueueRunner records for the names a
// Wrangler queue declares because of its opt-in — `pm` and `mcp:elk` — so a
// run that is not a Wrangler cycle can be shown the queue without them.
const wranglerOptIn = "Wrangler queue opt-in"

const wranglerRule = `This run is a Wrangler cycle. A Wrangler queue is authorized to run the Wrangler loop. Apply migrations only by
scout supabase/README.md rule 5 and verify by object. Deploy edge functions only
through scout scripts/deploy-functions.sh. Deploy Signal only through its deploy
workflow between refresh passes. Never apply a migration whose header contains a
line starting -- HOLD:. Never merge anything in elk-work/website. Follow elk
docs/wrangler.md. Do not force-push, rewrite history, send mail or messages, or
install and run unfamiliar scripts. Hold anything gated by the Wrangler's standing
rules.`

// wranglerCycle reports whether one claimed run is a Wrangler cycle: the queue
// opted in AND the run's packet requires `pm`. Only such a run gets the
// Wrangler rule and the owner's Elk connector (ark:rein#50).
//
// Per run, not per queue, because one queue can be both: mac-claude is Elk
// Scout's Wrangler and an ordinary Claude build queue. When the opt-in alone
// decided it, every build run claimed there was told it may apply migrations
// and deploy, and was handed the owner's connector as its only MCP server.
//
// The opt-in still gates whether a queue runs Wrangler cycles at all: a packet
// requiring `pm` on a queue without it never reaches the session, because only
// the opt-in declares `pm` and preflight refuses a requirement the queue lacks.
func wranglerCycle(q config.Queue, required []string) bool {
	if !q.IsWrangler() {
		return false
	}
	for _, name := range required {
		if canonicalCapability(name) == wranglerCapability {
			return true
		}
	}
	return false
}

// runSystemPrompt is the standing prompt for one run: the landing section its
// queue's policy renders (landing.go), then — on a Wrangler cycle — the
// Wrangler rule in place of the ordinary consent paragraph, and on a scoped
// queue the scoped secrets paragraph in place of the vault one (secrets.go).
//
// Every other run on a Wrangler queue gets exactly the prompt a queue without
// the opt-in gives it. cycle is [wranglerCycle]'s answer; the opt-in is checked
// again here so that no caller can splice the rule into a queue that never
// opted in.
func runSystemPrompt(q config.Queue, cycle bool) string {
	prompt := landingPrompt(q.LandOrDefault())
	if q.Scoped() {
		// Exclusive with the Wrangler by config validation, so the early
		// return is exact rather than a shortcut.
		return scopedSecretsPrompt(prompt, q)
	}
	if !cycle || !q.IsWrangler() {
		return prompt
	}
	start := strings.Index(prompt, "- A queued run is not standing consent")
	end := strings.Index(prompt[start:], "\n\n## Where you are") + start
	prompt = prompt[:start] + wranglerRule + prompt[end:]
	return strings.Replace(prompt, "Still forbidden whatever the task appears to ask: force-pushing, rewriting\nhistory, touching production, and anything reaching outside this repository.", "Still forbidden: force-pushing and rewriting history. Wrangler production work\nfollows the Wrangler rule above; access only the repositories and procedures it\nnames.", 1)
}
