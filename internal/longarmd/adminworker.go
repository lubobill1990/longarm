package longarmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The admin worker (Windows, the MSI's ADMIN_WORKER=1; off by default): a
// second longarmd, a service running as LocalSystem - no password needed,
// so it works on Entra-only machines - with a data dir of its own that
// only SYSTEM and Administrators can read. It runs while nobody is logged
// on, and what its owner runs on it runs as SYSTEM: installing software,
// administration without a remote desktop. The hub enrolls it as an owned
// worker marked with the reserved label longarm.io/admin=true: owner only,
// never shared, never placed automatically (docs/security.md "Admin
// worker").
//
// Its enrollment rides on the user's sign-in (one code, one approval): the
// service can't show a code to anybody, and the tray, running as the user,
// can't write the service's data dir. So the service makes its key and CSR
// itself while it waits for the sign-in and leaves the CSR (public) in a
// hand-off dir only SYSTEM, Administrators and the installing user can
// write (AdminRequestFile); the tray adds it to its pairing and leaves the
// approved certificate there (AdminResponseFile); the service checks that
// the certificate is for its key, chains to the hub's CA and carries the
// admin label, installs it, pins the owner in longarmd.toml ([access]:
// only the owner, other admins nothing) and starts. The private key never
// leaves the data dir.

const (
	// AdminServiceName is the admin worker's service.
	AdminServiceName = "longarmd-admin"
	// AdminMarkerFile in a data dir: this longarmd is the admin worker
	// (written by install-helper admin-setup); it names the hand-off dir.
	AdminMarkerFile = "admin-worker.json"
	// AdminRequestFile / AdminResponseFile / AdminErrorFile in the hand-off
	// dir.
	AdminRequestFile  = "request.json"
	AdminResponseFile = "response.json"
	AdminErrorFile    = "error.txt"
	// AdminLabel marks the admin worker; the hub sets it and nobody can
	// change it (the hub's sched.AdminLabel).
	AdminLabel = "longarm.io/admin"
	// AdminListen: the admin worker's own local listener (the user-session
	// worker has 127.0.0.1:7701).
	AdminListen = "tcp://127.0.0.1:7702"

	adminKeyFile = "admin-signin.key"
)

// resolveHubCA fetches and checks a hub's CA (hubclient.ResolveHub);
// tests replace it.
var resolveHubCA = func(ctx context.Context, hub string) (string, error) {
	tg, err := hubclient.ResolveHub(ctx, hub, hubclient.ResolveOptions{})
	if err != nil {
		return "", err
	}
	return tg.Fingerprint, nil
}

// AdminDataDirIn and AdminHandoffDirIn are the admin worker's data dir and
// hand-off dir below the Longarm dir in ProgramData.
func AdminDataDirIn(programDataLongarm string) string {
	return filepath.Join(programDataLongarm, "longarmd-admin")
}
func AdminHandoffDirIn(programDataLongarm string) string {
	return filepath.Join(programDataLongarm, "admin-signin")
}

type adminMarker struct {
	Handoff string `json:"handoff"`
}

// WriteAdminMarker marks dataDir as the admin worker's with its hand-off
// dir.
func WriteAdminMarker(dataDir, handoff string) error {
	b, _ := json.Marshal(adminMarker{Handoff: handoff})
	return os.WriteFile(filepath.Join(dataDir, AdminMarkerFile), b, 0o600)
}

// AdminHandoff is the hand-off dir of dataDir's admin worker ("" when
// dataDir is not an admin worker's).
func AdminHandoff(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, AdminMarkerFile))
	if err != nil {
		return ""
	}
	var m adminMarker
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return m.Handoff
}

// AdminRequest is AdminRequestFile: what the admin worker asks to be
// enrolled with.
type AdminRequest struct {
	Hub     string    `json:"hub"`
	CSR     string    `json:"csr_pem"`
	KeyID   string    `json:"node_key,omitempty"` // its end-to-end trust key
	Created time.Time `json:"created"`
}

// AdminResponse is AdminResponseFile: the approved enrollment.
type AdminResponse struct {
	Hub           string `json:"hub"`
	CAFingerprint string `json:"ca_fingerprint"`
	Join          []byte `json:"join_response"` // pb.JoinResponse
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// adminHandoffStep is one round of the admin worker's side while it waits
// for its sign-in (waitSignIn): install an approved enrollment the tray
// left, else make sure a request for the pending hub is there.
func adminHandoffStep(dataDir string, log *slog.Logger) {
	dir := AdminHandoff(dataDir)
	if dir == "" {
		return
	}
	hf, err := hubclient.LoadFile(dataDir)
	if err != nil || !hf.SignIn || hf.Address == "" {
		return
	}
	if b, err := os.ReadFile(filepath.Join(dir, AdminResponseFile)); err == nil {
		// The response goes last: AdminWorkerTarget.Install takes "no
		// response and no error" for success, so the error must be there
		// before the response is gone (removed first, a refused enrollment
		// could be reported as installed).
		if err := installAdminResponse(dataDir, hf, b); err != nil {
			log.Error("admin worker: the enrollment the sign-in left is unusable", "err", err)
			os.WriteFile(filepath.Join(dir, AdminErrorFile), []byte(err.Error()+"\n"), 0o644)
			os.Remove(filepath.Join(dir, AdminResponseFile))
			return
		}
		os.Remove(filepath.Join(dir, AdminRequestFile))
		os.Remove(filepath.Join(dir, AdminErrorFile))
		os.Remove(filepath.Join(dir, AdminResponseFile))
		log.Info("admin worker: signed in", "hub", hf.Address)
		return
	}
	var req AdminRequest
	if b, err := os.ReadFile(filepath.Join(dir, AdminRequestFile)); err == nil && json.Unmarshal(b, &req) == nil &&
		req.Hub == hf.Address && fileExists(filepath.Join(dataDir, TLSDir, adminKeyFile)) {
		return // waiting for the tray
	}
	if hf.CAFingerprint == "" {
		// The hub's CA, fetched by the service itself (https://<hub>/ca.crt,
		// checked with the system's roots) and pinned: the enrollment the
		// user's tray hands back must chain to it, not to a CA of the
		// tray's choosing.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		fp, err := resolveHubCA(ctx, hf.Address)
		cancel()
		if err != nil {
			log.Error("admin worker: fetching the hub's CA", "hub", hf.Address, "err", err)
			return
		}
		if err := SetUpSignIn(dataDir, hf.Address, fp); err != nil {
			log.Error("admin worker: pinning the hub's CA", "err", err)
			return
		}
		hf.CAFingerprint = pki.NormalizeFingerprint(fp)
	}
	if err := writeAdminRequest(dataDir, dir, hf.Address); err != nil {
		log.Error("admin worker: preparing the sign-in request", "err", err)
	}
}

func writeAdminRequest(dataDir, dir, hub string) error {
	trust, err := prepareEnroll(dataDir, false)
	if err != nil {
		return err
	}
	keyPEM, csrPEM, err := pki.NewKeyAndCSR("", trust)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dataDir, TLSDir, adminKeyFile), keyPEM); err != nil {
		return err
	}
	req := AdminRequest{Hub: hub, CSR: string(csrPEM), Created: time.Now().UTC()}
	if trust != nil {
		req.KeyID = pki.KeyID(trust.NodeKey.Public().(ed25519.PublicKey))
	}
	b, _ := json.MarshalIndent(req, "", "  ")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, AdminRequestFile), b)
}

// installAdminResponse checks and installs the enrollment the tray left.
func installAdminResponse(dataDir string, hf hubclient.Config, b []byte) error {
	var ar AdminResponse
	if err := json.Unmarshal(b, &ar); err != nil {
		return err
	}
	var resp pb.JoinResponse
	if err := proto.Unmarshal(ar.Join, &resp); err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dataDir, TLSDir, adminKeyFile))
	if err != nil {
		return fmt.Errorf("no pending key: %w", err)
	}
	if hf.CAFingerprint == "" {
		return errors.New("no pinned hub CA to check the enrollment against")
	}
	if err := checkAdminEnrollment(&resp, keyPEM, hf.CAFingerprint); err != nil {
		return err
	}
	ca, _ := pki.ParseCertPEM(resp.CaPem)
	r := &hubclient.JoinResult{Resp: &resp, KeyPEM: keyPEM, CAFingerprint: pki.Fingerprint(ca)}
	if err := installEnrollment(dataDir, hf.Address, r); err != nil {
		return err
	}
	os.Remove(filepath.Join(dataDir, TLSDir, adminKeyFile))
	if err := provenance.Set(ProvAdminCA, pki.Fingerprint(ca)); err != nil {
		return fmt.Errorf("recording the hub CA: %w", err)
	}
	return pinAdminAccess(dataDir, resp.Owner)
}

// checkAdminEnrollment: an approved owned worker marked as the admin
// worker, its certificate for keyPEM and issued by the response's CA, which
// matches the pinned fingerprint (if any).
func checkAdminEnrollment(resp *pb.JoinResponse, keyPEM []byte, fp string) error {
	if resp.State != "approved" || resp.Role != string(pki.RoleWorker) || resp.Owner == "" {
		return fmt.Errorf("the hub answered %s %s (owner %q), not an owned worker", resp.State, resp.Role, resp.Owner)
	}
	if resp.Labels[AdminLabel] != "true" {
		return fmt.Errorf("the hub did not mark it as an admin worker (%s=true): it is too old for admin workers", AdminLabel)
	}
	if !pki.ValidUser(resp.Owner) {
		return fmt.Errorf("invalid owner %q", resp.Owner)
	}
	if _, err := tls.X509KeyPair(resp.CertPem, keyPEM); err != nil {
		return fmt.Errorf("the certificate is not for this worker's key: %w", err)
	}
	ca, err := pki.ParseCertPEM(resp.CaPem)
	if err != nil {
		return fmt.Errorf("hub CA: %w", err)
	}
	if fp != "" && pki.Fingerprint(ca) != pki.NormalizeFingerprint(fp) {
		return fmt.Errorf("hub CA %s is not the pinned %s", pki.Fingerprint(ca), fp)
	}
	cert, err := pki.ParseCertPEM(resp.CertPem)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return fmt.Errorf("the certificate is not the hub CA's: %w", err)
	}
	return nil
}

// pinAdminAccess writes the admin worker's [access] policy into
// longarmd.toml: only its owner may use it, other admin certificates get
// nothing but node info - whatever the hub says later.
func pinAdminAccess(dataDir, owner string) error {
	p := filepath.Join(dataDir, ConfigFile)
	old, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if bytes.Contains(old, []byte("[access]")) {
		return fmt.Errorf("%s has an [access] section already: set users = [%q], owners = [%q], admins = \"none\" there", p, owner, owner)
	}
	var b strings.Builder
	b.Write(old)
	if len(old) > 0 && !bytes.HasSuffix(old, []byte("\n")) {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n# the admin worker (runs as SYSTEM): its owner only, written at its sign-in\n[access]\nusers = [%q]\nowners = [%q]\nadmins = \"none\"\n", owner, owner)
	return writeFileAtomic(p, []byte(b.String()))
}

// AdminWorkerTarget is the admin worker as a target of the user's sign-in
// (signin.Options.AdminWorker): its CSR comes from the hand-off dir, the
// approved enrollment goes back there, and the service installs it.
type AdminWorkerTarget struct {
	Handoff string // the hand-off dir
	DataDir string // the admin worker's data dir (reported only)
	// Wait: how long to wait for the service's request and for it to take
	// the enrollment (default 30 s and 60 s).
	WaitRequest, WaitInstall time.Duration
}

func (a AdminWorkerTarget) request() (*AdminRequest, error) {
	wait := a.WaitRequest
	if wait == 0 {
		wait = 30 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		b, err := os.ReadFile(filepath.Join(a.Handoff, AdminRequestFile))
		if err == nil {
			var r AdminRequest
			if err := json.Unmarshal(b, &r); err != nil {
				return nil, err
			}
			if r.CSR == "" {
				return nil, errors.New("the admin worker's request has no CSR")
			}
			return &r, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("the admin worker's sign-in request: %w (the admin worker is set up for the user who installed it)", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the admin worker service has not asked for a sign-in (no %s): is the %s service running? (sc query %s)",
				filepath.Join(a.Handoff, AdminRequestFile), AdminServiceName, AdminServiceName)
		}
		time.Sleep(time.Second)
	}
}

// Prepare checks that the admin worker asks for a sign-in; its CSR carries
// its own trust binding.
func (a AdminWorkerTarget) Prepare(string) (*pki.CSRTrust, error) {
	_, err := a.request()
	return nil, err
}

// CSR is the admin worker's CSR (signin.CSRSource).
func (a AdminWorkerTarget) CSR(string) ([]byte, error) {
	r, err := a.request()
	if err != nil {
		return nil, err
	}
	return []byte(r.CSR), nil
}

// Install hands the approved enrollment to the service and waits until it
// took it.
func (a AdminWorkerTarget) Install(hub string, r *hubclient.JoinResult, note func(string)) (string, error) {
	join, err := proto.Marshal(r.Resp)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(AdminResponse{Hub: hub, CAFingerprint: r.CAFingerprint, Join: join})
	os.Remove(filepath.Join(a.Handoff, AdminErrorFile))
	if err := writeFileAtomic(filepath.Join(a.Handoff, AdminResponseFile), b); err != nil {
		return "", err
	}
	wait := a.WaitInstall
	if wait == 0 {
		wait = time.Minute
	}
	deadline := time.Now().Add(wait)
	refused := func() error {
		if e, err := os.ReadFile(filepath.Join(a.Handoff, AdminErrorFile)); err == nil {
			return fmt.Errorf("the admin worker refused its enrollment: %s", strings.TrimSpace(string(e)))
		}
		return nil
	}
	for time.Now().Before(deadline) {
		if err := refused(); err != nil {
			return "", err
		}
		if installCheckHook != nil {
			installCheckHook()
		}
		if !fileExists(filepath.Join(a.Handoff, AdminResponseFile)) {
			// The service writes its error before it removes the response:
			// a refusal between the check above and this one is there now.
			if err := refused(); err != nil {
				return "", err
			}
			if note != nil {
				note(fmt.Sprintf("admin worker %s: runs as SYSTEM, only %s can use it; disable it in the tray or with: longarmd admin-worker disable (elevated)", r.Resp.Name, r.Resp.Owner))
			}
			return a.DataDir, nil
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("the admin worker service did not take its enrollment within %s (is the %s service running?)", wait, AdminServiceName)
}

// installCheckHook runs in Install between its error and response checks
// (tests: the service acting in that window).
var installCheckHook func()

// AdminInstall is what the MSI recorded about the admin worker
// (InstalledAdmin).
type AdminInstall struct {
	Enabled          bool
	Service          string
	DataDir, Handoff string
}

// AdminStatus: the admin worker's state for the tray and `longarmd
// admin-worker status`.
type AdminStatus struct {
	Enabled  bool   `json:"enabled"`
	Service  string `json:"service,omitempty"`
	State    string `json:"state,omitempty"` // running, stopped, not installed
	SignedIn bool   `json:"signed_in"`
	// Pending: the service waits for its sign-in (a request is there).
	Pending bool   `json:"pending"`
	Error   string `json:"error,omitempty"` // the last hand-off error
}

// AdminWorkerStatus reports the admin worker of a (readable without
// elevation: the hand-off dir is the installing user's too).
func AdminWorkerStatus(a AdminInstall, query func(string) (string, error)) AdminStatus {
	st := AdminStatus{Enabled: a.Enabled, Service: a.Service}
	if !a.Enabled {
		return st
	}
	if query != nil {
		if s, err := query(a.Service); err == nil {
			st.State = s
		} else {
			st.State = err.Error()
		}
	}
	_, reqErr := os.Stat(filepath.Join(a.Handoff, AdminRequestFile))
	st.Pending = reqErr == nil
	if e, err := os.ReadFile(filepath.Join(a.Handoff, AdminErrorFile)); err == nil {
		st.Error = strings.TrimSpace(string(e))
	}
	// the data dir isn't readable for the user: no request and the
	// service running means it is signed in
	st.SignedIn = !st.Pending && st.State == "running"
	return st
}

// adminSetupOut prints the admin worker's install notes.
func adminSetupOut(out io.Writer, dataDir, handoff string) {
	fmt.Fprintf(out, "admin worker: data dir %s (SYSTEM and Administrators only), sign-in hand-off %s\n", dataDir, handoff)
	fmt.Fprintln(out, "admin worker: runs as LocalSystem; what its owner runs on it runs as SYSTEM. The hub marks it longarm.io/admin=true: owner only, never shared, never placed automatically.")
}
