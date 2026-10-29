package updatesvc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrNoService: this machine has no LongarmUpdate service (not an MSI
// install, an older MSI, or not Windows).
var ErrNoService = errors.New("the Longarm Update service is not installed here")

// ErrImpostor: the pipe's server is not the LongarmUpdate service.
var ErrImpostor = errors.New("the update pipe is served by another program, not the Longarm Update service")

// Client talks to the service.
type Client struct {
	// Dial connects to the pipe (DialPipe on Windows; tests use their own).
	Dial func(ctx context.Context) (net.Conn, error)
	// VerifyServer checks that the other end is the service (Windows:
	// VerifyPipeServer); a failure ends the request with its error.
	VerifyServer func(net.Conn) error
	// Requester is sent along for the service's log.
	Requester string
}

// NewClient is the client of this machine's service (Windows; elsewhere
// every request fails with ErrNoService).
func NewClient(requester string) *Client {
	return &Client{Dial: dialService, VerifyServer: VerifyPipeServer, Requester: requester}
}

// Do sends req and returns the service's answer (a refusal is a Response
// with OK false, see Response.Err).
func (c *Client) Do(ctx context.Context, req *Request) (*Response, error) {
	if req.Requester == "" {
		req.Requester = c.Requester
	}
	conn, err := c.Dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if c.VerifyServer == nil {
		return nil, errors.New("update service client without a server check")
	}
	if err := c.VerifyServer(conn); err != nil {
		return nil, err
	}
	d := 30 * time.Second
	if req.Op == OpInstall {
		d = installDeadline + time.Minute
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d {
		d = time.Until(dl)
	}
	conn.SetDeadline(time.Now().Add(d))
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()
	if err := writeMessage(conn, req); err != nil {
		return nil, fmt.Errorf("update service: %w", err)
	}
	var resp Response
	if err := readMessage(conn, &resp, false); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("update service: %w", err)
	}
	return &resp, nil
}

// Status asks for the service's status.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	r, err := c.Do(ctx, &Request{Op: OpStatus})
	if err != nil {
		return nil, err
	}
	if err := r.Err(); err != nil {
		return nil, err
	}
	if r.Status == nil {
		return nil, errors.New("update service: no status in the answer")
	}
	return r.Status, nil
}
