package longarmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// RevokedFile is the revocation list pushed by the hub (data dir).
const RevokedFile = "revoked.json"

// JoinOptions for Join.
type JoinOptions struct {
	Name        string
	Fingerprint string // pin the hub CA (sha256 hex); empty = trust on first use
	NoWait      bool   // return while pending; longarmd run finishes the enrollment
	Force       bool   // replace an existing certificate
	Out         io.Writer
}

// Join enrolls this worker with a hub using a join code (`longarmd join`).
func Join(ctx context.Context, dataDir, hubAddr, code string, o JoinOptions) error {
	tlsDir := filepath.Join(dataDir, TLSDir)
	if _, err := os.Stat(filepath.Join(tlsDir, pki.NodeCertFile)); err == nil && !o.Force {
		return fmt.Errorf("%s already holds a certificate (use --force to enroll again)", tlsDir)
	}
	if err := EnsureSystemDataDir(dataDir); err != nil {
		return err
	}
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		return err
	}
	hubAddr = hubclient.NormalizeAddr(hubAddr)
	r, err := hubclient.Join(ctx, hubAddr, code, hubclient.JoinOptions{Name: o.Name, Fingerprint: o.Fingerprint})
	if err != nil {
		return err
	}
	if r.Pinned {
		fmt.Fprintf(o.Out, "hub CA verified (%s)\n", r.CAFingerprint)
	} else {
		fmt.Fprintf(o.Out, "warning: hub CA not pinned; trusting %s on first use (pass --ca-fingerprint to verify)\n", r.CAFingerprint)
	}
	if r.Resp.State == "approved" {
		if err := installEnrollment(dataDir, hubAddr, r); err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "joined as worker %s\n", r.Resp.Name)
		return nil
	}
	// Pending: keep the key and the CA until an admin approves.
	if err := os.WriteFile(filepath.Join(tlsDir, hubclient.PendingKeyFile), r.KeyPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tlsDir, pki.CACertFile), r.Resp.CaPem, 0o644); err != nil {
		return err
	}
	if err := hubclient.SaveFile(dataDir, hubclient.Config{Address: hubAddr, PollToken: r.Resp.PollToken}); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "enrolled as %s; pending approval: longarm admin approve %s\n", r.Resp.Name, r.Resp.Name)
	if o.NoWait {
		fmt.Fprintln(o.Out, "longarmd run (or the service) finishes the enrollment once approved")
		return nil
	}
	cfg := Config{DataDir: dataDir, Hub: HubConfig{Address: hubAddr}, HubPollToken: r.Resp.PollToken}
	log := slog.New(slog.NewTextHandler(o.Out, nil))
	if err := completeEnrollment(ctx, cfg, log); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "approved; joined as worker %s\n", r.Resp.Name)
	return nil
}

func installEnrollment(dataDir, hubAddr string, r *hubclient.JoinResult) error {
	if err := pki.WriteBundleAtomic(filepath.Join(dataDir, TLSDir), r.Resp.CaPem, r.Resp.CertPem, r.KeyPEM); err != nil {
		return err
	}
	os.Remove(filepath.Join(dataDir, TLSDir, hubclient.PendingKeyFile))
	hc := hubclient.Config{Address: hubAddr}
	// The hub's Headscale minted a tailnet key: longarmd's embedded tailscale
	// node joins with it (an extra transport leg, DESIGN §4.4).
	if err := hubclient.SaveTailnet(dataDir, &hc, r.Resp.Tailnet); err != nil {
		return err
	}
	return hubclient.SaveFile(dataDir, hc)
}

// completeEnrollment waits until a pending enrollment is approved and
// installs the certificate.
func completeEnrollment(ctx context.Context, cfg Config, log *slog.Logger) error {
	tlsDir := filepath.Join(cfg.DataDir, TLSDir)
	if _, err := os.Stat(filepath.Join(tlsDir, pki.NodeCertFile)); err == nil {
		// Already installed (e.g. by a concurrent longarmd join): drop the token.
		return hubclient.SaveFile(cfg.DataDir, hubclient.Config{Address: cfg.Hub.Address})
	}
	keyPEM, err := os.ReadFile(filepath.Join(tlsDir, hubclient.PendingKeyFile))
	if err != nil {
		return fmt.Errorf("pending enrollment without its key: %w (run longarmd join again)", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(tlsDir, pki.CACertFile))
	if err != nil {
		return err
	}
	var last time.Time
	r, err := hubclient.WaitApproved(ctx, cfg.Hub.Address, cfg.HubPollToken, caPEM, keyPEM, 3*time.Second, func(resp *pb.JoinResponse) {
		if time.Since(last) > time.Minute {
			log.Info("waiting for enrollment approval", "node", resp.Name, "approve_with", "longarm admin approve "+resp.Name)
			last = time.Now()
		}
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return fmt.Errorf("enrollment: %w", err)
	}
	log.Info("enrollment approved", "node", r.Resp.Name)
	return installEnrollment(cfg.DataDir, cfg.Hub.Address, r)
}
