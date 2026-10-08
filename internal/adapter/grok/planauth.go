package grok

import (
	"strconv"

	"github.com/elk-work/rein/internal/adapter"
)

// Plan login only. A Rein run draws on the developer's SuperGrok plan, the
// same limits the terminal uses. The session's environment is checked at
// Start ([adapter.RunSpec.CheckPlanEnv]); then the agent's `initialize` reply
// must name the plan sign-in as its auth method, or the session stops before
// `session/new`.
//
// `_meta.defaultAuthMethodId` is `cached_token` — "Cached token from
// ~/.grok/auth.json", the grok.com sign-in the private GROK_HOME links in —
// on every plan login measured (grok 1.0.46, 2026-10-07). `grok.com` is the
// other method the agent advertises, the same sign-in not yet cached.

// planAuthMethods are the auth method ids of the grok.com plan sign-in.
var planAuthMethods = map[string]bool{"cached_token": true, "grok.com": true}

func checkPlanAuthMethod(id string) error {
	if planAuthMethods[id] {
		return nil
	}
	got := id
	if got == "" {
		got = "(not reported)"
	}
	return &adapter.PlanAuthError{Because: "the Grok agent reports auth method " +
		strconv.Quote(got) + ", not the grok.com plan sign-in (" + strconv.Quote("cached_token") + ")"}
}
