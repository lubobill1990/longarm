package client

// Leg proxy: a one-shot longarm process dials the tailscale and longnet legs
// through the running `longarm daemon`. Both need a long-lived node (a tsnet
// node takes seconds to come up, longnet's hole punching needs time and
// state), and the daemon keeps them up with direct paths already found. The
// daemon only opens the raw stream to the worker; the mTLS handshake with
// the worker runs end to end in the longarm process, so the daemon never sees
// the plaintext and the worker still checks this client's own certificate.
//
// Wire format (local channel, HTTP/1.1 upgrade):
//
//	POST /v1/legs/dial  Connection: Upgrade, Upgrade: longarm-leg
//	{"worker":..,"cert_name":..,"transport":..,"address":..,"explicit":..}
//	-> 101 Switching Protocols, Longarm-Path: <PathInfo JSON>; then raw bytes
//	-> 4xx/5xx {"error": ...} when the daemon could not dial the leg

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/localapi"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/transport"
	"github.com/lubobill1990/longarm/internal/transport/longnet"
)

// LegProxy dials single transport legs on behalf of this process.
type LegProxy interface {
	// Available reports (cheaply) whether the proxy runs.
	Available() bool
	// DialLeg opens a raw stream to w over leg e (no mTLS yet). ErrNoProxy:
	// the proxy isn't there, dial the leg locally.
	DialLeg(ctx context.Context, w Worker, e Endpoint, explicit bool) (net.Conn, transport.PathInfo, error)
}

// ErrNoProxy: the leg proxy (longarm daemon) does not run.
var ErrNoProxy = errors.New("longarm daemon not running")

// proxied reports whether a leg's transport needs a long-lived node (and
// so goes through the proxy when there is one).
func proxied(tr transport.Transport) bool {
	switch tr.(type) {
	case *transport.TSNet, *longnet.Transport:
		return true
	}
	return false
}

// proxyUp reports whether this dialer's proxy runs.
func (d *Dialer) proxyUp() bool { return d.Proxy != nil && d.Proxy.Available() }

// DialLeg dials one leg of w without the mTLS handshake, always with this
// process' own nodes (the daemon's side of the leg proxy).
func (d *Dialer) DialLeg(ctx context.Context, w Worker, e Endpoint, explicit bool) (net.Conn, transport.PathInfo, error) {
	tr, node, err := d.legTransport(w, e, explicit, false)
	if err != nil {
		return nil, transport.PathInfo{}, err
	}
	return tr.Dial(ctx, node)
}

// legDialRequest is the body of POST /v1/legs/dial.
type legDialRequest struct {
	Worker    string `json:"worker"`
	CertName  string `json:"cert_name"`
	Transport string `json:"transport"`
	Address   string `json:"address"`
	Explicit  bool   `json:"explicit"`
}

// legPath is PathInfo on the wire.
type legPath struct {
	Transport string `json:"transport"`
	Kind      string `json:"kind"`
	RTTNs     int64  `json:"rtt_ns"`
	Detail    string `json:"detail"`
}

const legUpgrade = "longarm-leg"

// LocalLegProxy is the client side: the longarm daemon whose local channel
// address is in AddrFile.
type LocalLegProxy struct {
	AddrFile string

	mu          sync.Mutex
	checked     time.Time
	up          bool
	unsupported bool // the daemon has no leg proxy (an older version)
}

// availableTTL: how long an Available answer is reused.
const availableTTL = 5 * time.Second

func (p *LocalLegProxy) Available() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unsupported {
		return false
	}
	if !p.checked.IsZero() && time.Since(p.checked) < availableTTL {
		return p.up
	}
	p.checked = time.Now()
	p.up = false
	addr, err := localapi.ReadAddr(p.AddrFile)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c, err := platform.DialLocal(ctx, addr)
	if err != nil {
		return false
	}
	c.Close()
	p.up = true
	return true
}

func (p *LocalLegProxy) markDown() {
	p.mu.Lock()
	p.checked, p.up = time.Now(), false
	p.mu.Unlock()
}

func (p *LocalLegProxy) DialLeg(ctx context.Context, w Worker, e Endpoint, explicit bool) (net.Conn, transport.PathInfo, error) {
	addr, err := localapi.ReadAddr(p.AddrFile)
	if err != nil {
		return nil, transport.PathInfo{}, ErrNoProxy
	}
	c, err := platform.DialLocal(ctx, addr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, transport.PathInfo{}, ctx.Err()
		}
		p.markDown() // a stale address file: the daemon is gone
		return nil, transport.PathInfo{}, ErrNoProxy
	}
	// The request and the response head are bounded by ctx; the stream
	// itself isn't.
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Unix(1, 0)) })
	conn, pi, err := legHandshake(c, legDialRequest{Worker: w.Name, CertName: w.CertName, Transport: e.Transport, Address: e.Address, Explicit: explicit})
	if !stop() || err != nil {
		c.Close()
		if ctx.Err() != nil {
			return nil, transport.PathInfo{}, ctx.Err()
		}
		if errors.Is(err, errOldDaemon) {
			p.mu.Lock()
			p.unsupported = true
			p.mu.Unlock()
			return nil, transport.PathInfo{}, ErrNoProxy
		}
		return nil, transport.PathInfo{}, err
	}
	return conn, pi, nil
}

// errOldDaemon: the daemon answered 404 (no leg proxy).
var errOldDaemon = errors.New("longarm daemon without a leg proxy")

func legHandshake(c net.Conn, lr legDialRequest) (net.Conn, transport.PathInfo, error) {
	body, _ := json.Marshal(lr)
	req, _ := http.NewRequest(http.MethodPost, "http://longarm-daemon/v1/legs/dial", bytes.NewReader(body))
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", legUpgrade)
	req.Header.Set("Content-Type", "application/json")
	if err := req.Write(c); err != nil {
		return nil, transport.PathInfo{}, fmt.Errorf("longarm daemon: %w", err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, transport.PathInfo{}, fmt.Errorf("longarm daemon: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		var e struct {
			Error    string `json:"error"`
			Fallback bool   `json:"fallback"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if resp.StatusCode == http.StatusNotFound && e.Error == "" {
			return nil, transport.PathInfo{}, errOldDaemon
		}
		if e.Fallback {
			// The daemon can't serve this leg (no tailscale node there, a
			// leg it doesn't know): this process dials it.
			return nil, transport.PathInfo{}, fmt.Errorf("%w (%s)", ErrNoProxy, msg)
		}
		return nil, transport.PathInfo{}, fmt.Errorf("via longarm daemon: %s", msg)
	}
	var lp legPath
	json.Unmarshal([]byte(resp.Header.Get("Longarm-Path")), &lp)
	pi := transport.PathInfo{Transport: lp.Transport, Kind: transport.PathKind(lp.Kind), RTT: time.Duration(lp.RTTNs), Detail: lp.Detail}
	if pi.Detail != "" {
		pi.Detail += ", "
	}
	pi.Detail += "via longarm daemon"
	c.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufConn{Conn: c, r: br}, pi, nil
	}
	return c, pi, nil
}

// bufConn reads what the response reader buffered first.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// errNotHere: the daemon can't serve a leg; the caller dials it itself.
type errNotHere struct{ why string }

func (e errNotHere) Error() string { return e.why }

// LegProxyHandler serves POST /v1/legs/dial for the longarm daemon: it dials
// the leg with d's own (long-lived) nodes and splices the stream. Only the
// tailscale and longnet legs are proxied; the local channel itself is the
// access control (owner only). resolve is the daemon's own view of a
// worker: only its known endpoints are dialed (with its certificate
// name), anything else - and a tailscale leg while the daemon has no
// tailscale node - is left to the caller.
//
// The stream is a plain byte pipe: a live path (longnet moving from the
// relay to a direct path) isn't passed on; the caller sees the path at
// dial time.
func LegProxyHandler(d *Dialer, resolve func(name string) (Worker, error)) http.HandlerFunc {
	return legProxyHandler(func(ctx context.Context, w Worker, e Endpoint, explicit bool) (net.Conn, transport.PathInfo, error) {
		known, err := resolve(w.Name)
		if err != nil || !containsKey(known.Legs(), e.Key()) {
			return nil, transport.PathInfo{}, errNotHere{"longarm daemon doesn't know " + e.Key() + " for " + w.Name}
		}
		if e.Transport == "tailscale" && !d.tsnetConfigured() {
			return nil, transport.PathInfo{}, errNotHere{"longarm daemon has no tailscale node"}
		}
		return d.DialLeg(ctx, known, e, false)
	})
}

func legProxyHandler(dial func(ctx context.Context, w Worker, e Endpoint, explicit bool) (net.Conn, transport.PathInfo, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fail := func(code int, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			b, _ := json.Marshal(map[string]string{"error": err.Error()})
			w.Write(b)
		}
		if !strings.EqualFold(r.Header.Get("Upgrade"), legUpgrade) {
			fail(http.StatusBadRequest, errors.New("want Upgrade: "+legUpgrade))
			return
		}
		var lr legDialRequest
		err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&lr)
		io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10)) // all of it: then the server notices a caller that gives up
		if err != nil {
			fail(http.StatusBadRequest, err)
			return
		}
		if lr.Transport != "tailscale" && lr.Transport != "longnet" {
			fail(http.StatusBadRequest, fmt.Errorf("transport %q is not proxied", lr.Transport))
			return
		}
		if lr.Worker == "" || lr.Address == "" {
			fail(http.StatusBadRequest, errors.New("worker and address are required"))
			return
		}
		if lr.CertName == "" {
			lr.CertName = lr.Worker
		}
		wk := Worker{Name: lr.Worker, CertName: lr.CertName, Transport: lr.Transport, Address: lr.Address}
		// The caller bounds the dial: it closes the channel when it gives
		// up, which cancels r.Context().
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		up, pi, err := dial(ctx, wk, Endpoint{Transport: lr.Transport, Address: lr.Address}, lr.Explicit)
		var skip errSkip
		var nh errNotHere
		if errors.As(err, &skip) || errors.As(err, &nh) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMisdirectedRequest)
			b, _ := json.Marshal(map[string]any{"error": err.Error(), "fallback": true})
			w.Write(b)
			return
		}
		if err != nil {
			fail(http.StatusBadGateway, err)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			up.Close()
			fail(http.StatusInternalServerError, errors.New("connection can't be hijacked"))
			return
		}
		c, brw, err := hj.Hijack()
		if err != nil {
			up.Close()
			return
		}
		if pi.Transport == "" {
			pi.Transport = lr.Transport
		}
		// The current path (a live tailscale/longnet path may have moved since
		// the dial).
		if lp, ok := up.(livePath); ok {
			pi = lp.PathInfo()
		}
		ph, _ := json.Marshal(legPath{Transport: pi.Transport, Kind: string(pi.Kind), RTTNs: int64(pi.RTT), Detail: pi.Detail})
		head := "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + legUpgrade + "\r\nLongarm-Path: " + string(ph) + "\r\n\r\n"
		if _, err := io.WriteString(c, head); err != nil {
			c.Close()
			up.Close()
			return
		}
		var down net.Conn = c
		if brw.Reader.Buffered() > 0 {
			down = &bufConn{Conn: c, r: brw.Reader}
		}
		go splice(down, up)
	}
}

// splice copies both ways until either side ends, then closes both.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
	<-done
}

// LiveLegsHandler serves GET /v1/legs/live?worker=NAME for the longarm daemon:
// the worker's legs as its pool monitor measures them (a JSON array of
// LiveLeg; empty when unknown).
func LiveLegsHandler(live func(worker string) []LiveLeg) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		legs := live(r.URL.Query().Get("worker"))
		if legs == nil {
			legs = []LiveLeg{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(legs)
	}
}

// LiveLegs asks the longarm daemon how it measures the worker's legs (nil when
// no daemon runs, it is an older version or doesn't answer within ctx).
func (p *LocalLegProxy) LiveLegs(ctx context.Context, worker string) []LiveLeg {
	if !p.Available() {
		return nil
	}
	addr, err := localapi.ReadAddr(p.AddrFile)
	if err != nil {
		return nil
	}
	c, err := platform.DialLocal(ctx, addr)
	if err != nil {
		return nil
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	req, _ := http.NewRequest(http.MethodGet, "http://longarm-daemon/v1/legs/live?worker="+url.QueryEscape(worker), nil)
	if req.Write(c) != nil {
		return nil
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var legs []LiveLeg
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&legs) != nil {
		return nil
	}
	return legs
}
