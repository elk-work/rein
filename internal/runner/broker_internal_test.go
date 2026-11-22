package runner

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
	"github.com/elk-work/rein/internal/keyring"
)

type brokerOnly struct{ token string }

func (b brokerOnly) Name() string { return "fake" }
func (b brokerOnly) ReadItem(service, account string) (string, error) {
	return "", fmt.Errorf("%w", keyring.ErrItemNotFound)
}
func (b brokerOnly) BrokerToken(workspace, queue string) (string, error) {
	if b.token == "" {
		return "", fmt.Errorf("%w", keyring.ErrItemNotFound)
	}
	return b.token, nil
}
func (b brokerOnly) SetBrokerToken(workspace, queue, token string) error { return nil }
func (b brokerOnly) DeleteBrokerToken(workspace, queue string) error     { return nil }

// A revision rebuilt after a restart reads the broker token again and redacts
// it, rather than running on a prompt that says the run holds a token it lost.
func TestARestoredRevisionReadsTheBrokerTokenAgain(t *testing.T) {
	const value = "elkbroker_TESTVALUE_restored_0123456789"
	qr := &queueRunner{
		r:         &Runner{opts: Options{Secrets: brokerOnly{token: value}, Out: io.Discard}},
		q:         config.Queue{Name: "mac-claude", AgentKind: "claude"},
		workspace: "Elk Scout",
		host:      NewHostCapabilities("a test", "supabase-vault"),
	}
	c := &reviewCont{wo: &elk.WorkOrder{RunID: "run-1"}, clean: &cleanupStack{}, spec: adapter.RunSpec{}}
	if err := qr.restoreReviewRuntime(c); err != nil {
		t.Fatal(err)
	}
	defer c.clean.run()
	if c.spec.Env[BrokerTokenEnv] != value {
		t.Error("the restored revision has no broker token")
	}
	if got := qr.redactString("token " + value); strings.Contains(got, value) {
		t.Error("the restored revision does not redact the broker token")
	}

	qr.r.opts.Secrets = brokerOnly{}
	c = &reviewCont{wo: &elk.WorkOrder{RunID: "run-1"}, clean: &cleanupStack{},
		spec: adapter.RunSpec{Env: map[string]string{BrokerTokenEnv: value}}}
	if err := qr.restoreReviewRuntime(c); err != nil {
		t.Fatal(err)
	}
	defer c.clean.run()
	if _, ok := c.spec.Env[BrokerTokenEnv]; ok {
		t.Error("a token removed from the keychain survived into the restored revision")
	}
}
