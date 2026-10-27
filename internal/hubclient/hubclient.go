// Package hubclient is the node side of longarm-hub (longarm and longarmd): connecting to
// the hub's gRPC API, enrolling with a join code, waiting for approval,
// renewing certificates, and the hub.toml file that records where the hub
// is.
package hubclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/telemetry/tgrpc"
	"github.com/lubobill1990/longarm/internal/transport"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// DefaultPort of longarm-hub.
const DefaultPort = 7443

// File is the name of the hub settings file (in longarm's config dir or longarmd's
// data dir), written by `longarm join` / `longarmd join`.
const File = "hub.toml"

// Config is hub.toml (also the [hub] section of workers.toml / longarmd.toml).
type Config struct {
	// Address of the hub's gRPC API, "host:port" (default port 7443).
	Address string `toml:"address"`
	// PollToken of a pending enrollment (longarmd join --no-wait).
	PollToken string `toml:"poll_token,omitempty"`
	// Disabled turns the hub off without forgetting it.
	Disabled bool `toml:"disabled,omitempty"`
	// Owner (workers): the user whose worker code enrolled it. Without its
	// worker-access.json such a worker serves only the owner until the
	// hub says more - never everybody.
	Owner string `toml:"owner,omitempty"`
	// SignIn (workers): an installer set this hub up for a sign-in to come
	// (the MSI with LOGIN=1): longarmd waits for its certificate instead of
	// failing, and `longarmd login` without a hub signs in to Address.
	SignIn bool `toml:"sign_in,omitempty"`
	// CAFingerprint the installer was given for that sign-in ("": fetched
	// over HTTPS then).
	CAFingerprint string `toml:"ca_fingerprint,omitempty"`
	// Tailnet is the hub's tailnet this node is enrolled in (Tailscale or
	// Headscale).
	Tailnet TailnetConfig `toml:"tailnet,omitempty"`
}

// TailnetConfig is the [tailnet] section of hub.toml (DESIGN §4.2, §4.4).
type TailnetConfig struct {
	Provider   string `toml:"provider,omitempty"` // tailscale | headscale ("" = before providers)
	ControlURL string `toml:"control_url,omitempty"`
	Hostname   string `toml:"hostname,omitempty"`
}

// TSNetBase is the base name of the embedded tailscale node's state dirs:
// "tsnet" (no control URL) and "tsnet-<control host>" per control server.
const TSNetBase = "tsnet"

// TailnetStateDir is the tsnet state dir for control server control in
// dir (longarm's config dir / longarmd's data dir).
func TailnetStateDir(dir, control string) string {
	return transport.StateDirFor(filepath.Join(dir, TSNetBase), control)
}

// TailnetKeyFile is where a minted key for control waits (next to its
// state dir) until the embedded node used it.
func TailnetKeyFile(dir, control string) string { return TailnetStateDir(dir, control) + ".authkey" }

// SaveTailnet stores a minted tailnet key in dir and records the tailnet
// in c (the caller saves hub.toml).
func SaveTailnet(dir string, c *Config, t *pb.TailnetAuth) error {
	if t == nil || t.AuthKey == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(TailnetKeyFile(dir, t.ControlUrl), []byte(t.AuthKey), 0o600); err != nil {
		return err
	}
	c.Tailnet = TailnetConfig{Provider: t.Provider, ControlURL: t.ControlUrl, Hostname: t.Hostname}
	return nil
}

// RemoveTailnetState removes every tsnet state dir and waiting key in dir.
func RemoveTailnetState(dir string) {
	base := filepath.Join(dir, TSNetBase)
	m, _ := filepath.Glob(base + "-*")
	for _, p := range append(m, base, base+".authkey") {
		os.RemoveAll(p)
	}
}

// FetchTailnetKey asks the hub for a key for this node's embedded
// tailscale node (Fleet.TailnetKey); reason is logged by the hub.
func FetchTailnetKey(ctx context.Context, addr string, b *pki.Bundle, reason string) (*pb.TailnetAuth, error) {
	conn, err := Dial(addr, b)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	t, err := conn.Fleet().TailnetKey(cctx, &pb.TailnetKeyRequest{Reason: reason})
	if err != nil {
		return nil, err
	}
	if t.AuthKey == "" || t.ControlUrl == "" {
		return nil, errors.New("the hub sent no tailnet key")
	}
	return t, nil
}

// Enabled reports whether a hub is configured.
func (c Config) Enabled() bool { return c.Address != "" && !c.Disabled }

// LoadFile reads dir/hub.toml (missing = zero Config).
func LoadFile(dir string) (Config, error) {
	var c Config
	_, err := toml.DecodeFile(filepath.Join(dir, File), &c)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("%s: %w", filepath.Join(dir, File), err)
	}
	return c, nil
}

// SaveFile writes dir/hub.toml.
func SaveFile(dir string, c Config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# longarm-hub settings (written by join)\n")
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return err
	}
	// Atomic: longarmd and longarm rewrite it at runtime (tailnet switches).
	tmp := filepath.Join(dir, fmt.Sprintf("%s.%d.tmp", File, os.Getpid()))
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if err := platform.ReplaceFile(tmp, filepath.Join(dir, File)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// NormalizeAddr adds the default port.
func NormalizeAddr(a string) string {
	a = strings.TrimSpace(a)
	a = strings.TrimPrefix(strings.TrimPrefix(a, "https://"), "grpcs://")
	a = strings.TrimSuffix(a, "/")
	if _, _, err := net.SplitHostPort(a); err != nil {
		return net.JoinHostPort(strings.Trim(a, "[]"), strconv.Itoa(DefaultPort))
	}
	return a
}

// verifyHub checks a hub certificate chain against roots: chain, server
// usage and the hub role.
func verifyHub(certs []*x509.Certificate, roots *x509.CertPool) error {
	if len(certs) == 0 {
		return errors.New("the hub presented no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return fmt.Errorf("hub certificate: %w", err)
	}
	id, err := pki.IdentityFromCert(certs[0])
	if err != nil {
		return err
	}
	if id.Role != pki.RoleHub {
		return fmt.Errorf("%s is not a hub", id)
	}
	return nil
}

// TLSConfig is a node's TLS config towards the hub: its own (live)
// certificate, and the hub verified against the bundle's CA.
func TLSConfig(b *pki.Bundle) *tls.Config {
	return &tls.Config{
		MinVersion:           tls.VersionTLS13,
		GetClientCertificate: b.GetClientCertificate,
		NextProtos:           []string{"h2"},
		InsecureSkipVerify:   true, // replaced by VerifyConnection (chain + hub role; hubs are reached by IP)
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyHub(cs.PeerCertificates, b.Pool)
		},
	}
}

// Conn is a connection to the hub.
type Conn struct {
	*grpc.ClientConn
	Addr string
}

func (c *Conn) Fleet() pb.FleetClient    { return pb.NewFleetClient(c.ClientConn) }
func (c *Conn) Node() pb.HubNodeClient   { return pb.NewHubNodeClient(c.ClientConn) }
func (c *Conn) Admin() pb.HubAdminClient { return pb.NewHubAdminClient(c.ClientConn) }
func (c *Conn) Enroll() pb.EnrollClient  { return pb.NewEnrollClient(c.ClientConn) }

// WaitReady starts connecting (if idle) and waits up to timeout for the
// connection to be ready. Callers for whom the hub is optional use a short
// timeout so an unreachable hub costs little.
func (c *Conn) WaitReady(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c.Connect()
	for {
		st := c.GetState()
		if st == connectivity.Ready {
			return nil
		}
		if st == connectivity.Shutdown {
			return errors.New("hub connection closed")
		}
		if st == connectivity.TransientFailure { // refused, TLS error ...: no need to wait
			return fmt.Errorf("hub %s unreachable (connection failed)", c.Addr)
		}
		if !c.WaitForStateChange(ctx, st) {
			return fmt.Errorf("hub %s not reachable within %s (%s)", c.Addr, timeout, strings.ToLower(st.String()))
		}
	}
}

func dial(addr string, tc *tls.Config) (*Conn, error) {
	bo := backoff.DefaultConfig
	bo.MaxDelay = 5 * time.Second
	d := net.Dialer{Timeout: 5 * time.Second}
	cc, err := grpc.NewClient("passthrough:///"+addr, append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, a string) (net.Conn, error) { return d.DialContext(ctx, "tcp", a) }),
		grpc.WithTransportCredentials(credentials.NewTLS(tc)),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: bo, MinConnectTimeout: 5 * time.Second}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 20 * time.Second}),
	}, tgrpc.DialOptions("hub")...)...)
	if err != nil {
		return nil, err
	}
	return &Conn{ClientConn: cc, Addr: addr}, nil
}

// Dial connects (lazily) to the hub with the node's bundle.
func Dial(addr string, b *pki.Bundle) (*Conn, error) {
	return dial(NormalizeAddr(addr), TLSConfig(b))
}

// ---------------------------------------------------------------- enroll

// JoinOptions for Join.
type JoinOptions struct {
	Name string
	// Fingerprint pins the hub's CA (sha256 of its DER, hex); empty =
	// trust on first use (the fingerprint is reported in the result).
	Fingerprint string
	// Trust (optional): the node key whose binding the certificate carries
	// (end-to-end trust, pki/trust.go).
	Trust *pki.CSRTrust
}

// JoinResult is what Join and Poll return.
type JoinResult struct {
	Resp          *pb.JoinResponse
	KeyPEM        []byte
	CAFingerprint string
	Pinned        bool // the CA matched the given fingerprint
	// Worker: a kind "both" pairing's worker enrollment (Resp, KeyPEM
	// above are the client's)
	Worker *JoinResult
}

// enrollTLS verifies the hub without a CA yet: the hub sends its CA in the
// chain; it must match the pinned fingerprint (if any) and sign the hub's
// certificate. caOut receives the CA.
func enrollTLS(fingerprint string, caOut **x509.Certificate) *tls.Config {
	fp := pki.NormalizeFingerprint(fingerprint)
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"h2"},
		InsecureSkipVerify: true, // replaced by VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			certs := cs.PeerCertificates
			if len(certs) < 2 {
				return errors.New("the hub did not send its CA certificate")
			}
			ca := certs[len(certs)-1]
			if !ca.IsCA {
				return errors.New("the hub's chain does not end in a CA")
			}
			if fp != "" && pki.Fingerprint(ca) != fp {
				return fmt.Errorf("hub CA fingerprint %s does not match the pinned %s", pki.Fingerprint(ca), fp)
			}
			pool := x509.NewCertPool()
			pool.AddCert(ca)
			if err := verifyHub(certs[:len(certs)-1], pool); err != nil {
				return err
			}
			*caOut = ca
			return nil
		},
	}
}

// Join redeems a join code: it creates the node key and a CSR and asks the
// hub for a certificate (or a pending enrollment).
func Join(ctx context.Context, addr, code string, o JoinOptions) (*JoinResult, error) {
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(o.Name, o.Trust)
	if err != nil {
		return nil, err
	}
	var ca *x509.Certificate
	c, err := dial(NormalizeAddr(addr), enrollTLS(o.Fingerprint, &ca))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	host, _ := os.Hostname()
	resp, err := c.Enroll().Join(ctx, &pb.JoinRequest{Code: code, CsrPem: csrPEM, Name: o.Name, Hostname: host,
		Os: runtime.GOOS, Arch: runtime.GOARCH, Version: version.Version})
	if err != nil {
		return nil, err
	}
	return checkResult(resp, ca, keyPEM, o.Fingerprint)
}

// ErrNoEntra: the hub has no Microsoft Entra sign-in.
var ErrNoEntra = errors.New("this hub has no Microsoft Entra sign-in (enroll with a join code: longarm join)")

// LoginOptions for Login: like JoinOptions, plus where the access token
// comes from.
type LoginOptions struct {
	JoinOptions
	// Token returns an access token for the hub's Entra settings (the
	// tenant and the scope to request).
	Token func(ctx context.Context, info *pb.AuthInfoResponse) (string, error)
	// Kind: "" (a device of the user) or "worker" (a worker the user owns).
	Kind string
}

// Login enrolls with a Microsoft Entra access token instead of a join
// code (Enroll.EntraJoin): the hub's sign-in settings are asked for over
// the same verified connection the token then goes to.
func Login(ctx context.Context, addr string, o LoginOptions) (*JoinResult, error) {
	var ca *x509.Certificate
	c, err := dial(NormalizeAddr(addr), enrollTLS(o.Fingerprint, &ca))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	info, err := c.Enroll().AuthInfo(ctx, &pb.AuthInfoRequest{})
	if status.Code(err) == codes.Unimplemented {
		return nil, errors.New("this hub is too old for longarm login (update it, or use longarm join)")
	}
	if err != nil {
		return nil, err
	}
	if info.EntraScope == "" || info.EntraTenantId == "" {
		return nil, ErrNoEntra
	}
	tok, err := o.Token(ctx, info)
	if err != nil {
		return nil, err
	}
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(o.Name, o.Trust)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	resp, err := c.Enroll().EntraJoin(ctx, &pb.EntraJoinRequest{AccessToken: tok, CsrPem: csrPEM, Name: o.Name, Hostname: host,
		Os: runtime.GOOS, Arch: runtime.GOARCH, Version: version.Version, Kind: o.Kind})
	if err != nil {
		return nil, err
	}
	return checkResult(resp, ca, keyPEM, o.Fingerprint)
}

// LinkOptions for Link: like JoinOptions, plus what to enroll as and where
// the code goes.
type LinkOptions struct {
	JoinOptions        // kind both: the client's (Fingerprint pins both)
	Kind        string // "client", "worker" or "both"
	// Worker: kind both, the worker's name and trust binding (its own key;
	// Fingerprint unused).
	Worker JoinOptions
	// Show is called once with the code and the URL to enter it at.
	Show func(*pb.StartLinkResponse)
}

var (
	// ErrNoBothLink: the hub predates kind "both" pairings (one code for a
	// client and a worker); pair the two separately.
	ErrNoBothLink  = errors.New("this hub can't pair a client and a worker with one code (update it); sign in the two separately")
	ErrLinkDenied  = errors.New("the pairing was denied")
	ErrLinkExpired = errors.New("the code expired before it was approved; run login again")
)

// Link enrolls through the hub's pairing flow (Enroll.StartLink): the
// user approves the code Show displays in the hub's /link page; Link
// polls until then, a denial or the code's expiry.
func Link(ctx context.Context, addr string, o LinkOptions) (*JoinResult, error) {
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(o.Name, o.Trust)
	if err != nil {
		return nil, err
	}
	var wkeyPEM, wcsrPEM []byte
	if o.Kind == "both" {
		if wkeyPEM, wcsrPEM, err = pki.NewKeyAndCSR(o.Worker.Name, o.Worker.Trust); err != nil {
			return nil, err
		}
	}
	var ca *x509.Certificate
	c, err := dial(NormalizeAddr(addr), enrollTLS(o.Fingerprint, &ca))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	host, _ := os.Hostname()
	st, err := c.Enroll().StartLink(ctx, &pb.StartLinkRequest{Kind: o.Kind, CsrPem: csrPEM, Name: o.Name, Hostname: host,
		Os: runtime.GOOS, Arch: runtime.GOARCH, Version: version.Version, WorkerCsrPem: wcsrPEM, WorkerName: o.Worker.Name})
	if o.Kind == "both" && status.Code(err) == codes.InvalidArgument && strings.Contains(status.Convert(err).Message(), "want client or worker") {
		return nil, ErrNoBothLink
	}
	if status.Code(err) == codes.Unimplemented {
		return nil, errors.New("this hub is too old for signing in through the browser (update it, or use a join code)")
	}
	if err != nil {
		return nil, err
	}
	if o.Show != nil {
		o.Show(st)
	}
	interval := max(st.Interval.AsDuration(), time.Second)
	pctx, cancel := context.WithDeadline(ctx, st.ExpiresAt.AsTime().Add(30*time.Second))
	defer cancel()
	for {
		select {
		case <-pctx.Done():
			if ctx.Err() == nil {
				return nil, ErrLinkExpired
			}
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		resp, err := c.Enroll().PollLink(pctx, &pb.PollLinkRequest{PollToken: st.PollToken})
		switch status.Code(err) {
		case codes.OK:
		case codes.ResourceExhausted: // slow down
			interval += 5 * time.Second
			continue
		case codes.NotFound: // forgotten: long expired, or the hub restarted
			return nil, ErrLinkExpired
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
			continue // the hub is back soon, or pctx ends the loop
		default:
			return nil, err
		}
		switch resp.State {
		case "approved":
			r, err := checkResult(resp, ca, keyPEM, o.Fingerprint)
			if err != nil || o.Kind != "both" {
				return r, err
			}
			if resp.Worker == nil || resp.Worker.State != "approved" {
				return nil, errors.New("the hub approved the device but sent no worker enrollment")
			}
			if len(resp.Worker.CaPem) == 0 {
				resp.Worker.CaPem = resp.CaPem
			}
			if r.Worker, err = checkResult(resp.Worker, ca, wkeyPEM, o.Fingerprint); err != nil {
				return nil, err
			}
			return r, nil
		case "denied":
			if resp.Message != "" && resp.Message != ErrLinkDenied.Error() {
				return nil, fmt.Errorf("%w: %s", ErrLinkDenied, resp.Message)
			}
			return nil, ErrLinkDenied
		case "expired":
			return nil, ErrLinkExpired
		}
	}
}

func checkResult(resp *pb.JoinResponse, ca *x509.Certificate, keyPEM []byte, fp string) (*JoinResult, error) {
	got, err := pki.ParseCertPEM(resp.CaPem)
	if err != nil || ca == nil || !got.Equal(ca) {
		return nil, errors.New("the hub returned a CA that differs from the one it presented")
	}
	r := &JoinResult{Resp: resp, KeyPEM: keyPEM, CAFingerprint: pki.Fingerprint(ca), Pinned: fp != ""}
	if resp.State == "approved" {
		if _, err := tls.X509KeyPair(resp.CertPem, keyPEM); err != nil {
			return nil, fmt.Errorf("the issued certificate does not match our key: %w", err)
		}
	}
	return r, nil
}

// Poll asks about a pending enrollment; caPEM is the CA learned at Join.
func Poll(ctx context.Context, addr, token string, caPEM, keyPEM []byte) (*JoinResult, error) {
	ca, err := pki.ParseCertPEM(caPEM)
	if err != nil {
		return nil, err
	}
	var seen *x509.Certificate
	c, err := dial(NormalizeAddr(addr), enrollTLS(pki.Fingerprint(ca), &seen))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	resp, err := c.Enroll().JoinStatus(ctx, &pb.JoinStatusRequest{PollToken: token})
	if err != nil {
		return nil, err
	}
	return checkResult(resp, seen, keyPEM, pki.Fingerprint(ca))
}

// WaitApproved polls every interval until the enrollment is approved
// (returns the result), rejected (error) or ctx ends.
func WaitApproved(ctx context.Context, addr, token string, caPEM, keyPEM []byte, interval time.Duration,
	onPending func(*pb.JoinResponse)) (*JoinResult, error) {
	for {
		r, err := Poll(ctx, addr, token, caPEM, keyPEM)
		if err == nil {
			switch r.Resp.State {
			case "approved":
				return r, nil
			case "rejected":
				return nil, errors.New("the enrollment was rejected by an admin")
			}
			if onPending != nil {
				onPending(r.Resp)
			}
		} else if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// Renew obtains a fresh certificate for a new key over an authenticated
// connection and returns (certPEM, keyPEM, caPEM); t (optional) is the
// node's end-to-end trust material for the new certificate.
func Renew(ctx context.Context, c *Conn, name string, t *pki.CSRTrust) ([]byte, []byte, []byte, error) {
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(name, t)
	if err != nil {
		return nil, nil, nil, err
	}
	resp, err := c.Node().Renew(ctx, &pb.RenewRequest{CsrPem: csrPEM})
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := tls.X509KeyPair(resp.CertPem, keyPEM); err != nil {
		return nil, nil, nil, fmt.Errorf("renewed certificate does not match the key: %w", err)
	}
	return resp.CertPem, keyPEM, resp.CaPem, nil
}

// NeedsRenewal reports whether cert expires within renewBefore.
func NeedsRenewal(cert *x509.Certificate, renewBefore time.Duration) bool {
	return cert != nil && renewBefore > 0 && time.Until(cert.NotAfter) < renewBefore
}

// PendingKeyFile holds the node key while an enrollment awaits approval.
const PendingKeyFile = "pending.key"
