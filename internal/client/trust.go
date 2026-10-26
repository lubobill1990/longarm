package client

// End-to-end trust on the client (docs/security.md "End-to-end trust"):
// with require_workers in <config>/trust/trust.toml, a worker must present
// a certificate whose node key is endorsed by a user key this client trusts
// (its own, or one added with `longarm trust add-user`). The check runs in
// every mTLS handshake with a worker (pki.Bundle.Trust); this file turns a
// refusal into an error that says what to do about it.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/trust"
)

// TrustDir is the client's trust directory (node key, endorsement, user
// key, trust.toml) in the config dir.
const TrustDir = "trust"

// TrustStore is the trust directory of the config dir.
func TrustStore(dir string) trust.Store { return trust.Store{Dir: filepath.Join(dir, TrustDir)} }

// trustMarker is in every TrustError message; TrustRefusal finds it again
// after gRPC flattened the error into a status message.
const trustMarker = "refused by end-to-end trust"

// pkiTrustPrefix starts pki.Bundle.ClientConfig's refusals (also seen
// through the longnet transport's inner handshake, where the error type is
// lost).
const pkiTrustPrefix = "end-to-end trust: "

// TrustError is a worker refused because it lacks a trusted endorsement.
type TrustError struct {
	Worker string
	Reason string
}

func (e *TrustError) Error() string {
	return fmt.Sprintf("worker %s %s: %s. If it is yours: run `longarmd trust request` on the worker, "+
		"endorse the request here with `longarm trust endorse <request>` and install the endorsement there with "+
		"`longarmd trust accept <endorsement>`; or trust the user who endorsed it (`longarm trust add-user <user-key>`)",
		e.Worker, trustMarker, strings.TrimSuffix(e.Reason, "."))
}

// trustRefusal classifies a leg's error (nil: not a trust refusal).
func trustRefusal(w Worker, err error) *TrustError {
	var te *TrustError
	if errors.As(err, &te) {
		return te
	}
	if _, reason, ok := strings.Cut(err.Error(), pkiTrustPrefix); ok {
		return &TrustError{Worker: w.Name, Reason: reason}
	}
	return nil
}

// tlsConfig is the mTLS configuration for a worker: the bundle's (which
// enforces the trust policy) with trust refusals typed as TrustError.
func (d *Dialer) tlsConfig(w Worker) *tls.Config {
	cfg := d.bundle.ClientConfig(w.CertName)
	verify := cfg.VerifyConnection
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		err := verify(cs)
		if err == nil || len(cs.PeerCertificates) == 0 {
			return err
		}
		// Only the trust check can have failed for a worker certificate.
		if id, ierr := pki.IdentityFromCert(cs.PeerCertificates[0]); ierr == nil && id.Role == pki.RoleWorker {
			if p := d.bundle.Trust.Load(); p.Active() {
				if _, terr := p.Check(cs.PeerCertificates[0]); terr != nil {
					return &TrustError{Worker: w.Name, Reason: terr.Error()}
				}
			}
		}
		return err
	}
	return cfg
}

// TrustRefusal returns the message of a TrustError inside err, which may be
// a gRPC status error that quoted it.
func TrustRefusal(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var te *TrustError
	if errors.As(err, &te) {
		return te.Error(), true
	}
	s := err.Error()
	if st, ok := status.FromError(err); ok {
		s = st.Message()
	}
	i := strings.Index(s, trustMarker)
	if i < 0 {
		return "", false
	}
	// gRPC: `connection error: desc = "transport: Error while dialing: <err>"`.
	if j := strings.LastIndex(s[:i], `desc = "`); j >= 0 {
		if q, err := strconv.QuotedPrefix(s[j+len(`desc = `):]); err == nil {
			if u, err := strconv.Unquote(q); err == nil {
				s = u
				i = strings.Index(s, trustMarker)
			}
		}
	}
	if i < 0 {
		return "", false
	}
	if k := strings.LastIndex(s[:i], "worker "); k >= 0 {
		s = s[k:]
	}
	return s, true
}

// WatchTrust keeps the trust policy current for a long-lived process
// (longarm daemon, longarm mcp): `longarm trust ...` changes apply to its
// next handshakes.
func (d *Dialer) WatchTrust(ctx context.Context, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	TrustStore(d.cfg.Dir).Watch(ctx, d.bundle.Trust, true, 2*time.Second, log)
}
