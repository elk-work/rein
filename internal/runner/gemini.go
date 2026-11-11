package runner

import (
	"errors"
	"fmt"
	"strings"

	geminiadapter "github.com/elk-work/rein/internal/adapter/gemini"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/secretenv"
)

// resolveGeminiKey reads the person's own Google key for one gemini run: the
// Elk app's managed secret GEMINI_API_KEY, which `rein secret set
// GEMINI_API_KEY` stores (Casey's ruling, 2026-11-04). It is read through the
// same keychain reader as a scoped queue's secrets, and its value is never in
// an error.
//
// It is not a queue secret. A queue's secrets map may not name it — it is a
// metered name in internal/secretenv, refused for every queue — and the run
// loop hands it to the gemini adapter alone, in RunSpec.ProviderKey.
func (qr *queueRunner) resolveGeminiKey() (string, error) {
	src := qr.r.opts.Secrets
	if src == nil {
		return "", errors.New("this Rein was started with no keychain reader, so the Google key cannot be read")
	}
	v, err := src.ReadItem(geminiadapter.KeyService, geminiadapter.KeyName)
	switch {
	case errors.Is(err, keyring.ErrItemNotFound):
		return "", geminiadapter.ErrNoKey
	case err != nil:
		return "", fmt.Errorf("the Google key could not be read from this computer's keychain: %v", err)
	}
	v = strings.TrimSpace(v)
	if len(v) < secretenv.MinValueLen {
		return "", geminiadapter.ErrNoKey
	}
	return v, nil
}

// absorbAntigravity takes Elk's Antigravity switch from a gemini queue's
// heartbeat reply (internal/adapter/gemini/switch.go, Elk #1755). Only a
// gemini queue's reply counts, so another queue's beat from an Elk that sends
// the field to gemini queues alone cannot flick it off. A reply without the
// field turns it off: the switch fails closed.
func (qr *queueRunner) absorbAntigravity(hb *elk.Heartbeat) {
	if qr.q.AgentKind != geminiadapter.Kind || hb == nil {
		return
	}
	was := geminiadapter.AntigravityOn()
	geminiadapter.SetAntigravity(hb.Antigravity)
	if was != hb.Antigravity {
		if hb.Antigravity {
			qr.logf("gemini: Elk switched Antigravity on; runs use it where it is installed")
		} else {
			qr.logf("gemini: Antigravity is switched off; runs use Gemini CLI")
		}
	}
}
