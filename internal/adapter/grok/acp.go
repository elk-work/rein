package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// A newline-delimited JSON-RPC 2.0 peer, which is what ACP is: "JSON-RPC over
// stdio", and what `grok agent stdio` speaks. One JSON object per line, in
// both directions.
//
// Grok does send `"jsonrpc": "2.0"` on its frames — unlike Codex, which omits
// it — but this decoder ignores the member anyway. Validating it would buy
// nothing and would make the client fragile against exactly the sort of
// omission the other vendor already ships.
//
// The discriminator is the same one the Codex adapter uses and for the same
// reason: a frame carrying `method` is a request or a notification; one without
// is a response, and only then does its id mean *our* id. ACP allows both sides
// to originate requests, so the id spaces are independent.

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("grok: rpc error %d: %s: %s", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("grok: rpc error %d: %s", e.Code, e.Message)
}

const (
	codeMethodNotFound = -32601
	codeRequestFailed  = -32000
)

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (m *wireMessage) isRequest() bool      { return m.Method != "" && len(m.ID) > 0 }
func (m *wireMessage) isNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// handler takes the frames the read loop cannot route itself. Both run on the
// read goroutine, in order, so an implementation that blocks stalls the
// stream — which is the back-pressure a run loop that stops reading is meant
// to apply to the agent it is watching.
type handler interface {
	onNotification(method string, params json.RawMessage)
	onRequest(id json.RawMessage, method string, params json.RawMessage)
}

type conn struct {
	w io.WriteCloser

	writeMu sync.Mutex
	enc     *json.Encoder

	dec *json.Decoder

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan wireMessage
	closed  bool
	closErr error
}

func newConn(r io.Reader, w io.WriteCloser) *conn {
	return &conn{
		w:       w,
		enc:     json.NewEncoder(w),
		dec:     json.NewDecoder(r),
		nextID:  1,
		pending: map[int64]chan wireMessage{},
	}
}

func (c *conn) serve(h handler) error {
	for {
		var m wireMessage
		if err := c.dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			c.shutdown(err)
			return err
		}
		switch {
		case m.isRequest():
			h.onRequest(m.ID, m.Method, m.Params)
		case m.isNotification():
			h.onNotification(m.Method, m.Params)
		default:
			c.deliver(m)
		}
	}
}

func (c *conn) deliver(m wireMessage) {
	var id int64
	if err := json.Unmarshal(m.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ok {
		ch <- m
	}
}

// call sends a request and blocks for the answer. `session/prompt` is one of
// these and it runs for the whole turn, which is why the read loop must never
// be the thing waiting on it.
func (c *conn) call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	if c.closed {
		err := c.closErr
		c.mu.Unlock()
		if err == nil {
			err = errClosed
		}
		return err
	}
	id := c.nextID
	c.nextID++
	ch := make(chan wireMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	raw, err := marshalParams(params)
	if err != nil {
		c.forget(id)
		return err
	}
	if err := c.write(wireMessage{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", id)),
		Method: method, Params: raw}); err != nil {
		c.forget(id)
		return err
	}

	select {
	case <-ctx.Done():
		c.forget(id)
		return fmt.Errorf("grok: %s: %w", method, ctx.Err())
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("grok: %s: %w", method, m.Error)
		}
		if result == nil || len(m.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(m.Result, result); err != nil {
			return fmt.Errorf("grok: %s: decoding result: %w", method, err)
		}
		return nil
	}
}

func (c *conn) notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(wireMessage{JSONRPC: "2.0", Method: method, Params: raw})
}

func (c *conn) reply(id json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.write(wireMessage{JSONRPC: "2.0", ID: id, Result: raw})
}

// replyError declines an agent request. Declining is a real answer: an
// unanswered request leaves the turn waiting forever.
func (c *conn) replyError(id json.RawMessage, code int, msg string) error {
	return c.write(wireMessage{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (c *conn) write(m wireMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	closed, err := c.closed, c.closErr
	c.mu.Unlock()
	if closed {
		if err == nil {
			err = errClosed
		}
		return err
	}
	return c.enc.Encode(m) // json.Encoder terminates with a newline: the framing
}

func (c *conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *conn) shutdown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closErr = err
	pending := c.pending
	c.pending = map[int64]chan wireMessage{}
	c.mu.Unlock()

	failure := err
	if failure == nil {
		failure = errClosed
	}
	for _, ch := range pending {
		ch <- wireMessage{Error: &rpcError{Code: codeRequestFailed, Message: failure.Error()}}
	}
}

func (c *conn) close() error { return c.w.Close() }

var errClosed = errors.New("grok: ACP connection closed")

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("grok: encoding params: %w", err)
	}
	return raw, nil
}
