package hub

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/control/controlhttp"
	"tailscale.com/control/controlhttp/controlhttpserver"
	"tailscale.com/derp"
	"tailscale.com/derp/derphttp"
	"tailscale.com/derp/derpserver"
	"tailscale.com/net/netmon"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// proxyBackend records what reaches "Headscale" behind the proxy.
type proxyBackend struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (f *proxyBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Clone(context.Background()))
	f.mu.Unlock()
	w.Header().Set("X-Fake", "headscale")
	fmt.Fprintf(w, "headscale %s %s", r.Method, r.URL.RequestURI())
}

func (f *proxyBackend) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return nil
	}
	return f.reqs[len(f.reqs)-1]
}

func proxyFront(t *testing.T, backend http.Handler) (*httptest.Server, *httptest.Server) {
	t.Helper()
	hs := httptest.NewServer(backend)
	t.Cleanup(hs.Close)
	target, _ := url.Parse(hs.URL)
	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Fake", "hub")
		fmt.Fprint(w, "hub")
	})
	front := httptest.NewServer(withHeadscale(hub, headscaleProxy(target, slog.New(slog.DiscardHandler))))
	t.Cleanup(front.Close)
	return front, hs
}

// TestHeadscaleProxyRoutes: exactly Headscale v0.29's public node endpoints
// reach it; its API, admin and interactive pages and the hub's own routes
// don't.
func TestHeadscaleProxyRoutes(t *testing.T) {
	fake := &proxyBackend{}
	front, _ := proxyFront(t, fake)
	cases := []struct {
		method, path string
		proxied      bool
	}{
		{"GET", "/key?v=130", true},
		{"POST", "/ts2021", true},
		{"GET", "/ts2021", true},
		{"GET", "/derp", true},
		{"GET", "/derp/probe", true},
		{"HEAD", "/derp/probe", true},
		{"GET", "/derp/latency-check", true},
		{"GET", "/bootstrap-dns?q=example.com", true},
		{"HEAD", "/machine/ping-response?id=abc", true},
		// Headscale's API, admin and interactive routes stay local
		{"GET", "/api/v1/apikey", false},
		{"POST", "/api/v1/preauthkey", false},
		{"GET", "/api/v1/node", false},
		{"GET", "/health", false},
		{"GET", "/version", false},
		{"GET", "/register/abc", false},
		{"GET", "/auth/abc", false},
		{"GET", "/oidc/callback", false},
		{"GET", "/swagger", false},
		{"GET", "/swagger/v1/openapiv2.json", false},
		{"POST", "/verify", false},
		{"GET", "/apple", false},
		{"GET", "/windows", false},
		{"GET", "/metrics", false},
		{"GET", "/machine/ping-response", false},
		{"POST", "/key", false},
		{"GET", "/key/", false},
		{"GET", "/ts2021x", false},
		{"GET", "/derp/x", false},
		// the hub's own
		{"GET", "/", false},
		{"GET", "/healthz", false},
		{"GET", "/ca.crt", false},
		{"GET", "/install.sh", false},
		{"POST", "/api/login", false},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, front.URL+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		resp.Body.Close()
		got := resp.Header.Get("X-Fake") == "headscale"
		if got != c.proxied {
			t.Errorf("%s %s: proxied=%v, want %v (status %s)", c.method, c.path, got, c.proxied, resp.Status)
		}
	}
	// the query reaches Headscale, the client's address in X-Forwarded-For,
	// the node's Host kept
	req, _ := http.NewRequest("GET", front.URL+"/key?v=130", nil)
	req.Host = "hub.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "headscale GET /key?v=130" {
		t.Fatalf("body %q", body)
	}
	r := fake.last()
	if r.Host != "hub.example.com" || !strings.HasPrefix(r.Header.Get("X-Forwarded-For"), "127.0.0.1") {
		t.Fatalf("host %q, X-Forwarded-For %q", r.Host, r.Header.Get("X-Forwarded-For"))
	}
}

// TestHeadscaleProxyDown: a 502, not the hub's page.
func TestHeadscaleProxyDown(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	target, _ := url.Parse("http://" + ln.Addr().String())
	ln.Close()
	front := httptest.NewServer(withHeadscale(http.NotFoundHandler(), headscaleProxy(target, slog.New(slog.DiscardHandler))))
	defer front.Close()
	resp, err := http.Get(front.URL + "/key?v=130")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %s", resp.Status)
	}
}

// TestHeadscaleProxyNoise: Tailscale's own control client completes the
// TS2021 Noise upgrade (HTTP Upgrade, the Noise handshake, then a byte stream
// both ways) through the proxy, over plain http (the :80
// listener) and over TLS (the hub's https).
func TestHeadscaleProxyNoise(t *testing.T) {
	serverKey := key.NewMachine()
	backend := http.NewServeMux()
	backend.HandleFunc("/ts2021", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Derp-Fast-Start") != "" {
			t.Errorf("Derp-Fast-Start forwarded")
		}
		conn, err := controlhttpserver.AcceptHTTP(r.Context(), w, r, serverKey, nil)
		if err != nil {
			t.Errorf("AcceptHTTP: %v", err)
			return
		}
		defer conn.Close()
		io.Copy(conn, conn) // echo
	})
	hs := httptest.NewServer(backend)
	defer hs.Close()
	target, _ := url.Parse(hs.URL)
	proxy := headscaleProxy(target, slog.New(slog.DiscardHandler))
	hubPage := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "hub", http.StatusTeapot) })

	plain := httptest.NewServer(withHeadscaleNoise(hubPage, proxy))
	defer plain.Close()
	tlsFront := httptest.NewUnstartedServer(withHeadscale(hubPage, proxy))
	tlsFront.EnableHTTP2 = true // like the hub's https: h2 offered, the Upgrade needs http/1.1
	tlsFront.StartTLS()
	defer tlsFront.Close()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	for _, c := range []struct {
		name            string
		httpPort, https string
		roots           *x509.CertPool
	}{
		{"http", port(plain.URL), controlhttp.NoPort, nil},
		{"https", fmt.Sprint(closedPort), port(tlsFront.URL), certPool(tlsFront)},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			d := &controlhttp.Dialer{
				Hostname: "127.0.0.1", HTTPPort: c.httpPort, HTTPSPort: c.https,
				MachineKey: key.NewMachine(), ControlKey: serverKey.Public(),
				ProtocolVersion: uint16(tailcfg.CurrentCapabilityVersion),
				ExtraRootCAs:    c.roots,
				Logf:            t.Logf,
			}
			conn, err := d.Dial(ctx)
			if err != nil {
				t.Fatalf("noise dial through the proxy: %v", err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			msg := []byte("ping over noise")
			if _, err := conn.Write(msg); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != string(msg) {
				t.Fatalf("echo %q %v", buf, err)
			}
		})
	}
}

// TestHeadscaleProxyDERP: two DERP clients (Tailscale's) connect to a DERP
// server through the proxy (Upgrade: DERP) and relay a packet.
func TestHeadscaleProxyDERP(t *testing.T) {
	srv := derpserver.New(key.NewNode(), t.Logf)
	defer srv.Close()
	backend := http.NewServeMux()
	backend.Handle("/derp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(derp.FastStartHeader) != "" {
			t.Errorf("%s forwarded", derp.FastStartHeader)
		}
		derpserver.Handler(srv).ServeHTTP(w, r)
	}))
	front, _ := proxyFront(t, backend)

	nm := netmon.NewStatic()
	client := func() (*derphttp.Client, key.NodePrivate) {
		k := key.NewNode()
		c, err := derphttp.NewClient(k, front.URL+"/derp", t.Logf, nm)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Connect(ctx); err != nil {
			t.Fatalf("DERP connect through the proxy: %v", err)
		}
		return c, k
	}
	a, _ := client()
	b, bk := client()
	got := make(chan string, 1)
	go func() {
		for {
			m, err := b.Recv()
			if err != nil {
				return
			}
			if p, ok := m.(derp.ReceivedPacket); ok {
				got <- string(p.Data)
				return
			}
		}
	}()
	deadline := time.After(10 * time.Second)
	for {
		if err := a.Send(bk.Public(), []byte("relayed")); err != nil {
			t.Fatal(err)
		}
		select {
		case s := <-got:
			if s != "relayed" {
				t.Fatalf("got %q", s)
			}
			return
		case <-time.After(200 * time.Millisecond): // b may not be registered yet
		case <-deadline:
			t.Fatal("no packet through DERP")
		}
	}
}

func port(u string) string {
	p, _ := url.Parse(u)
	return p.Port()
}

func certPool(s *httptest.Server) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(s.Certificate())
	return p
}
