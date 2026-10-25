// Package longnet is the self-made tunnel of DESIGN §4.3 (a simplified
// magicsock): end-to-end QUIC with the Longarm mTLS certificates, running
// over a virtual PacketConn that sends each packet either directly over UDP
// (after hole punching with signed disco probes) or through the relay
// (internal/relay). Paths switch without the QUIC connection noticing: peers
// have stable virtual addresses.
//
// Listen URL (longarmd):  longnet://<relay-host:port>?name=<worker>[&tcp=host:port][&bind=:7702][&mode=auto|udp|tcp][&direct=0][&relay_name=DNS][&stun=host:port,...]
// Client (longarm):       workers.toml: transport = "longnet", address = "<relay-host:port>"
//
// Debug knobs (environment): LONGARM_NET_DISABLE_DIRECT=1 (always relay),
// LONGARM_NET_RELAY_MODE=udp|tcp|auto (LONGARM_NET_FORCE_TCP=1 = tcp), LONGARM_NET_STUN=host:port,...
// (extra STUN servers, e.g. a public one at another IP for better NAT type
// detection), LONGARM_NET_LOG=1|debug (path events on stderr when the caller has no
// logger, i.e. in longarm).
package longnet

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	"github.com/lubobill1990/longarm/internal/transport"
)

func init() {
	transport.RegisterScheme("longnet", func(u *url.URL, env transport.Env) (transport.Transport, error) {
		if env.Bundle == nil {
			return nil, fmt.Errorf("longnet needs the node's TLS bundle")
		}
		o, err := OptionsFromURL(u)
		if err != nil {
			return nil, err
		}
		if name := u.Query().Get("name"); name != "" && name != env.Bundle.Identity.Name {
			return nil, fmt.Errorf("listen %s: name=%s but the certificate is for %s", u.Redacted(), name, env.Bundle.Identity.Name)
		}
		o.Bundle, o.Logger = env.Bundle, env.Logger
		return New(o), nil
	})
}

// ParseMode parses "auto" | "udp" | "tcp".
func ParseMode(s string) (relay.Mode, error) {
	switch strings.ToLower(s) {
	case "", "auto":
		return relay.ModeAuto, nil
	case "udp", "quic":
		return relay.ModeUDP, nil
	case "tcp", "tls":
		return relay.ModeTCP, nil
	}
	return 0, fmt.Errorf("longnet: unknown relay mode %q (auto|udp|tcp)", s)
}

// OptionsFromURL parses a listen URL (see package doc); environment knobs
// override.
func OptionsFromURL(u *url.URL) (Options, error) {
	if u.Host == "" {
		return Options{}, fmt.Errorf("listen %s: missing relay host:port", u.Redacted())
	}
	q := u.Query()
	mode, err := ParseMode(q.Get("mode"))
	if err != nil {
		return Options{}, err
	}
	o := Options{Relay: u.Host, RelayTCP: q.Get("tcp"), RelayName: q.Get("relay_name"), Bind: q.Get("bind"), Mode: mode}
	if d := q.Get("direct"); d == "0" || d == "false" {
		o.DisableDirect = true
	}
	o.STUN = splitList(q.Get("stun"))
	return ApplyEnv(o)
}

// ApplyEnv applies the LONGARM_NET_* debug knobs.
func ApplyEnv(o Options) (Options, error) {
	if v := os.Getenv("LONGARM_NET_DISABLE_DIRECT"); v == "1" || v == "true" {
		o.DisableDirect = true
	}
	if v := os.Getenv("LONGARM_NET_FORCE_TCP"); v == "1" || v == "true" {
		o.Mode = relay.ModeTCP
	}
	if v := os.Getenv("LONGARM_NET_RELAY_MODE"); v != "" {
		m, err := ParseMode(v)
		if err != nil {
			return o, err
		}
		o.Mode = m
	}
	if v := os.Getenv("LONGARM_NET_STUN"); v != "" {
		o.STUN = append(o.STUN, splitList(v)...)
	}
	if v := os.Getenv("LONGARM_NET_LOG"); v != "" && v != "0" && o.Logger == nil {
		level := slog.LevelInfo
		if v == "debug" {
			level = slog.LevelDebug
		}
		o.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}
	return o, nil
}

func splitList(s string) []string {
	var out []string
	for _, e := range strings.Split(s, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// Transport adapts a lazily created Node to transport.Transport.
type Transport struct {
	opts Options

	mu   sync.Mutex
	node *Node
}

// New returns a longnet transport; the node starts on first Listen/Dial.
func New(o Options) *Transport { return &Transport{opts: o} }

func (t *Transport) Name() string { return "longnet" }

// Node returns the node, starting it if needed.
func (t *Transport) Node() (*Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.node == nil {
		n, err := NewNode(t.opts)
		if err != nil {
			return nil, err
		}
		t.node = n
	}
	return t.node, nil
}

// Listen implements transport.Transport (longarmd).
func (t *Transport) Listen(ctx context.Context) (net.Listener, error) {
	n, err := t.Node()
	if err != nil {
		return nil, err
	}
	if t.opts.Logger != nil {
		t.opts.Logger.Info("longnet: listening", "relay", t.opts.Relay, "udp", n.LocalAddr(), "id", n.ID(),
			"direct", !t.opts.DisableDirect)
	}
	return n.Listen()
}

// Dial implements transport.Transport (longarm); node is the worker's
// certificate name.
func (t *Transport) Dial(ctx context.Context, node transport.NodeID) (net.Conn, transport.PathInfo, error) {
	n, err := t.Node()
	if err != nil {
		return nil, transport.PathInfo{}, err
	}
	return n.Dial(ctx, string(node))
}

// Close stops the node.
func (t *Transport) Close() error {
	t.mu.Lock()
	n := t.node
	t.node = nil
	t.mu.Unlock()
	if n != nil {
		return n.Close()
	}
	return nil
}

// ClientOptions builds client-side options (longarm) from config values.
func ClientOptions(relayAddr, relayTCP, mode string, disableDirect bool, b *pki.Bundle, log *slog.Logger) (Options, error) {
	m, err := ParseMode(mode)
	if err != nil {
		return Options{}, err
	}
	return ApplyEnv(Options{Relay: relayAddr, RelayTCP: relayTCP, Mode: m, DisableDirect: disableDirect, Bundle: b, Logger: log})
}
