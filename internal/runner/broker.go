package runner

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/secretenv"
)

// The vault broker caller token (ark:rein#175).
//
// Elk's `vault-run` reads a secret through Scout's vault broker when the run's
// environment carries a caller token, and otherwise logs "vault broker
// no_connector; fell back to Management API" and reads it with the machine's
// Management API key. Until every workstation read reaches the broker, that
// fallback cannot be switched off (VAULT_BROKER_ALLOW_MANAGEMENT_FALLBACK=0).
//
// An unscoped queue — Elk's own, which keep `supabase-vault` — hands each run
// a caller token of its own: one keychain item per queue, at service
// keyring.BrokerService and account "<workspace>/<queue>", read fresh at the
// start of every run and set as ELK_CONNECTOR_TOKEN, the variable vault-run
// reads first (elk scripts/lib/vault-broker.mjs callerToken).
//
// Per queue, not per run. A per-run token would be minted for each run from
// Elk, but Elk mints run-bound tokens only for its cloud-hosted queues
// (private.mint_run_token, service role only, refusing any other queue), so a
// machine has nothing to mint one with; it would need a new Elk call first.
// The queue's own enrolment token is not a candidate either: it is the one Rein
// claims and reports with, which a session must never hold (ark:rein#112), so
// a broker token equal to it is refused.
//
// It is optional. With no item, or one Rein will not hand over, the run goes
// ahead exactly as before and vault-run falls back as it always has; the run
// log says which. It is a credential, so it joins every redactor of the run
// before the agent can say anything, as a scoped queue's mapped secrets do.

// BrokerTokenEnv is the variable a run's vault-run reads its caller token from.
const BrokerTokenEnv = "ELK_CONNECTOR_TOKEN"

// brokerTokenRE is what Scout's vault broker accepts as a bearer token
// (supabase/functions/vault-broker/handler.ts). Anything else would be refused
// there, so it is refused here, before a run holds it.
var brokerTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)

// CheckBrokerToken reports why a value cannot be a queue's broker caller
// token, or nil when it can. The error never carries the value.
func CheckBrokerToken(v string) error {
	switch {
	case len(v) < secretenv.MinValueLen:
		return errors.New("shorter than " + strconv.Itoa(secretenv.MinValueLen) + " characters, too short to keep out of logs")
	case !brokerTokenRE.MatchString(v):
		return errors.New("not a token the vault broker accepts (letters, digits, - and _ only, at most 512)")
	}
	return nil
}

// brokerToken reads this queue's vault broker caller token for one run. It
// returns "" with a note for the run log when there is none to hand over; the
// note names the item and never a value.
func (qr *queueRunner) brokerToken() (token, note string) {
	if !qr.host.Has("supabase-vault") {
		return "", ""
	}
	item := keyring.BrokerItem(qr.workspace, qr.q.Name)
	src, ok := qr.r.opts.Secrets.(keyring.BrokerTokens)
	if !ok || src == nil {
		return "", "vault broker: this Rein has no keychain reader, so runs carry no caller token and vault-run falls back to the Management API"
	}
	v, err := src.BrokerToken(qr.workspace, qr.q.Name)
	switch {
	case errors.Is(err, keyring.ErrItemNotFound):
		return "", "vault broker: no caller token for this queue (keychain " + item + "); vault-run falls back to the Management API"
	case err != nil:
		return "", "vault broker: the caller token at keychain " + item + " could not be read, so this run carries none: " +
			err.Error()
	}
	v = strings.TrimSpace(v)
	if err := CheckBrokerToken(v); err != nil {
		return "", "vault broker: the value at keychain " + item + " is " + err.Error() + ", so this run carries none"
	}
	if qr.elkToken != "" && v == qr.elkToken {
		return "", "vault broker: the value at keychain " + item + " is this queue's own Elk token, which a run must " +
			"never hold, so this run carries none; store a separate token for the broker"
	}
	return v, "vault broker: " + BrokerTokenEnv + " set for this run from keychain " + item
}

// brokerTokenNote tells a run holding the caller token what it is, by name.
const brokerTokenNote = "Rein set `" + BrokerTokenEnv + "` for this run, so `vault-run` reads through Elk's\n" +
	"vault broker. It is a credential: never print, echo or write out its value,\n" +
	"and use it for nothing but `vault-run`."
