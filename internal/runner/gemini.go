package runner

import (
	"errors"
	"strings"

	geminiadapter "github.com/elk-work/rein/internal/adapter/gemini"
	"github.com/elk-work/rein/internal/keyring"
	"github.com/elk-work/rein/internal/secretenv"
)

// storedGeminiKey reads a Google key the person may have stored for Rein
// (`rein secret set GEMINI_API_KEY`, the Elk app's managed secret), for one
// gemini run. It is optional: Gemini CLI runs on the key setup the person
// already has (Casey's ruling, 2026-11-11), and a key stored for Rein is only
// one such setup. A Rein service has no other way to hold one, since the daemon's
// own environment never passes GEMINI_API_KEY down (internal/secretenv), so a
// stored key is handed over when there is one and nothing is asked for when
// there is not.
//
// It is not a queue secret. A queue's secrets map may not name it — it is a
// metered name in internal/secretenv, refused for every queue — and the run
// loop hands it to the gemini adapter alone, in RunSpec.ProviderKey. A read
// that fails for any reason other than "not there" is logged, never printed
// with a value, and the run goes ahead on the program's own setup.
func (qr *queueRunner) storedGeminiKey() string {
	src := qr.r.opts.Secrets
	if src == nil {
		return ""
	}
	v, err := src.ReadItem(geminiadapter.KeyService, geminiadapter.KeyName)
	switch {
	case errors.Is(err, keyring.ErrItemNotFound):
		return ""
	case err != nil:
		qr.logf("gemini: a stored Google key could not be read from this computer's keychain, so the run uses the program's own setup: %v", err)
		return ""
	}
	v = strings.TrimSpace(v)
	if len(v) < secretenv.MinValueLen {
		return ""
	}
	return v
}
