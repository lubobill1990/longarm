package hubclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

// Signing in with nothing but the hub's domain (docs/security.md "First
// contact"): the hub's gRPC port presents a certificate of its own CA,
// which no system trusts, so the CA must be pinned before the first
// enrollment. With an explicit fingerprint (--ca-fingerprint) that pin is
// given; without, it is bootstrapped from the hub's web panel, which has
// a WebPKI certificate (Let's Encrypt): GET https://<host>/ca.crt,
// verified with the system's roots exactly like a browser opening the
// panel. The CA's fingerprint is then pinned for the gRPC connection as
// if it had been passed. Trust rests on DNS and the WebPKI for the first
// contact - the same trust as opening the panel to approve the sign-in.

// WellKnownPath is the hub's public description of itself: the gRPC
// address nodes dial and the CA fingerprint (JSON, WellKnown).
const WellKnownPath = "/.well-known/longarm-hub"

// WellKnown is what WellKnownPath serves.
type WellKnown struct {
	Address       string `json:"address"`        // gRPC "host:port"
	CAFingerprint string `json:"ca_fingerprint"` // sha256 of the CA's DER, hex
	Name          string `json:"name,omitempty"` // the hub's name (its certificate)
}

// HubTarget is where to sign in: the hub's gRPC address and the CA
// fingerprint to pin.
type HubTarget struct {
	Address     string // gRPC "host:port"
	Fingerprint string // pinned CA fingerprint (normalized)
	// PanelURL the CA was fetched from ("https://host"); "" when the
	// fingerprint was given.
	PanelURL string
	// FromHTTPS: the fingerprint was bootstrapped over HTTPS (else it was
	// given explicitly).
	FromHTTPS bool
}

// ResolveOptions for ResolveHub.
type ResolveOptions struct {
	// Fingerprint given explicitly (--ca-fingerprint): it wins, nothing is
	// fetched (the gRPC connection verifies it).
	Fingerprint string
	// Roots verify the panel's HTTPS certificate; nil: the system's
	// (tests inject theirs).
	Roots *x509.CertPool
	// Timeout of the HTTPS requests (default 20 s).
	Timeout time.Duration
}

// ErrNoBootstrap wraps a failed HTTPS bootstrap of the hub's CA.
var ErrNoBootstrap = errors.New("can't get the hub's CA over HTTPS")

// ResolveHub turns a hub as the user typed it - "longarm.example.com",
// "https://longarm.example.com", "host:port" (the gRPC port), "[::1]:7443"
// - into the gRPC address and the CA fingerprint to pin.
//
// Without o.Fingerprint, the CA is fetched from https://<host>/ca.crt
// (https://<host[:port]>/ca.crt for an https:// URL: that port is the
// panel's) with o.Roots, and the gRPC address, when no port was given,
// from WellKnownPath (default port 7443 when the hub has none). Errors
// wrap ErrNoBootstrap and ask for --ca-fingerprint.
func ResolveHub(ctx context.Context, hub string, o ResolveOptions) (*HubTarget, error) {
	host, port, panel, err := parseHub(hub)
	if err != nil {
		return nil, err
	}
	addr := ""
	if port != "" {
		addr = net.JoinHostPort(host, port)
	}
	if fp := pki.NormalizeFingerprint(o.Fingerprint); fp != "" {
		if addr == "" {
			addr = net.JoinHostPort(host, strconv.Itoa(DefaultPort))
		}
		return &HubTarget{Address: addr, Fingerprint: fp}, nil
	}
	if panel == "" {
		return nil, fmt.Errorf("%w: %s is not an https:// URL; pass --ca-fingerprint (longarm-hub ca show)", ErrNoBootstrap, hub)
	}
	hc := bootstrapClient(o)
	ca, err := fetchCA(ctx, hc, panel)
	if err != nil {
		return nil, fmt.Errorf("%w from %s/ca.crt: %v\n(a hub without a public HTTPS panel: pass --ca-fingerprint, printed by longarm-hub ca show)", ErrNoBootstrap, panel, err)
	}
	t := &HubTarget{Address: addr, Fingerprint: pki.Fingerprint(ca), PanelURL: panel, FromHTTPS: true}
	if wk, err := fetchWellKnown(ctx, hc, panel); err == nil {
		if wk.CAFingerprint != "" && pki.NormalizeFingerprint(wk.CAFingerprint) != t.Fingerprint {
			return nil, fmt.Errorf("%w: %s names CA %s but serves %s at /ca.crt", ErrNoBootstrap, panel, wk.CAFingerprint, t.Fingerprint)
		}
		if t.Address == "" && validAddr(wk.Address) {
			t.Address = wk.Address
		}
	}
	if t.Address == "" {
		t.Address = net.JoinHostPort(host, strconv.Itoa(DefaultPort))
	}
	return t, nil
}

// parseHub splits a hub reference: host, the gRPC port if given, and the
// panel's https base URL ("" for plain http, which can't vouch for a CA).
func parseHub(hub string) (host, port, panel string, err error) {
	s := strings.TrimSpace(hub)
	if s == "" {
		return "", "", "", errors.New("no hub given")
	}
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"):
		u, perr := url.Parse(s)
		if perr != nil || u.Hostname() == "" {
			return "", "", "", fmt.Errorf("hub %q: not a URL", hub)
		}
		host = u.Hostname()
		if u.Scheme == "https" {
			panel = "https://" + u.Host // u.Host: the panel's port, if any
		}
		return host, "", panel, nil
	case strings.HasPrefix(lower, "grpcs://"):
		s = s[len("grpcs://"):]
	}
	s = strings.TrimSuffix(s, "/")
	if h, p, serr := net.SplitHostPort(s); serr == nil {
		if n, perr := strconv.Atoi(p); perr != nil || n <= 0 || n > 65535 {
			return "", "", "", fmt.Errorf("hub %q: bad port", hub)
		}
		host, port = h, p
	} else {
		host = strings.Trim(s, "[]")
	}
	if host == "" || strings.ContainsAny(host, "/?#@ ") {
		return "", "", "", fmt.Errorf("hub %q: not a host name", hub)
	}
	return host, port, "https://" + bracket(host), nil
}

func bracket(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func validAddr(a string) bool {
	h, p, err := net.SplitHostPort(a)
	n, perr := strconv.Atoi(p)
	return err == nil && h != "" && perr == nil && n > 0 && n <= 65535
}

func bootstrapClient(o ResolveOptions) *http.Client {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: o.Roots},
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{Transport: tr, Timeout: timeout,
		// HTTPS on the same host only: the answer must come from the
		// name the user gave.
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if r.URL.Scheme != "https" || !strings.EqualFold(r.URL.Hostname(), via[0].URL.Hostname()) {
				return fmt.Errorf("refusing a redirect to %s", r.URL.Redacted())
			}
			return nil
		}}
}

func get(ctx context.Context, hc *http.Client, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %s", u, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: answer too large", u)
	}
	return b, nil
}

// fetchCA gets panel/ca.crt: one PEM CA certificate.
func fetchCA(ctx context.Context, hc *http.Client, panel string) (*x509.Certificate, error) {
	b, err := get(ctx, hc, panel+"/ca.crt", 64<<10)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("the answer is not a PEM certificate (is this a longarm hub?)")
	}
	ca, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the certificate: %w", err)
	}
	if !ca.IsCA {
		return nil, errors.New("the certificate is not a CA")
	}
	return ca, nil
}

func fetchWellKnown(ctx context.Context, hc *http.Client, panel string) (*WellKnown, error) {
	b, err := get(ctx, hc, panel+WellKnownPath, 16<<10)
	if err != nil {
		return nil, err
	}
	var wk WellKnown
	if err := json.Unmarshal(b, &wk); err != nil {
		return nil, err
	}
	return &wk, nil
}
