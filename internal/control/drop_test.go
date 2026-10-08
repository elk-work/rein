package control

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// dropHandler counts attachments and releases, and nothing else.
type dropHandler struct {
	mu       sync.Mutex
	released int
	detached int
}

func (h *dropHandler) List(context.Context) ([]RunInfo, error) { return nil, nil }

func (h *dropHandler) Attach(context.Context, string) (Attached, func(), error) {
	var once sync.Once
	return Attached{Run: RunInfo{RunID: "run-1"}}, func() {
		once.Do(func() {
			h.mu.Lock()
			h.released++
			h.mu.Unlock()
		})
	}, nil
}

func (h *dropHandler) Input(context.Context, string, string) error { return nil }

func (h *dropHandler) Respond(context.Context, string, string, bool, string) error { return nil }

func (h *dropHandler) Detach(context.Context, string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.detached++
	return nil
}

func (h *dropHandler) counts() (released, detached int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.released, h.detached
}

// The connection IS the attachment. A client that dies without detaching —
// a killed terminal, a dropped ssh session — must release it anyway, or the
// daemon would stay stepped back with nobody there.
//
// Internal because it has to sever the socket without sending a detach, which
// is exactly what [Client.Close] is careful not to do.
func TestDroppedConnectionReleasesTheAttachment(t *testing.T) {
	home, err := os.MkdirTemp("", "rn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)

	ln, err := Listen(home)
	if err != nil {
		t.Fatal(err)
	}
	h := &dropHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Serve(ctx, ln, h, nil)
	}()
	defer func() {
		cancel()
		<-done
	}()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	c, err := Dial(dialCtx, home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Attach("run-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.conn.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		released, detached := h.counts()
		if released == 1 {
			if detached != 0 {
				t.Errorf("a dropped connection recorded a clean detach: %d", detached)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the attachment was never released (released=%d)", released)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
