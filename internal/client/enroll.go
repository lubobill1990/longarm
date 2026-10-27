package client

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
)

// DeviceTarget is the user's client device as a sign-in target
// (signin.Target): certificate, key and hub.toml in ConfigDir (longarm's
// config dir, DefaultConfigDir()).
type DeviceTarget struct {
	ConfigDir string
	// Force replaces a certificate of another hub.
	Force bool
}

// Prepare refuses to replace another hub's certificate without Force
// (signing in to the same hub again is what login is for) and returns the
// device's end-to-end trust binding.
func (d DeviceTarget) Prepare(hub string) (*pki.CSRTrust, error) {
	tlsDir := filepath.Join(d.ConfigDir, TLSDir)
	if _, err := os.Stat(filepath.Join(tlsDir, pki.NodeCertFile)); err == nil && !d.Force {
		if hc, _ := hubclient.LoadFile(d.ConfigDir); hc.Address != hub {
			return nil, fmt.Errorf("%s already holds a certificate of another hub (use --force to replace it)", tlsDir)
		}
	}
	tr, err := TrustStore(d.ConfigDir).CSRTrust()
	if err != nil {
		return nil, fmt.Errorf("end-to-end trust: %w", err)
	}
	return tr, nil
}

// Install writes an approved enrollment (InstallEnrollment).
func (d DeviceTarget) Install(hub string, r *hubclient.JoinResult, note func(string)) (string, error) {
	tr, _ := TrustStore(d.ConfigDir).CSRTrust()
	return d.ConfigDir, InstallEnrollment(d.ConfigDir, hub, r, tr, note)
}

// InstallEnrollment installs an approved client enrollment in configDir:
// the certificate bundle, hub.toml (and a minted tailnet key). tr is the
// trust binding the CSR carried (for the endorsement check); note gets
// what the user should know.
func InstallEnrollment(configDir, hub string, r *hubclient.JoinResult, tr *pki.CSRTrust, note func(string)) error {
	if note == nil {
		note = func(string) {}
	}
	if r.Resp.State != "approved" {
		return fmt.Errorf("enrollment is %s: %s", r.Resp.State, r.Resp.Message)
	}
	if err := pki.WriteBundleAtomic(filepath.Join(configDir, TLSDir), r.Resp.CaPem, r.Resp.CertPem, r.KeyPEM); err != nil {
		return err
	}
	hc := hubclient.Config{Address: hub}
	if err := hubclient.SaveTailnet(configDir, &hc, r.Resp.Tailnet); err != nil {
		return err
	}
	if err := hubclient.SaveFile(configDir, hc); err != nil {
		return err
	}
	if tr != nil && tr.Endorsement != nil && tr.Endorsement.Name != r.Resp.Name {
		note(fmt.Sprintf("warning: this device's endorsement is for %q; get one for %s (longarm trust request / accept)", tr.Endorsement.Name, r.Resp.Name))
	}
	if t := r.Resp.Tailnet; t != nil {
		note(fmt.Sprintf("tailnet %s: key for %s (%s) stored; the tailscale leg starts on first use", t.ControlUrl, t.Hostname, strings.Join(t.Tags, ",")))
	}
	return nil
}
