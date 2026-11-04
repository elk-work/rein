package runner

import (
	"context"
	"sync"
	"time"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/elk"
)

// sessionAsk serializes publication and withdrawal across the stream and the
// control socket. Elk arbitrates answers racing a withdrawal; we never poll
// here, so we cannot consume a reply intended for the agent.
type sessionAsk struct {
	mu      sync.Mutex
	client  *elk.Client
	runID   string
	pending bool
}

func (a *sessionAsk) retract(ctx context.Context, attached bool) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !attached && !a.pending {
		return nil
	}
	reason := "The session resumed outside Elk."
	if attached {
		reason = "A person attached to the session."
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := a.client.RetractAsk(ctx, a.runID, reason); err != nil {
		return err
	}
	a.pending = false
	return nil
}

// Usage, idle notifications and generic vendor progress are not evidence that
// the agent has resumed: they can be trailing bookkeeping from the question.
func resumesQuestion(ev adapter.Event) bool {
	switch ev.Kind {
	case adapter.EventText:
		return ev.Text != ""
	case adapter.EventToolUse, adapter.EventPermissionRequest, adapter.EventDone:
		return true
	default:
		return false
	}
}
