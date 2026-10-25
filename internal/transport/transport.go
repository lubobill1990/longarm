// Package transport is the network abstraction of DESIGN §4.1. A Transport
// yields plain net.Conns; gRPC + app-level mTLS always run on top, so
// authentication never depends on the network backend.
//
// Backends in P0a:
//   - tcp: plain TCP on a given address (127.0.0.1 in tests, or the system
//     Tailscale IP when tailscaled is installed on the machine)
//   - tailscale: embedded tailscale.com/tsnet node (no tailscaled needed)
//
// More backends register a URL scheme with RegisterScheme (longnet: QUIC + hole
// punching + relay, in internal/transport/longnet; importing that package
// registers "longnet://").
package transport

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

// NodeID addresses a node within a transport: "host:port" for tcp,
// "hostname[:port]" (tailnet name or 100.x IP) for tailscale.
type NodeID string

// PathKind describes how bytes travel to the peer.
type PathKind string

const (
	PathDirect    PathKind = "direct"     // straight UDP/TCP to the peer
	PathPeerRelay PathKind = "peer-relay" // tailscale peer relay
	PathDERP      PathKind = "derp"       // tailscale DERP relay
	PathRelay     PathKind = "relay"      // longnet relay (Detail: udp|tcp + relay address)
	PathUnknown   PathKind = "unknown"
)

// PathInfo is returned by Dial.
type PathInfo struct {
	Transport string
	Kind      PathKind
	RTT       time.Duration // connect latency as a cheap RTT proxy
	Detail    string        // e.g. remote endpoint or DERP region
}

func (p PathInfo) String() string {
	s := fmt.Sprintf("%s/%s %s", p.Transport, p.Kind, p.RTT.Round(100*time.Microsecond))
	if p.Detail != "" {
		s += " (" + p.Detail + ")"
	}
	return s
}

// Transport is the interface of DESIGN §4.1.
type Transport interface {
	Name() string                                                      // "tcp" | "tailscale" | "longnet"
	Listen(ctx context.Context) (net.Listener, error)                  // longarmd
	Dial(ctx context.Context, node NodeID) (net.Conn, PathInfo, error) // longarm
}

// DefaultPort is longarmd's default listen port.
const DefaultPort = 7701

// ParseListen builds a listening transport from a URL:
//
//	tcp://127.0.0.1:7701
//	tcp://100.64.0.11:7701
//	tailscale://longarm-myhost:7701?dir=C:/data/tsnet&control=https://hs.example.com&ephemeral=1
//
// The tailscale auth key comes from $TS_AUTHKEY (never from the URL, to keep
// it out of service command lines).
func ParseListen(s string, defaultStateDir string) (Transport, error) {
	return ParseListenEnv(s, Env{StateDir: defaultStateDir})
}

// Env is what a listening transport may need from longarmd.
type Env struct {
	StateDir string      // default state dir (tailscale)
	Bundle   *pki.Bundle // the node's TLS bundle (longnet: QUIC + relay auth)
	Logger   *slog.Logger
	// TailnetKey fetches a one-time key for the embedded tailscale node of
	// a listen URL with control server control (from the hub); nil = none.
	TailnetKey func(ctx context.Context, control, reason string) (string, error)
}

// SchemeFactory builds a listening transport for a registered URL scheme.
type SchemeFactory func(u *url.URL, env Env) (Transport, error)

var (
	schemesMu sync.Mutex
	schemes   = map[string]SchemeFactory{}
)

// RegisterScheme registers a listen URL scheme (called from init of the
// backend package).
func RegisterScheme(scheme string, f SchemeFactory) {
	schemesMu.Lock()
	defer schemesMu.Unlock()
	schemes[scheme] = f
}

// ParseListenEnv is ParseListen with the full environment (registered
// schemes such as longnet need the TLS bundle).
func ParseListenEnv(s string, env Env) (Transport, error) {
	defaultStateDir := env.StateDir
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", s, err)
	}
	switch u.Scheme {
	case "tcp":
		if u.Host == "" {
			return nil, fmt.Errorf("listen %q: missing host:port", s)
		}
		return &TCP{Addr: u.Host}, nil
	case "tailscale", "tsnet":
		port := DefaultPort
		if p := u.Port(); p != "" {
			if port, err = strconv.Atoi(p); err != nil {
				return nil, fmt.Errorf("listen %q: bad port", s)
			}
		}
		q := u.Query()
		dir := q.Get("dir")
		control := q.Get("control")
		if dir == "" {
			dir = StateDirFor(defaultStateDir, control) // one state dir per control server
		}
		cfg := TSNetConfig{
			Hostname:    u.Hostname(),
			Dir:         dir,
			AuthKey:     os.Getenv("TS_AUTHKEY"),
			AuthKeyFile: dir + ".authkey",
			ControlURL:  control,
			Port:        port,
			Ephemeral:   q.Get("ephemeral") == "1" || q.Get("ephemeral") == "true",
		}
		if env.TailnetKey != nil && control != "" && cfg.AuthKey == "" {
			cfg.KeyFunc = func(ctx context.Context, reason string) (string, error) {
				return env.TailnetKey(ctx, control, reason)
			}
		}
		return NewTSNet(cfg), nil
	}
	schemesMu.Lock()
	f := schemes[u.Scheme]
	schemesMu.Unlock()
	if f != nil {
		return f(u, env)
	}
	return nil, fmt.Errorf("listen %q: unknown transport %q (want tcp, tailscale or longnet)", s, u.Scheme)
}

// splitHostPortDefault adds DefaultPort when node has no port.
func splitHostPortDefault(node string, def int) string {
	if _, _, err := net.SplitHostPort(node); err == nil {
		return node
	}
	return net.JoinHostPort(strings.Trim(node, "[]"), strconv.Itoa(def))
}

// Advertiser is implemented by transports whose listener address is not
// what a client dials (tailscale listens on ":port"; clients need the
// tailnet IP). longarmd advertises the returned URL to the hub.
type Advertiser interface {
	AdvertiseURL(ln net.Listener) string
}
