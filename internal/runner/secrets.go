package runner

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/runlog"
	"github.com/elk-work/rein/internal/secretenv"
)

// Per-queue scoped secrets, as the run loop applies them (ark:rein#48).
//
// Before this, every queue on a machine reached every credential the machine
// held: the vault broker on PATH (`vault-run`, declared `supabase-vault`) hands
// over the whole vault, and whatever was in the daemon's environment went to
// every agent it started. That is fine while every queue is elk-work's own,
// and wrong the moment one Mac serves a customer workspace too — two
// workspaces' queues would share every secret either one needs.
//
// A queue that declares a secrets map is scoped, and four things change for it
// and for nothing else:
//
//  1. Each run reads the map's keychain items at its start, before a worktree
//     exists, and fails closed — naming the variable and the item, never a
//     value — if any is missing ([queueRunner.resolveSecrets]).
//  2. The values go into RunSpec.Env, the one door into the agent process, and
//     ScopedEnv narrows what that process inherits from the daemon to system
//     variables (internal/secretenv). The queue's allowed MCP servers, which
//     the agent process starts, see the same environment and nothing more;
//     resolveMCP checks them against the map (mcp.go).
//  3. The queue stops declaring what its runs no longer have: the vault, and
//     any credential-shaped capability the map does not supply
//     ([HostCapabilities.scopedTo]). It declares each name in the map instead.
//  4. The system prompt says which variables the run holds, by name, and that
//     nothing else is its to use (prompt.go's scoped paragraph).
//
// Values never leave the process except into the agent's environment. Every
// line Rein writes down — the run log, the daemon log, a progress report, a
// deliverable — passes a redactor built from the run's values, as a backstop
// for the agent echoing one ([secretenv.Redactor]).
//
// It is not a sandbox. An agent with a shell runs as the same OS user as Rein,
// and that user can read the keychain itself; the map scopes what Rein hands a
// run, not what the account can reach. Hard isolation between customers is a
// separate OS user or a separate machine, and the docs say so.

// scopedProvenance labels a capability a scoped queue declares from its map.
const scopedProvenance = "secrets map, this queue"

// scopedTo returns this queue's capability list under a secrets map: without
// the machine-wide vault, without any credential-shaped name the map does not
// supply, and with every name the map does.
func (h *HostCapabilities) scopedTo(q config.Queue) *HostCapabilities {
	out := &HostCapabilities{how: make(map[string]string, len(h.how)+len(q.Secrets))}
	for name, how := range h.how {
		if name == "supabase-vault" || config.LooksLikeEnvName(name) {
			continue
		}
		out.how[name] = how
	}
	for _, ref := range q.SecretRefs() {
		out.add(ref.Env, scopedProvenance)
	}
	return out
}

// resolveSecrets reads a scoped queue's keychain items for one run and returns
// the environment to hand the agent. Every item is tried, so one stuck
// deliverable names every missing one rather than the first.
//
// The error text names variables and items only. It is still passed through a
// redactor built from whatever did resolve, because an [keyring.ItemReader]
// promising not to put a value in an error is a promise, and this is cheap.
func (qr *queueRunner) resolveSecrets() (map[string]string, error) {
	refs := qr.q.SecretRefs()
	if len(refs) == 0 {
		return nil, nil
	}
	src := qr.r.opts.Secrets
	if src == nil {
		return nil, errors.New("this Rein was started with no keychain reader, so a scoped queue's secrets cannot be read")
	}
	env := make(map[string]string, len(refs))
	var problems []string
	for _, ref := range refs {
		v, err := src.ReadItem(ref.Service, ref.Account)
		switch {
		case errors.Is(err, keyring.ErrItemNotFound):
			problems = append(problems, fmt.Sprintf("`%s`: no keychain item at %s", ref.Env, ref.Item()))
		case err != nil:
			problems = append(problems, fmt.Sprintf("`%s`: %s could not be read: %v", ref.Env, ref.Item(), err))
		case strings.TrimSpace(v) == "":
			problems = append(problems, fmt.Sprintf("`%s`: the keychain item at %s is empty", ref.Env, ref.Item()))
		case len(v) < secretenv.MinValueLen:
			problems = append(problems, fmt.Sprintf("`%s`: the keychain item at %s is shorter than %d characters — "+
				"too short to keep out of logs by redaction, so Rein will not hand it to a run. "+
				"Only credentials belong in a secrets map", ref.Env, ref.Item(), secretenv.MinValueLen))
		default:
			env[ref.Env] = v
		}
	}
	if len(problems) > 0 {
		msg := secretenv.NewRedactor(env).String("- " + strings.Join(problems, "\n- "))
		return nil, errors.New(msg)
	}
	return env, nil
}

// secretsHelp is the second half of a stuck deliverable for a missing item:
// how to create one, with the value prompted for so it never reaches a shell
// history, a process list or this deliverable.
func (qr *queueRunner) secretsHelp() string {
	refs := qr.q.SecretRefs()
	example := config.SecretRef{Service: "<service>", Account: config.DefaultSecretAccount}
	if len(refs) > 0 {
		example = refs[0]
	}
	backend := "os"
	if qr.r.opts.Secrets != nil {
		backend = qr.r.opts.Secrets.Name()
	}
	return "Queue `" + qr.q.Name + "` is scoped: its runs get exactly the variables in its `[queues.secrets]` map, " +
		"read from this machine's keychain (backend: " + backend + ") at the start of every run, and nothing else.\n\n" +
		"Create the item as the user Rein runs as. On macOS — `-w` with no value prompts for it:\n\n" +
		"```sh\nsecurity add-generic-password -s " + example.Service + " -a " + example.Account + " -w\n```\n\n" +
		"On Linux: `secret-tool store --label=" + example.Service + " service " + example.Service +
		" username " + example.Account + "`. On Windows: a generic credential with target `" +
		example.Service + ":" + example.Account + "`.\n\n" +
		"The next run reads it; nothing needs restarting. No work was started and no worktree was created."
}

// secretNames is the queue's variable names, for a log line or a prompt.
func secretNames(q config.Queue) []string {
	refs := q.SecretRefs()
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Env
	}
	return out
}

// applySecrets arms every redactor with a scoped run's resolved values, before
// the agent can say anything: the run log's, and the one report, submit and
// the daemon log read. closeLog disarms it when the run ends.
func (qr *queueRunner) applySecrets(env map[string]string) {
	red := secretenv.NewRedactor(env)
	qr.redact.Store(red)
	qr.log.SetRedactor(red)
	names := secretNames(qr.q)
	if qr.r.opts.Hosted {
		names = nil
		for name := range env {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	what := "none — this queue's runs get no credentials from this machine"
	if len(names) > 0 {
		what = strings.Join(names, ", ")
	}
	qr.log.Runner(runlog.KindNote, "scoped secrets: %s", what)
}

// redactString passes s through the current run's redactor, if it has one.
func (qr *queueRunner) redactString(s string) string {
	return qr.redact.Load().String(s)
}

// vaultParagraphStart opens prompt.md's paragraph about machine-held secrets
// — "use the tool on PATH" — which is exactly wrong for a scoped run. It is
// replaced whole, up to the blank line that ends it; a test holds prompt.md
// to still containing it.
const vaultParagraphStart = "Secrets are never in the environment."

// scopedSecretsPrompt swaps the vault paragraph for the scoped one. NAMES
// only: a value never enters a prompt, which a test asserts.
func scopedSecretsPrompt(prompt string, q config.Queue) string {
	return scopedSecretsPromptNames(prompt, secretNames(q))
}

func scopedSecretsPromptNames(prompt string, names []string) string {
	para := scopedSecretsParagraph(names)
	start := strings.Index(prompt, vaultParagraphStart)
	if start < 0 {
		// prompt.md moved on without this file. Still tell the agent: an
		// extra paragraph is better than a run told to use the vault.
		return prompt + "\n\n" + para
	}
	end := strings.Index(prompt[start:], "\n\n")
	if end < 0 {
		return prompt[:start] + para
	}
	return prompt[:start] + para + prompt[start+end:]
}

func scopedSecretsParagraph(names []string) string {
	const rest = " They are the only credentials this run has: do not run `vault-run`, read\n" +
		"the keychain, or look for credentials in other checkouts, in dotfiles or in\n" +
		"other processes. When the work needs a credential that is not listed, name it\n" +
		"and stop."
	if len(names) == 0 {
		return "This queue's credentials are scoped, and it holds none: Rein gave this run\n" +
			"no credentials from this machine." + rest
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + n + "`"
	}
	return "This queue's credentials are scoped. Rein set exactly these environment\n" +
		"variables for this run, from this machine's keychain: " + strings.Join(quoted, ", ") + ".\n" +
		"Use them by name (`$" + names[0] + "` in a shell) and never print, echo or write\n" +
		"out a value — not in a file, a commit, a progress note or the deliverable." + rest
}
