package longarmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/client"
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
	for _, s := range ownerNotes(r.Resp.Owner) {
		fmt.Fprintln(out, s)
	}
	return nil
}

// ownerNotes tell the user of a worker owned by ow what that means and
// how to hold it against the hub too.
func ownerNotes(ow string) []string {
	if ow == "" {
		return nil
	}
	// The rule comes from the hub; pinning the owner locally holds even
	// against the hub's operators. Never written from hub data.
	return []string{
		fmt.Sprintf("owned by %s: only %s (and whom %s shares it with) may use it", ow, ow, ow),
		fmt.Sprintf("to hold that even against the hub's admins, pin it in longarmd.toml: [access] owners = [%q], users = [%q]\n"+
			"(plus whom you share with), and use end-to-end trust (longarmd trust add-user <%s's key line from longarm trust show>; docs/security.md)",
			ow, ow, ow),
	}
}

// WorkerTarget is this machine's longarmd as a sign-in target
// (signin.Target): an owned worker's enrollment in DataDir
// (DefaultDataDir(): LONGARM_DATA_DIR, the MSI's C:\ProgramData\Longarm\longarmd,
// /var/lib/longarmd as root, else ~/.longarm). Whoever installs it must
// be able to write DataDir (the service account, or an administrator).
type WorkerTarget struct {
	DataDir string
	// Force replaces an existing certificate.
	Force bool
}

// Prepare refuses an enrolled data dir without Force, makes the data dir
// and returns the worker's end-to-end trust binding.
func (w WorkerTarget) Prepare(string) (*pki.CSRTrust, error) {
	return prepareEnroll(w.DataDir, w.Force)
}

// Install writes an approved owned-worker enrollment. A longarmd service
// waiting for its sign-in (hub.toml sign_in) picks it up by itself.
func (w WorkerTarget) Install(hub string, r *hubclient.JoinResult, note func(string)) (string, error) {
	if r.Resp.State != "approved" || r.Resp.Role != string(pki.RoleWorker) || r.Resp.Owner == "" {
		return "", fmt.Errorf("the hub answered %s %s (owner %q), not an owned worker", r.Resp.State, r.Resp.Role, r.Resp.Owner)
	}
	if err := installEnrollment(w.DataDir, hub, r); err != nil {
		return "", err
	}
	if note != nil {
		for _, s := range ownerNotes(r.Resp.Owner) {
			note(s)
		}
	}
	return w.DataDir, nil
}

// LoginOptions for Login.
type LoginOptions struct {
	Name string // worker name (default: the hostname)
	// Fingerprint pins the hub CA (sha256 hex); empty: fetched from
	// https://<hub>/ca.crt (signin.SignIn).
	Fingerprint string
	Force       bool // replace an existing certificate
	// Az: sign in with the Azure CLI's token (EntraJoin) instead of
	// pairing; Scope overrides the token's scope.
	Az      bool
	Scope   string
	Browser bool // pairing: open the /link page here if there is a desktop
	// Client: also sign in the user's client device (role both) - one
	// code, one approval for both - into ConfigDir (longarm's config dir)
	// as DeviceName (default <user>-<hostname>).
	Client     bool
	ConfigDir  string
	DeviceName string
	// Labels for the worker, recorded by the hub (nil: an installer's,
	// signin.InstalledLabels).
	Labels map[string]string
	// WorkerKind for the summary ("user session", "service"; "": unsaid).
	WorkerKind string
	Out        io.Writer
}

// Login enrolls this worker as its user's own worker with the user's
// Microsoft account (`longarmd login`): by pairing - the user approves a
// code at the hub's /link page, in any browser - or with az's token (Az).
// The worker is approved right away and owned by that user. With Client
// the same approval also makes this machine the user's client device.
func Login(ctx context.Context, dataDir, hub string, o LoginOptions) error {
	so := signin.Options{Hub: hub, Fingerprint: o.Fingerprint, WorkerName: o.Name, DeviceName: o.DeviceName,
		Worker: WorkerTarget{DataDir: dataDir, Force: o.Force}, Scope: o.Scope, OpenBrowser: o.Browser, WorkerLabels: o.Labels,
		WorkerKind: o.WorkerKind}
	if o.Client {
		so.Client = client.DeviceTarget{ConfigDir: o.ConfigDir, Force: o.Force}
	}
	if o.Az {
		so.Method = signin.MethodAz
	}
	r, err := signin.SignIn(ctx, so, signin.Console(o.Out))
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "hub CA verified (%s)\n", r.CAFingerprint)
	fmt.Fprintf(o.Out, "joined as worker %s\n", r.Worker.Name)
	if len(r.Worker.Labels) > 0 {
		fmt.Fprintf(o.Out, "worker labels: %s (change them: longarm worker label %s KEY=VALUE KEY-)\n", labelString(r.Worker.Labels), r.Worker.Name)
	}
	if r.Device != nil {
		fmt.Fprintf(o.Out, "signed in to %s as %s/%s (config %s)\n", r.Hub, r.Device.Role, r.Device.Name, r.Device.Dir)
	}
	return nil
}

// PendingHub is the hub `longarmd login` signs in to when none is given:
// the data dir's hub.toml (an installer's sign-in to come, or the hub this
// worker is enrolled with, its CA pinned), else the hub the MSI recorded,
// else the one built into this release (signin.FallbackHub).
// fp, when given, wins.
func PendingHub(dataDir, fp string) (string, string, error) {
	hf, err := hubclient.LoadFile(dataDir)
	if err != nil {
		return "", "", err
	}
	if hf.Address != "" {
		if fp == "" {
			fp = hf.CAFingerprint
		}
		if fp == "" {
			if b, err := os.ReadFile(filepath.Join(dataDir, TLSDir, pki.CACertFile)); err == nil {
				if ca, err := pki.ParseCertPEM(b); err == nil {
					fp = pki.Fingerprint(ca)
				}
			}
		}
		return hf.Address, fp, nil
	}
	if hub, ifp, _ := signin.FallbackHub(); hub != "" {
		if fp == "" {
			fp = ifp
		}
		return hub, fp, nil
	}
	return "", "", fmt.Errorf("which hub? longarmd login <hub>, e.g. longarmd login longarm.example.com (no hub in %s)", filepath.Join(dataDir, hubclient.File))
}

// SetUpSignIn records a hub for a sign-in to come (the MSI with LOGIN=1,
// install-helper setup --login): hub.toml with the address, the CA
// fingerprint if known and sign_in, so that longarmd waits for its
// certificate and `longarmd login` / the tray need no input. An existing
// enrollment is left alone.
func SetUpSignIn(dataDir, hub, fp string) error {
	if _, err := os.Stat(filepath.Join(dataDir, TLSDir, pki.NodeCertFile)); err == nil {
		return nil
	}
	return hubclient.SaveFile(dataDir, hubclient.Config{Address: strings.TrimSpace(hub), SignIn: true,
		CAFingerprint: pki.NormalizeFingerprint(fp)})
}

// waitSignIn holds longarmd's start while a sign-in is to come (hub.toml
// sign_in): it returns once the enrollment is installed - the certificate
// there and hub.toml rewritten without sign_in (longarmd login / the tray
// write the certificate first, hub.toml last) - or ctx ends. The caller
// re-reads hub.toml afterwards (Config.reloadHubFile).
func waitSignIn(ctx context.Context, dataDir string, log *slog.Logger) error {
	cert := filepath.Join(dataDir, TLSDir, pki.NodeCertFile)
	var last time.Time
	for {
		hf, _ := hubclient.LoadFile(dataDir)
		if !hf.SignIn || hf.Address == "" {
			if !last.IsZero() {
				log.Info("signed in; starting", "hub", hf.Address)
			}
			return nil
		}
		if _, err := os.Stat(cert); err != nil && time.Since(last) > 10*time.Minute {
			log.Info("waiting for this worker's sign-in", "hub", hf.Address,
				"sign_in_with", "the Longarm tray, or: longarmd login --role both (as the service's account, or elevated)")
			last = time.Now()
		}
		// the admin worker: its CSR to the tray, the approved enrollment back
		adminHandoffStep(dataDir, log)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// applyHubFile merges hub.toml into the config: its address when
// longarmd.toml names none (hubFromFile), and the pending poll token.
func (c *Config) applyHubFile(hf hubclient.Config) {
	if c.hubFromFile {
		c.Hub.Address = hf.Address
		c.Hub.Disabled = c.Hub.Disabled || hf.Disabled
	}
	c.HubPollToken = hf.PollToken
}

// reloadHubFile re-reads hub.toml (applyHubFile): an enrollment written
// after LoadConfig - a sign-in the service waited for, or one that
// finished just before - may have changed the hub address.
func (c *Config) reloadHubFile() error {
	if !c.hubLoaded { // a Config built in code (tests, join): nothing came from hub.toml
		return nil
	}
	hf, err := hubclient.LoadFile(c.DataDir)
	if err != nil {
		return err
	}
	c.applyHubFile(hf)
	return nil
}

func labelString(m map[string]string) string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
