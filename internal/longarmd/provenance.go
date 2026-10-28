package longarmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

// Provenance: what the installer (install-helper, as SYSTEM) and the admin
// worker record about the machine-wide data dirs, where only administrators
// can write it (Windows: HKLM\Software\Longarm\Provenance - not one of the
// MSI's own registry values, which a repair rewrites from properties).
// ProgramData lets every user create directories, so a data dir that
// exists before the first install may hold planted credentials (a CA, a
// certificate, authorized_keys, longarmd.toml): the installer keeps a data
// dir only when it recorded creating it and its ACL still lets nobody else
// write, and moves anything else aside.

// ProvenanceStore keeps the provenance values.
type ProvenanceStore interface {
	Get(name string) string
	Set(name, value string) error
	Delete(name string) error
}

// Provenance names.
const (
	// ProvAdminCA: the hub CA fingerprint the admin worker signed in with
	// (it serves only with that CA).
	ProvAdminCA = "AdminCAFingerprint"
)

// ProvDataDir names a data dir the installer created fresh.
func ProvDataDir(dir string) string {
	return "DataDir:" + strings.ToLower(filepath.Clean(dir))
}

// provenance is the store (registry on Windows; tests replace it).
var provenance ProvenanceStore = newProvenance()

// memProvenance keeps values in memory (other systems, tests).
type memProvenance struct {
	mu sync.Mutex
	m  map[string]string
}

func (p *memProvenance) Get(n string) string { p.mu.Lock(); defer p.mu.Unlock(); return p.m[n] }
func (p *memProvenance) Set(n, v string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]string{}
	}
	p.m[n] = v
	return nil
}
func (p *memProvenance) Delete(n string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, n)
	return nil
}

// trustedDir checks that only SYSTEM / Administrators (and also) can
// change dir (platform.TrustedSystemPath on Windows; tests replace it).
var trustedDir = func(dir string, also ...string) error { return trustedSystemPath(dir, also...) }

// FreshSystemDir prepares a machine-wide data dir before the installer
// secures and fills it: kept when it was recorded as the installer's
// (requireRecord) or - for data dirs of older installers - when only
// administrators could write it; otherwise whatever is there is moved
// aside (to <dir>.untrusted-<time>, then removed: a reparse point is
// removed, never followed) and the dir starts empty. The caller secures it
// and then calls RecordSystemDir.
func FreshSystemDir(dir string, requireRecord bool, out io.Writer, also ...string) error {
	if out == nil {
		out = io.Discard
	}
	fi, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		provenance.Delete(ProvDataDir(dir))
		return nil
	}
	if err != nil {
		return err
	}
	why := ""
	switch {
	case fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !fi.IsDir():
		why = "it is not a plain directory"
	case requireRecord && provenance.Get(ProvDataDir(dir)) == "":
		why = "this installer did not create it"
	default:
		if err := trustedDir(dir, also...); err != nil {
			why = err.Error()
		}
	}
	if why == "" {
		return nil
	}
	aside := fmt.Sprintf("%s.untrusted-%d", dir, time.Now().Unix())
	fmt.Fprintf(out, "%s existed before and is not trusted (%s): moving it aside to %s and starting empty\n", dir, why, aside)
	if err := os.Rename(dir, aside); err != nil {
		return fmt.Errorf("move the untrusted %s aside: %w", dir, err)
	}
	if err := os.RemoveAll(aside); err != nil {
		fmt.Fprintf(out, "warning: removing %s: %v\n", aside, err)
	}
	provenance.Delete(ProvDataDir(dir))
	return nil
}

// RecordSystemDir records that the installer created and secured dir.
func RecordSystemDir(dir string) error {
	return provenance.Set(ProvDataDir(dir), time.Now().UTC().Format(time.RFC3339))
}

// verifyAdminProvenance: the admin worker serves only with the hub CA its
// sign-in recorded - a certificate and CA nobody recorded (planted before
// the install) are refused.
func verifyAdminProvenance(dataDir string) error {
	cert := filepath.Join(dataDir, TLSDir, pki.NodeCertFile)
	if _, err := os.Stat(cert); err != nil {
		return nil // not signed in yet: nothing to serve with
	}
	b, err := os.ReadFile(filepath.Join(dataDir, TLSDir, pki.CACertFile))
	if err != nil {
		return fmt.Errorf("admin worker: %w", err)
	}
	ca, err := pki.ParseCertPEM(b)
	if err != nil {
		return fmt.Errorf("admin worker: hub CA: %w", err)
	}
	rec := provenance.Get(ProvAdminCA)
	if rec == "" {
		return fmt.Errorf("admin worker: refusing to serve: no hub CA was recorded for its credentials in %s (they were not made by its sign-in); disable and re-enable it", ProvenanceLocation)
	}
	if got := pki.Fingerprint(ca); got != pki.NormalizeFingerprint(rec) {
		return fmt.Errorf("admin worker: refusing to serve: its hub CA %s is not the recorded %s", got, rec)
	}
	return nil
}

// IsAdminWorker reports whether cfg runs the admin worker (its service
// name, or the installer's marker in the data dir).
func IsAdminWorker(cfg Config) bool {
	return strings.EqualFold(cfg.ServiceName, AdminServiceName) || AdminHandoff(cfg.DataDir) != ""
}

// ForgetAdminWorker drops the admin worker's provenance (its CA and data
// dir record): a new admin worker starts from nothing.
func ForgetAdminWorker() {
	provenance.Delete(ProvAdminCA)
	if pd := os.Getenv("ProgramData"); pd != "" {
		provenance.Delete(ProvDataDir(AdminDataDirIn(filepath.Join(pd, "Longarm"))))
	}
}
