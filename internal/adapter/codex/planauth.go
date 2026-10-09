package codex

import (
	"strconv"

	"github.com/elk-work/rein/internal/adapter"
)

// Plan login only. A Rein run draws on the developer's ChatGPT plan, the same
// limits the terminal uses. The session's environment is checked at Start
// ([adapter.RunSpec.CheckPlanEnv]); then, before any thread starts, the app
// server is asked which login it is on (`account/read`) and anything but the
// plan stops the session. The private CODEX_HOME links in only auth.json, so
// an API key in that file is the remaining way in, and this catches it.

// planAccountType is `account.type` on a ChatGPT sign-in.
const planAccountType = "chatgpt"

func checkPlanAccount(r accountReadResponse, api ...bool) error {
	if len(api) > 0 && api[0] {
		if r.Account != nil && r.Account.Type == "apiKey" {
			return nil
		}
		return &adapter.APIAuthError{Because: "Codex did not report an apiKey account"}
	}

	got := "(no account)"
	if r.Account != nil {
		if r.Account.Type == planAccountType {
			return nil
		}
		got = r.Account.Type
	}
	return &adapter.PlanAuthError{Because: "the Codex app server reports account type " +
		strconv.Quote(got) + ", not " + strconv.Quote(planAccountType) + " (the ChatGPT plan login)"}
}
