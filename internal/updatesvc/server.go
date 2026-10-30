package updatesvc

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// installDeadline: an install request may take this long (download,
// verification) before the connection is cut.
const installDeadline = 20 * time.Minute

// Serve answers requests on ln (one request per connection, a few at a
// time) until ctx ends. identify tells who is on the other end of a
// connection (Windows: the pipe client's token); a connection it can't
// identify gets nothing.
func Serve(ctx context.Context, ln net.Listener, s *Service, identify func(net.Conn) (Caller, error)) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			// too many at once: answer briefly, don't queue
			c.SetDeadline(time.Now().Add(2 * time.Second))
			writeMessage(c, refuse(CodeRateLimited, "the update service is busy: try again"))
			c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			serveConn(ctx, c, s, identify)
		}()
	}
}

func serveConn(ctx context.Context, c net.Conn, s *Service, identify func(net.Conn) (Caller, error)) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	caller, err := identify(c)
	if err != nil {
		s.cfg.Log.Warn("pipe client not identified", "err", err)
		return
	}
	var req Request
	if err := readMessage(c, &req, true); err != nil {
		writeMessage(c, refuse(CodeInvalid, "bad request: %v", err))
		return
	}
	if req.Op == OpInstall {
		c.SetDeadline(time.Now().Add(installDeadline))
	}
	rctx, cancel := context.WithTimeout(ctx, installDeadline)
	defer cancel()
	resp := s.Handle(rctx, caller, &req)
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	writeMessage(c, resp)
}
