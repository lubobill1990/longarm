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
	"github.com/lubobill1990/longarm/internal/signin"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/trust"
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
	tr, err := prepareEnroll(dataDir, o.Force)
	if err != nil {
		return err
	}
	hubAddr = hubclient.NormalizeAddr(hubAddr)
	r, err := hubclient.Join(ctx, hubAddr, code, hubclient.JoinOptions{Name: o.Name, Fingerprint: o.Fingerprint, Trust: tr})
	if err != nil {
		return err
	}
	if r.Resp.State == "approved" {
		return finishEnroll(o.Out, dataDir, hubAddr, r)
	}
	reportCA(o.Out, r)
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
	// The owner the join named holds from the start - before the first hub
	// session, also with the hub down (version 0: any push of the hub wins).
	// Only ever a restriction, and a fresh enrollment's: overwrite.
	if err := tenancy.WriteWorkerAccess(filepath.Join(dataDir, tenancy.WorkerAccessFile), &pb.WorkerAccess{Owner: r.Resp.Owner}); err != nil {
		return err
	}
	hc := hubclient.Config{Address: hubAddr, Owner: r.Resp.Owner}
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

// prepareEnroll checks for an existing certificate and makes the data
// dir; it returns the node key binding the certificate carries from the
// start, so an endorsement (longarmd trust accept) only needs a renewal.
func prepareEnroll(dataDir string, force bool) (*pki.CSRTrust, error) {
	tlsDir := filepath.Join(dataDir, TLSDir)
	if _, err := os.Stat(filepath.Join(tlsDir, pki.NodeCertFile)); err == nil && !force {
		return nil, fmt.Errorf("%s already holds a certificate (use --force to enroll again)", tlsDir)
	}
	if err := EnsureSystemDataDir(dataDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		return nil, err
	}
	return trust.Store{Dir: filepath.Join(dataDir, TrustDir)}.CSRTrust()
}

func reportCA(out io.Writer, r *hubclient.JoinResult) {
	if r.Pinned {
		fmt.Fprintf(out, "hub CA verified (%s)\n", r.CAFingerprint)
	} else {
		fmt.Fprintf(out, "warning: hub CA not pinned; trusting %s on first use (pass --ca-fingerprint to verify)\n", r.CAFingerprint)
	}
}

// finishEnroll installs an approved enrollment and says so.
func finishEnroll(out io.Writer, dataDir, hubAddr string, r *hubclient.JoinResult) error {
	reportCA(out, r)
	if err := installEnrollment(dataDir, hubAddr, r); err != nil {
		return err
	}
	fmt.Fprintf(out, "joined as worker %s\n", r.Resp.Name)
	if ow := r.Resp.Owner; ow != "" {
		// The rule comes from the hub; pinning the owner locally holds
		// even against the hub's operators. Never written from hub data.
		fmt.Fprintf(out, "owned by %s: only %s (and whom %s shares it with) may use it\n", ow, ow, ow)
		fmt.Fprintf(out, "to hold that even against the hub's admins, pin it in longarmd.toml: [access] owners = [%q], users = [%q]\n"+
			"(plus whom you share with), and use end-to-end trust (longarmd trust add-user <%s's key line from longarm trust show>; docs/security.md)\n",
			ow, ow, ow)
	}
	return nil
}

// LoginOptions for Login.
type LoginOptions struct {
	Name        string
	Fingerprint string // pin the hub CA (sha256 hex); empty = trust on first use
	Force       bool   // replace an existing certificate
	// Az: sign in with the Azure CLI's token (EntraJoin) instead of
	// pairing; Scope overrides the token's scope.
	Az      bool
	Scope   string
	Browser bool // pairing: open the /link page here if there is a desktop
	Out     io.Writer
}

// Login enrolls this worker as its user's own worker with the user's
// Microsoft account (`longarmd login`): by pairing - the user approves a
// code at the hub's /link page, in any browser - or with az's token (Az).
// The worker is approved right away and owned by that user.
func Login(ctx context.Context, dataDir, hubAddr string, o LoginOptions) error {
	tr, err := prepareEnroll(dataDir, o.Force)
	if err != nil {
		return err
	}
	hubAddr = hubclient.NormalizeAddr(hubAddr)
	jo := hubclient.JoinOptions{Name: o.Name, Fingerprint: o.Fingerprint, Trust: tr}
	var r *hubclient.JoinResult
	if o.Az {
		r, err = hubclient.Login(ctx, hubAddr, hubclient.LoginOptions{JoinOptions: jo, Kind: "worker",
			Token: func(ctx context.Context, info *pb.AuthInfoResponse) (string, error) {
				return signin.AzToken(ctx, info, o.Scope)
			}})
	} else {
		r, err = signin.Pair(ctx, hubAddr, "worker", jo, o.Out, o.Browser)
	}
	if err != nil {
		return err
	}
	if r.Resp.State != "approved" || r.Resp.Role != "worker" || r.Resp.Owner == "" {
		return fmt.Errorf("the hub answered %s %s (owner %q), not an owned worker", r.Resp.State, r.Resp.Role, r.Resp.Owner)
	}
	return finishEnroll(o.Out, dataDir, hubAddr, r)
}
