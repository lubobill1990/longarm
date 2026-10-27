package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/lubobill1990/longarm/internal/localapi"
	"github.com/lubobill1990/longarm/internal/transport"
)

// serveLegProxy serves the leg proxy with dial on a real local channel (as
// longarm daemon does) and returns its address file.
func serveLegProxy(t *testing.T, dial func(ctx context.Context, w Worker, e Endpoint, explicit bool) (net.Conn, transport.PathInfo, error)) string {
	t.Helper()
	dir := t.TempDir()
	addrFile := filepath.Join(dir, "daemon.addr")
	ln, _, err := localapi.Listen("latest", dir, addrFile)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/legs/dial", legProxyHandler(dial))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { localapi.Serve(ctx, ln, grpc.NewServer(), mux); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return addrFile
}

// TestLegProxy: the longnet / tailscale legs of a dialer with a proxy go
// through it (the mTLS handshake with the worker still happens in the
// dialing process); tcp legs never do; errors of the proxy's dial come
// back; no proxy (no address file, stale address) means dialing locally.
func TestLegProxy(t *testing.T) {
	e := newRaceEnv(t)
	worker := e.serve(0)
	var calls atomic.Int32
	var got atomic.Value
	addrFile := serveLegProxy(t, func(ctx context.Context, w Worker, ep Endpoint, explicit bool) (net.Conn, transport.PathInfo, error) {
		calls.Add(1)
		got.Store(w.Name + " " + w.CertName + " " + ep.Key())
		if ep.Address == "fail:1" {
			return nil, transport.PathInfo{}, errors.New("no route to the worker")
		}
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", worker.Addr().String())
		return c, transport.PathInfo{Transport: ep.Transport, Kind: transport.PathDirect, RTT: time.Millisecond, Detail: "1.2.3.4:5"}, err
	})
	p := &LocalLegProxy{AddrFile: addrFile}
	if !p.Available() {
		t.Fatal("proxy not available")
	}
	e.d.Proxy = p

	// longnet leg (only): through the proxy.
	w := Worker{Name: "w1", CertName: "w1", Transport: "longnet", Address: "relay.invalid:7443"}
	won, legs, err := e.race(w)
	if err != nil || won.Transport != "longnet" {
		t.Fatalf("race via the proxy: %v %v (%s)", won, err, RaceSummary(legs))
	}
	if calls.Load() != 1 || got.Load() != "w1 w1 longnet://relay.invalid:7443" {
		t.Fatalf("proxy calls %d, %v", calls.Load(), got.Load())
	}
	if pi := legs[0].Path; pi.Kind != transport.PathDirect || !strings.Contains(pi.Detail, "1.2.3.4:5, via longarm daemon") {
		t.Fatalf("path %+v", pi)
	}

	// A tcp leg is dialed here, not through the proxy.
	calls.Store(0)
	if won, legs, err := e.race(Worker{Name: "w1", CertName: "w1", Transport: "tcp", Address: worker.Addr().String()}); err != nil || won.Transport != "tcp" || calls.Load() != 0 {
		t.Fatalf("tcp leg: %v %v calls %d (%s)", won, err, calls.Load(), RaceSummary(legs))
	}

	// The proxy's dial error is the leg's error (no local fallback).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err = p.DialLeg(ctx, Worker{Name: "w1", CertName: "w1"}, Endpoint{Transport: "longnet", Address: "fail:1"}, false)
	if err == nil || !strings.Contains(err.Error(), "via longarm daemon: no route to the worker") {
		t.Fatalf("proxy dial error: %v", err)
	}
	// The proxy refuses legs it doesn't proxy.
	if _, _, err = p.DialLeg(ctx, Worker{Name: "w1"}, Endpoint{Transport: "tcp", Address: "x:1"}, false); err == nil || !strings.Contains(err.Error(), "not proxied") {
		t.Fatalf("tcp through the proxy: %v", err)
	}

	// No address file / a stale one: ErrNoProxy (dial locally).
	none := &LocalLegProxy{AddrFile: filepath.Join(t.TempDir(), "daemon.addr")}
	if none.Available() {
		t.Fatal("available without an address file")
	}
	if _, _, err := none.DialLeg(ctx, w, Endpoint{Transport: "longnet", Address: "x:1"}, false); !errors.Is(err, ErrNoProxy) {
		t.Fatalf("no daemon: %v", err)
	}
	stale := filepath.Join(t.TempDir(), "daemon.addr")
	b, _ := os.ReadFile(addrFile)
	os.WriteFile(stale, []byte(strings.Replace(string(b), "latest-", "lagone-", 1)), 0o600)
	sp := &LocalLegProxy{AddrFile: stale}
	if _, _, err := sp.DialLeg(ctx, w, Endpoint{Transport: "longnet", Address: "x:1"}, false); !errors.Is(err, ErrNoProxy) {
		t.Fatalf("stale daemon address: %v", err)
	}
	if sp.Available() {
		t.Fatal("stale address available")
	}
}

// TestLegProxyCancel: a caller that gives up while the proxy still dials
// gets its context error at once.
func TestLegProxyCancel(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	addrFile := serveLegProxy(t, func(ctx context.Context, w Worker, ep Endpoint, explicit bool) (net.Conn, transport.PathInfo, error) {
		select {
		case <-ctx.Done():
		case <-release:
		}
		return nil, transport.PathInfo{}, errors.New("gave up")
	})
	p := &LocalLegProxy{AddrFile: addrFile}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := p.DialLeg(ctx, Worker{Name: "w1"}, Endpoint{Transport: "tailscale", Address: "100.64.0.1:7701"}, false)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("cancelled proxy dial: %v after %s", err, time.Since(start))
	}
}

// TestOneShotCloseDoesNotWait: Close of a OneShot dialer returns at once
// even when a node takes long to shut down.
func TestOneShotCloseDoesNotWait(t *testing.T) {
	e := newRaceEnv(t)
	e.d.OneShot = true
	e.d.cfg.Tailscale.ControlURL = "http://127.0.0.1:1" // nobody answers: the node stays starting
	e.d.cfg.Tailscale.Hostname = "longarm-client-test"
	ts := e.d.tsnetTransport().(*transport.TSNet)
	ts.Warm()
	// Close doesn't wait for the node, so it may still write its state
	// under the config dir: let it finish before t.TempDir removes the dir
	// (cleanups run last-registered first).
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for os.RemoveAll(e.d.cfg.Dir) != nil && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
	})
	start := time.Now()
	e.d.Close()
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Close took %s", took)
	}
}

// TestLegProxyFallback: legs the daemon can't serve come back as
// ErrNoProxy (the caller dials them itself): an endpoint the daemon doesn't
// know for the worker, a tailscale leg while it has no tailscale node, a
// leg it would skip; a daemon without the leg proxy (404) is remembered.
func TestLegProxyFallback(t *testing.T) {
	e := newRaceEnv(t)
	known := Worker{Name: "w1", CertName: "w1", Transport: "tcp", Address: "127.0.0.1:1",
		Alt: []Endpoint{{Transport: "longnet", Address: "relay.known:7443"}, {Transport: "tailscale", Address: "100.64.0.9:7701"}}}
	dir := t.TempDir()
	addrFile := filepath.Join(dir, "daemon.addr")
	ln, _, err := localapi.Listen("latest", dir, addrFile)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/legs/dial", LegProxyHandler(e.d, func(name string) (Worker, error) {
		if name != "w1" {
			return Worker{}, errors.New("unknown worker")
		}
		return known, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { localapi.Serve(ctx, ln, grpc.NewServer(), mux); close(done) }()
	defer func() { cancel(); <-done }()

	p := &LocalLegProxy{AddrFile: addrFile}
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()
	for _, c := range []struct {
		w    string
		e    Endpoint
		want string
	}{
		{"w1", Endpoint{Transport: "longnet", Address: "relay.other:7443"}, "doesn't know longnet://relay.other:7443"},
		{"w9", Endpoint{Transport: "longnet", Address: "relay.known:7443"}, "doesn't know"},
		{"w1", Endpoint{Transport: "tailscale", Address: "100.64.0.9:7701"}, "no tailscale node"},
	} {
		_, _, err := p.DialLeg(dctx, Worker{Name: c.w, CertName: c.w}, c.e, false)
		if !errors.Is(err, ErrNoProxy) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s %s: %v", c.w, c.e.Key(), err)
		}
	}
	if !p.Available() {
		t.Fatal("a fallback marked the daemon down")
	}

	// A daemon without the endpoint (an older version): 404, remembered.
	oldMux := http.NewServeMux() // nothing on /v1/legs/dial
	dir2 := t.TempDir()
	oldFile := filepath.Join(dir2, "daemon.addr")
	ln2, _, err := localapi.Listen("laold", dir2, oldFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { localapi.Serve(ctx2, ln2, grpc.NewServer(), oldMux); close(done2) }()
	defer func() { cancel2(); <-done2 }()
	po := &LocalLegProxy{AddrFile: oldFile}
	if !po.Available() {
		t.Fatal("old daemon not reachable")
	}
	if _, _, err := po.DialLeg(dctx, Worker{Name: "w1"}, Endpoint{Transport: "longnet", Address: "x:1"}, false); !errors.Is(err, ErrNoProxy) {
		t.Fatalf("old daemon: %v", err)
	}
	if po.Available() {
		t.Fatal("old daemon still counted as a leg proxy")
	}
}

// TestLiveLegsOverLocalChannel: a one-shot longarm asks the daemon for its live
// view of a worker's legs; no daemon or an older one (404) means none.
func TestLiveLegsOverLocalChannel(t *testing.T) {
	dir := t.TempDir()
	addrFile := filepath.Join(dir, "daemon.addr")
	ln, _, err := localapi.Listen("latest", dir, addrFile)
	if err != nil {
		t.Fatal(err)
	}
	want := []LiveLeg{{Leg: "tcp://10.0.0.2:7701", Up: true, Kind: "direct", RTTMs: 77}, {Leg: "longnet://relay:7443"}}
	mux := http.NewServeMux()
	mux.Handle("GET /v1/legs/live", LiveLegsHandler(func(w string) []LiveLeg {
		if w == "w 1" {
			return want
		}
		return nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { localapi.Serve(ctx, ln, grpc.NewServer(), mux); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	p := &LocalLegProxy{AddrFile: addrFile}
	lctx, lcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer lcancel()
	got := p.LiveLegs(lctx, "w 1")
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("LiveLegs = %+v", got)
	}
	if got := p.LiveLegs(lctx, "other"); len(got) != 0 {
		t.Fatalf("unknown worker: %+v", got)
	}
	// An older daemon without the route.
	old := serveLegProxy(t, nil)
	if got := (&LocalLegProxy{AddrFile: old}).LiveLegs(lctx, "w 1"); got != nil {
		t.Fatalf("old daemon: %+v", got)
	}
	if got := (&LocalLegProxy{AddrFile: filepath.Join(dir, "none")}).LiveLegs(lctx, "w 1"); got != nil {
		t.Fatalf("no daemon: %+v", got)
	}
}
