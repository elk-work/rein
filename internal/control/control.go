// Package control is the local control plane a running `rein run` exposes so
// that `rein attach` can take one of its live sessions over.
//
// # Why a socket rather than a resume
//
// ark:rein#26 left the choice open between two ways of handing a live session
// to a person: (a) the daemon brokers it over a local socket, or (b) the
// session is respawned with `--resume` in the person's own terminal while the
// daemon pauses. (b) is cheaper to build and is what the `resume` capability
// exists for — but it is not a takeover. It is a second session against the
// same id, started from whatever the vendor persisted, while the original
// process is still running and still holds the worktree; the two would write
// over each other, and a `--read-only` follow would be impossible because
// there is nothing to follow.
//
// So: (a). The daemon keeps the process and stays the one thing driving it,
// and this socket carries three questions — which runs are live, deliver this
// text to one, answer this permission request — plus the fact of a person
// being attached, which is what makes the daemon step back.
//
// # Not a PTY
//
// The session is a pipe in `-p`/stream-json mode, not a terminal, and every
// adapter that takes input at all takes it as structured text through
// [adapter.Session.Send]. So attach sends *lines*, not keystrokes: no raw
// mode, no window-size propagation, no terminal to restore on a panic, and no
// ConPTY. The person's own shell does the line editing.
//
// The stream a person reads comes from the run log rather than from this
// socket — `rein attach` is `rein tail <run>` plus a control connection — so
// there is exactly one renderer, and following a run is identical whether or
// not anybody is driving it.
//
// # The connection is the attachment
//
// One connection, one attachment. When the connection closes — a clean
// `detach`, a Ctrl-C, an `ssh` session dropped — the runner resumes on its
// own, because there is no state to reconcile beyond the socket being open.
// That is the whole reason the lifetime is the connection's rather than a
// flag somebody has to remember to clear.
package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// ErrInUse reports that another process already holds the control endpoint.
var ErrInUse = errors.New("control: another runner is already listening")

// ErrNoRunner reports that nothing is listening — no `rein run` on this
// machine, or one too old to serve the control plane.
var ErrNoRunner = errors.New("control: no runner is listening")

// ErrEndpoint reports that the endpoint itself cannot be used, whatever is or
// is not listening on it. It is kept apart from [ErrNoRunner] because the two
// send a person to different places: one means "start a runner", the other
// means "this machine cannot have an endpoint here at all".
var ErrEndpoint = errors.New("control: the endpoint cannot be used")

// Op is a request's operation.
type Op string

const (
	// OpList asks what is running. It needs no attachment and changes
	// nothing, so `rein attach` uses it to say what a bad reference could
	// have meant.
	OpList Op = "list"

	// OpAttach claims the person's attachment to one run. Exactly one per
	// connection: the connection IS the attachment.
	OpAttach Op = "attach"

	// OpInput delivers text to the attached session.
	OpInput Op = "input"

	// OpPermission answers a permission request the session raised.
	OpPermission Op = "permission"

	// OpDetach releases the attachment early. Closing the connection does the
	// same thing; this exists so a clean detach is distinguishable from a
	// dropped one in the log.
	OpDetach Op = "detach"
)

// Request is one line from a client.
type Request struct {
	Op Op `json:"op"`

	// Run is what to attach to: a run id, a unique prefix, or a queue name
	// with exactly one live run.
	Run string `json:"run,omitempty"`

	// Text is [OpInput]'s body, and [OpPermission]'s reason.
	Text string `json:"text,omitempty"`

	// ID is the permission request being answered.
	ID string `json:"id,omitempty"`

	// Allow is [OpPermission]'s verdict.
	Allow bool `json:"allow,omitempty"`
}

// Response is one line back. Exactly one per request.
type Response struct {
	// OK is false when Error says why not.
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	// Attached answers [OpAttach].
	Attached *Attached `json:"attached,omitempty"`

	// Runs answers [OpList].
	Runs []RunInfo `json:"runs,omitempty"`

	// Note is anything else worth saying to the person.
	Note string `json:"note,omitempty"`
}

// RunInfo is one live run, as the control plane sees it.
type RunInfo struct {
	RunID     string    `json:"run_id"`
	Queue     string    `json:"queue"`
	AgentKind string    `json:"agent_kind"`
	Direction string    `json:"direction,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	StartedAt time.Time `json:"started_at"`

	// Interactive is whether this session takes text after it started — the
	// adapter's `steer` capability. False means attaching to it can only
	// watch.
	Interactive bool `json:"interactive"`

	// Approvals is whether it honours a permission answer.
	Approvals bool `json:"approvals"`

	// Attached is whether somebody already is.
	Attached bool `json:"attached"`
}

// Attached is what a successful [OpAttach] hands back.
type Attached struct {
	Run RunInfo `json:"run"`

	// Note says what the person can and cannot do, in a sentence they can
	// read — most importantly when the answer is "watch only", which they
	// should learn before they type rather than after.
	Note string `json:"note,omitempty"`
}

// Handler is the runner's side of the control plane.
//
// Attach returns a release function the server calls when the connection
// ends, however it ends. Nothing else has to clean up.
type Handler interface {
	List(ctx context.Context) ([]RunInfo, error)
	Attach(ctx context.Context, ref string) (Attached, func(), error)
	Input(ctx context.Context, runID, text string) error
	Respond(ctx context.Context, runID, id string, allow bool, reason string) error
	Detach(ctx context.Context, runID string) error
}

// maxLine bounds one request. A control socket is a local, trusted channel,
// but a bound is still cheaper than the failure it prevents.
const maxLine = 1 << 20

// Serve accepts connections until ctx is cancelled or the listener closes.
//
// One connection is one session with the control plane, and — once it has
// attached — one attachment. Errors are answered on the connection and never
// returned: a client that asks for something silly must not be able to stop a
// daemon from serving its queues.
func Serve(ctx context.Context, ln net.Listener, h Handler, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			wg.Wait()
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			if err := handle(ctx, conn, h); err != nil && ctx.Err() == nil {
				logf("attach: %v", err)
			}
		}()
	}
}

// handle runs one connection.
func handle(ctx context.Context, conn net.Conn, h Handler) error {
	var (
		enc     = json.NewEncoder(conn)
		br      = bufio.NewReaderSize(conn, 64*1024)
		runID   string
		release func()
	)
	defer func() {
		if release != nil {
			release()
		}
	}()

	reply := func(r Response) error { return enc.Encode(r) }
	fail := func(err error) error { return reply(Response{Error: err.Error()}) }

	for {
		line, err := readLine(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			if err := fail(fmt.Errorf("control: unreadable request: %w", err)); err != nil {
				return err
			}
			continue
		}

		switch req.Op {
		case OpList:
			runs, err := h.List(ctx)
			if err != nil {
				err = fail(err)
			} else {
				err = reply(Response{OK: true, Runs: runs})
			}
			if err != nil {
				return err
			}

		case OpAttach:
			if runID != "" {
				if err := fail(errors.New("control: this connection is already attached")); err != nil {
					return err
				}
				continue
			}
			att, rel, aErr := h.Attach(ctx, req.Run)
			if aErr != nil {
				if err := fail(aErr); err != nil {
					return err
				}
				continue
			}
			runID, release = att.Run.RunID, rel
			if err := reply(Response{OK: true, Attached: &att}); err != nil {
				return err
			}

		case OpInput:
			if runID == "" {
				if err := fail(errors.New("control: attach first")); err != nil {
					return err
				}
				continue
			}
			var err error
			if iErr := h.Input(ctx, runID, req.Text); iErr != nil {
				err = fail(iErr)
			} else {
				err = reply(Response{OK: true, Note: "delivered"})
			}
			if err != nil {
				return err
			}

		case OpPermission:
			if runID == "" {
				if err := fail(errors.New("control: attach first")); err != nil {
					return err
				}
				continue
			}
			var err error
			if rErr := h.Respond(ctx, runID, req.ID, req.Allow, req.Text); rErr != nil {
				err = fail(rErr)
			} else {
				err = reply(Response{OK: true, Note: "answered"})
			}
			if err != nil {
				return err
			}

		case OpDetach:
			if runID != "" {
				_ = h.Detach(ctx, runID)
				release()
				release, runID = nil, ""
			}
			if err := reply(Response{OK: true, Note: "detached"}); err != nil {
				return err
			}
			return nil

		default:
			if err := fail(fmt.Errorf("control: unknown op %q", req.Op)); err != nil {
				return err
			}
		}
	}
}

func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	if len(line) > maxLine {
		return nil, errors.New("control: request too long")
	}
	return []byte(strings.TrimSpace(string(line))), err
}

// Client is the `rein attach` end of the control plane.
type Client struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder

	mu       sync.Mutex
	attached string
}

// Dial connects to the runner serving a Rein home.
func Dial(ctx context.Context, home string) (*Client, error) {
	conn, err := dial(ctx, home)
	if err != nil {
		if errors.Is(err, ErrEndpoint) {
			// Not "nobody is listening": nobody could be.
			return nil, err
		}
		return nil, fmt.Errorf("%w at %s: %v", ErrNoRunner, Endpoint(home), err)
	}
	return &Client{conn: conn, enc: json.NewEncoder(conn), dec: json.NewDecoder(conn)}, nil
}

// Close releases the attachment and closes the connection. The runner resumes
// either way: closing the socket is itself a detach.
func (c *Client) Close() error {
	c.mu.Lock()
	attached := c.attached != ""
	c.attached = ""
	c.mu.Unlock()
	if attached {
		// Best effort, with a short deadline: a clean detach is nicer in the
		// log, and a dropped connection does the same job.
		_ = c.conn.SetDeadline(time.Now().Add(2 * time.Second))
		_ = c.enc.Encode(Request{Op: OpDetach})
	}
	return c.conn.Close()
}

func (c *Client) call(req Request) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enc.Encode(req); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := c.dec.Decode(&resp); err != nil {
		return Response{}, err
	}
	if !resp.OK {
		return resp, errors.New(orDefault(resp.Error, "the runner refused, without saying why"))
	}
	return resp, nil
}

// List asks what is running.
func (c *Client) List() ([]RunInfo, error) {
	resp, err := c.call(Request{Op: OpList})
	return resp.Runs, err
}

// Attach claims one run. The attachment lasts until [Client.Close].
func (c *Client) Attach(ref string) (Attached, error) {
	resp, err := c.call(Request{Op: OpAttach, Run: ref})
	if err != nil {
		return Attached{}, err
	}
	if resp.Attached == nil {
		return Attached{}, errors.New("control: the runner attached to nothing")
	}
	c.mu.Lock()
	c.attached = resp.Attached.Run.RunID
	c.mu.Unlock()
	return *resp.Attached, nil
}

// Input delivers a line to the attached session.
func (c *Client) Input(text string) error {
	_, err := c.call(Request{Op: OpInput, Text: text})
	return err
}

// Respond answers a permission request.
func (c *Client) Respond(id string, allow bool, reason string) error {
	_, err := c.call(Request{Op: OpPermission, ID: id, Allow: allow, Text: reason})
	return err
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
