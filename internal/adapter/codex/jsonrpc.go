package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// A newline-delimited JSON-RPC 2.0 peer, which is what `codex app-server`
// speaks on stdio (`--listen stdio://`, the default).
//
// Three things about the real wire that a textbook implementation gets wrong:
//
//  1. **Codex omits `"jsonrpc"` on the frames it sends.** A decoder that
//     validates the member rejects every message. So this one does not look at
//     it — it is written on the way out, ignored on the way in.
//
//  2. **A server request and a response to our call are told apart by
//     `method`, not by the id.** The server numbers its own requests from 0
//     while we are numbering ours from 1, so ids collide by construction. A
//     frame carrying `method` is a request or a notification; one without is a
//     response, and only then does the id mean *our* id.
//
//  3. **Frames are large.** A `turn/completed` carries the whole turn. This
//     reads with a [json.Decoder] over the stream rather than a
//     [bufio.Scanner], which has a token-size ceiling a long turn will hit.

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("codex: rpc error %d: %s", e.Code, e.Message)
}

// JSON-RPC error codes this client sends when it declines a server request.
const (
	codeMethodNotFound = -32601
	codeRequestFailed  = -32000
)

// wireMessage is every frame in either direction. Which fields are set says
// what it is; see the note above.
type wireMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// isRequest reports whether the frame is a request addressed to us — it names
// a method and carries an id to answer.
func (m *wireMessage) isRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// isNotification reports whether the frame is a one-way notification.
func (m *wireMessage) isNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// handler receives the frames the read loop cannot route itself. Both are
// called on the read goroutine, in order, so an implementation that blocks
// stalls the stream — which is deliberate: back-pressure from a run loop that
// stops reading events must reach the agent rather than being buffered away.
type handler interface {
	onNotification(method string, params json.RawMessage)
	onRequest(id json.RawMessage, method string, params json.RawMessage)
}

type conn struct {
	w   io.WriteCloser
	dec *json.Decoder

	writeMu sync.Mutex
	enc     *json.Encoder

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan wireMessage
	closed  bool
	closErr error
}

func newConn(r io.Reader, w io.WriteCloser) *conn {
	return &conn{
		w:       w,
		dec:     json.NewDecoder(r),
		enc:     json.NewEncoder(w),
		nextID:  1,
		pending: map[int64]chan wireMessage{},
	}
}

// serve reads frames until the stream ends, routing responses to the calls
// waiting on them and everything else to h. It returns the error that ended
// the stream, or nil on a clean EOF.
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

// deliver hands a response to the call waiting on its id. An id nobody is
// waiting for is dropped: it is either a late answer to a call that gave up or
// a frame from a Codex version this build does not know, and neither is worth
// killing a session over.
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

// call sends a request and blocks until the answer arrives, ctx is done, or
// the connection closes. result may be nil to discard the payload.
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
		return fmt.Errorf("codex: %s: %w", method, ctx.Err())
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("codex: %s: %w", method, m.Error)
		}
		if result == nil || len(m.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(m.Result, result); err != nil {
			return fmt.Errorf("codex: %s: decoding result: %w", method, err)
		}
		return nil
	}
}

// notify sends a one-way notification.
func (c *conn) notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(wireMessage{JSONRPC: "2.0", Method: method, Params: raw})
}

// reply answers a server request.
func (c *conn) reply(id json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.write(wireMessage{JSONRPC: "2.0", ID: id, Result: raw})
}

// replyError declines a server request. Declining is a real answer: a server
// request left unanswered wedges the turn, so every request this adapter does
// not implement gets one of these rather than silence.
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
	// json.Encoder terminates every value with a newline, which is exactly the
	// framing.
	return c.enc.Encode(m)
}

func (c *conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// shutdown fails every waiting call. Called when the stream ends, so a caller
// blocked on a request the agent will never answer gets an error instead of
// hanging until its context expires.
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

// close stops the peer's stdin, which is how `codex app-server` is told the
// client is done.
func (c *conn) close() error { return c.w.Close() }

var errClosed = errors.New("codex: app-server connection closed")

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("codex: encoding params: %w", err)
	}
	return raw, nil
}
