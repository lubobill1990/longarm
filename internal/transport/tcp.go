package transport

import (
	"context"
	"net"
	"time"
)

// TCP is the plain TCP backend.
type TCP struct {
	Addr string // listen address (longarmd); unused for Dial
}

func (t *TCP) Name() string { return "tcp" }

// Listen listens on t.Addr.
func (t *TCP) Listen(ctx context.Context) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", t.Addr)
}

// Dial connects to node ("host:port"; DefaultPort if no port).
func (t *TCP) Dial(ctx context.Context, node NodeID) (net.Conn, PathInfo, error) {
	addr := splitHostPortDefault(string(node), DefaultPort)
	d := net.Dialer{KeepAlive: 15 * time.Second}
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, PathInfo{}, err
	}
	return c, PathInfo{Transport: t.Name(), Kind: PathDirect, RTT: time.Since(start), Detail: c.RemoteAddr().String()}, nil
}
