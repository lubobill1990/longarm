package hubclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

func testCA(t *testing.T) *pki.CA {
	t.Helper()
	ca, err := pki.InitCA(t.TempDir(), "test hub CA", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// panel is a hub's web panel over HTTPS: /ca.crt answers caBody, the
// well-known document wk (404 when nil).
type panel struct {
	srv   *httptest.Server
	roots *x509.CertPool
	hits  atomic.Int32
}

func newPanel(t *testing.T, caBody []byte, wk *WellKnown) *panel {
	t.Helper()
	p := &panel{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ca.crt", func(w http.ResponseWriter, _ *http.Request) {
		p.hits.Add(1)
		w.Write(caBody)
	})
	mux.HandleFunc("GET "+WellKnownPath, func(w http.ResponseWriter, _ *http.Request) {
		p.hits.Add(1)
		if wk == nil {
			http.NotFound(w, nil)
			return
		}
		json.NewEncoder(w).Encode(wk)
	})
	p.srv = httptest.NewTLSServer(mux)
	t.Cleanup(p.srv.Close)
	p.roots = x509.NewCertPool()
	p.roots.AddCert(p.srv.Certificate())
	return p
}

func (p *panel) url() string { return p.srv.URL } // https://127.0.0.1:port

// grpcHub is a TLS listener presenting a hub certificate of ca with ca in
// the chain, as longarm-hub's gRPC port does; dial runs enrollTLS against
// it.
func grpcHub(t *testing.T, ca *pki.CA) string {
	t.Helper()
	certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{Role: pki.RoleHub, Name: "hub", Hosts: []string{"127.0.0.1"}, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(append(certPEM, ca.CertPEM...), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	return ln.Addr().String()
}

func dialEnroll(addr, fp string) error {
	var ca *x509.Certificate
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, enrollTLS(fp, &ca))
	if err != nil {
		return err
	}
	c.Close()
	return nil
}

// TestResolveHubBootstrap: a domain-only hub gets its CA from the panel's
// /ca.crt over WebPKI-verified HTTPS, and the gRPC address from the
// well-known document; the pin then guards the gRPC connection.
func TestResolveHubBootstrap(t *testing.T) {
	ca := testCA(t)
	fp := pki.Fingerprint(ca.Cert)
	hubAddr := grpcHub(t, ca)
	p := newPanel(t, ca.CertPEM, &WellKnown{Address: hubAddr, CAFingerprint: strings.ToUpper(fp)})
	ctx := context.Background()

	// the right CA
	tg, err := ResolveHub(ctx, p.url(), ResolveOptions{Roots: p.roots})
	if err != nil {
		t.Fatal(err)
	}
	if tg.Address != hubAddr || tg.Fingerprint != fp || !tg.FromHTTPS || tg.PanelURL != p.url() {
		t.Fatalf("resolved %+v", tg)
	}
	if err := dialEnroll(tg.Address, tg.Fingerprint); err != nil {
		t.Fatalf("enroll TLS with the bootstrapped pin: %v", err)
	}

	// no well-known document (an older hub): the default port
	p2 := newPanel(t, ca.CertPEM, nil)
	if tg, err := ResolveHub(ctx, p2.url(), ResolveOptions{Roots: p2.roots}); err != nil || tg.Address != "127.0.0.1:7443" || tg.Fingerprint != fp {
		t.Fatalf("without well-known: %+v %v", tg, err)
	}
	// an https:// URL's port is the panel's: the gRPC address still comes from the hub
	if tg, err := ResolveHub(ctx, p.url()+"/", ResolveOptions{Roots: p.roots}); err != nil || tg.Address != hubAddr {
		t.Fatalf("trailing slash: %+v %v", tg, err)
	}
}

// TestResolveHubWrongCA: a panel whose HTTPS certificate isn't trusted,
// a panel contradicting itself, and a gRPC hub of another CA than the
// panel's all fail.
func TestResolveHubWrongCA(t *testing.T) {
	ca, other := testCA(t), testCA(t)
	ctx := context.Background()
	p := newPanel(t, ca.CertPEM, nil)

	// HTTPS certificate not from a trusted root (here: an empty pool)
	_, err := ResolveHub(ctx, p.url(), ResolveOptions{Roots: x509.NewCertPool()})
	if !errors.Is(err, ErrNoBootstrap) || !strings.Contains(err.Error(), "--ca-fingerprint") {
		t.Fatalf("untrusted panel: %v", err)
	}

	// /ca.crt and the well-known document disagree
	p2 := newPanel(t, ca.CertPEM, &WellKnown{Address: "127.0.0.1:1", CAFingerprint: pki.Fingerprint(other.Cert)})
	if _, err := ResolveHub(ctx, p2.url(), ResolveOptions{Roots: p2.roots}); !errors.Is(err, ErrNoBootstrap) {
		t.Fatalf("inconsistent panel: %v", err)
	}

	// the panel's CA is not the one the gRPC hub presents
	tg, err := ResolveHub(ctx, p.url(), ResolveOptions{Roots: p.roots})
	if err != nil {
		t.Fatal(err)
	}
	if err := dialEnroll(grpcHub(t, other), tg.Fingerprint); err == nil || !strings.Contains(err.Error(), "does not match the pinned") {
		t.Fatalf("gRPC hub of another CA: %v", err)
	}
}

// TestResolveHubNotPEM: /ca.crt answering something else (an HTML page,
// a certificate that isn't a CA) fails.
func TestResolveHubNotPEM(t *testing.T) {
	ctx := context.Background()
	p := newPanel(t, []byte("<!doctype html><title>login</title>"), nil)
	if _, err := ResolveHub(ctx, p.url(), ResolveOptions{Roots: p.roots}); !errors.Is(err, ErrNoBootstrap) || !strings.Contains(err.Error(), "not a PEM certificate") {
		t.Fatalf("html: %v", err)
	}
	ca := testCA(t)
	leaf, _, err := ca.Issue(pki.IssueOptions{Role: pki.RoleHub, Name: "hub", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	p = newPanel(t, leaf, nil)
	if _, err := ResolveHub(ctx, p.url(), ResolveOptions{Roots: p.roots}); !errors.Is(err, ErrNoBootstrap) || !strings.Contains(err.Error(), "not a CA") {
		t.Fatalf("leaf: %v", err)
	}
}

// TestResolveHubNoTLS: plain HTTP never vouches for a CA - neither a
// panel speaking HTTP on the https port, a redirect to http://, nor an
// http:// hub.
func TestResolveHubNoTLS(t *testing.T) {
	ctx := context.Background()
	ca := testCA(t)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(ca.CertPEM) }))
	defer plain.Close()
	https := "https://" + strings.TrimPrefix(plain.URL, "http://")
	if _, err := ResolveHub(ctx, https, ResolveOptions{}); !errors.Is(err, ErrNoBootstrap) {
		t.Fatalf("HTTP on the https URL: %v", err)
	}
	if _, err := ResolveHub(ctx, plain.URL, ResolveOptions{}); !errors.Is(err, ErrNoBootstrap) || !strings.Contains(err.Error(), "not an https:// URL") {
		t.Fatalf("http:// hub: %v", err)
	}
	redir := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
	}))
	defer redir.Close()
	roots := x509.NewCertPool()
	roots.AddCert(redir.Certificate())
	if _, err := ResolveHub(ctx, redir.URL, ResolveOptions{Roots: roots}); !errors.Is(err, ErrNoBootstrap) || !strings.Contains(err.Error(), "refusing a redirect") {
		t.Fatalf("redirect to http: %v", err)
	}
}

// TestResolveHubExplicitFingerprint: --ca-fingerprint wins - nothing is
// fetched - and is still verified by the gRPC connection.
func TestResolveHubExplicitFingerprint(t *testing.T) {
	ca, other := testCA(t), testCA(t)
	p := newPanel(t, ca.CertPEM, nil)
	tg, err := ResolveHub(context.Background(), p.url(), ResolveOptions{Roots: p.roots, Fingerprint: pki.Fingerprint(other.Cert)})
	if err != nil {
		t.Fatal(err)
	}
	if p.hits.Load() != 0 || tg.FromHTTPS || tg.Fingerprint != pki.Fingerprint(other.Cert) || tg.Address != "127.0.0.1:7443" {
		t.Fatalf("explicit: %+v, %d requests", tg, p.hits.Load())
	}
	addr := grpcHub(t, ca)
	if err := dialEnroll(addr, tg.Fingerprint); err == nil || !strings.Contains(err.Error(), "does not match the pinned") {
		t.Fatalf("mismatching explicit pin: %v", err)
	}
	if err := dialEnroll(addr, pki.Fingerprint(ca.Cert)); err != nil {
		t.Fatalf("matching explicit pin: %v", err)
	}
	// without a fingerprint, a private hub fails clearly
	if _, err := ResolveHub(context.Background(), "127.0.0.1:1", ResolveOptions{Timeout: 5 * time.Second}); !errors.Is(err, ErrNoBootstrap) ||
		!strings.Contains(err.Error(), "--ca-fingerprint") {
		t.Fatalf("private hub: %v", err)
	}
}

func TestParseHub(t *testing.T) {
	for _, c := range []struct{ in, host, port, panel string }{
		{"longarm.example.com", "longarm.example.com", "", "https://longarm.example.com"},
		{" longarm.example.com/ ", "longarm.example.com", "", "https://longarm.example.com"},
		{"https://longarm.example.com", "longarm.example.com", "", "https://longarm.example.com"},
		{"https://longarm.example.com/link", "longarm.example.com", "", "https://longarm.example.com"},
		{"HTTPS://longarm.example.com:8443", "longarm.example.com", "", "https://longarm.example.com:8443"},
		{"longarm.example.com:7444", "longarm.example.com", "7444", "https://longarm.example.com"},
		{"grpcs://longarm.example.com:7443", "longarm.example.com", "7443", "https://longarm.example.com"},
		{"http://hub.lan", "hub.lan", "", ""},
		{"[::1]:7443", "::1", "7443", "https://[::1]"},
		{"10.0.0.5", "10.0.0.5", "", "https://10.0.0.5"},
	} {
		h, p, u, err := parseHub(c.in)
		if err != nil || h != c.host || p != c.port || u != c.panel {
			t.Errorf("parseHub(%q) = %q %q %q %v", c.in, h, p, u, err)
		}
	}
	for _, bad := range []string{"", "host:0", "host:x", "user@host", "https://"} {
		if _, _, _, err := parseHub(bad); err == nil {
			t.Errorf("parseHub(%q) accepted", bad)
		}
	}
}
