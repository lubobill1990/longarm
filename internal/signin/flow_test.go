package signin_test

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hub"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/signin"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// testHub runs a longarm-hub in-process (no Entra: panel passwords
// approve) with a Longarm user alice.
func testHub(t *testing.T) *hub.Hub {
	t.Helper()
	dir := t.TempDir()
	if _, err := hub.InitCA(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hub.toml")
	cfg := fmt.Sprintf("data_dir = %q\nlisten = \"127.0.0.1:0\"\nrelay_udp = \"off\"\nhttp = \"127.0.0.1:0\"\n[web]\ninsecure_cookie = true\n",
		filepath.ToSlash(dir))
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := hub.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.Open(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; h.Close() })
	for h.Addr("grpc") == "" || h.Addr("http") == "" {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "alice", Create: true}); err != nil {
		t.Fatal(err)
	}
	return h
}

// httpsPanel puts a TLS front (the Let's Encrypt panel in production) on
// the hub's HTTP listener; roots trust it.
func httpsPanel(t *testing.T, h *hub.Hub) (string, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: h.Addr("http")}))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	return srv.URL, roots
}

var (
	tokenRE = regexp.MustCompile(`name="token" value="([^"]+)"`)
	idRE    = regexp.MustCompile(`name="id" value="([^"]+)"`)
)

// approve signs in to the panel as alice (password + TOTP) and approves
// code at /link; it returns the confirmation page.
func approve(t *testing.T, h *hub.Hub, linkURL, code string) string {
	t.Helper()
	secret, err := hub.AddPanelUser(h.Store(), "alice-web", "correct horse battery", "user", "alice", true)
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSuffix(linkURL, "/link")
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, form url.Values) string {
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", base)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	totp, _ := hub.TOTP(secret, time.Now())
	post("/login", url.Values{"user": {"alice-web"}, "password": {"correct horse battery"}, "totp": {totp}})
	resp, err := c.Get(base + "/link")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := tokenRE.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("/link: %s", b)
	}
	page := post("/link", url.Values{"token": {m[1]}, "code": {code}})
	id := idRE.FindStringSubmatch(page)
	if id == nil {
		t.Fatalf("confirmation: %s", page)
	}
	if out := post("/link/decide", url.Values{"token": {tokenRE.FindStringSubmatch(page)[1]}, "id": {id[1]}, "decision": {"approve"}}); !strings.Contains(out, "Approved") {
		t.Fatalf("approve: %s", out)
	}
	return page
}

type recorder struct {
	mu    sync.Mutex
	steps []signin.Progress
	codes chan signin.Code
}

func (r *recorder) events() signin.Events {
	return signin.Events{
		Progress: func(p signin.Progress) { r.mu.Lock(); r.steps = append(r.steps, p); r.mu.Unlock() },
		Code:     func(c signin.Code) { r.codes <- c },
	}
}

func (r *recorder) saw(s signin.Step) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.steps {
		if p.Step == s {
			return p.Message
		}
	}
	return ""
}

// TestSignInBoth: the GUI's call - domain only (the CA from the HTTPS
// panel, the gRPC address from its well-known document), both roles, one
// code: the device certificate lands in the config dir, the owned worker
// in the data dir, the result names both.
func TestSignInBoth(t *testing.T) {
	h := testHub(t)
	panelURL, roots := httpsPanel(t, h)
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	rec := &recorder{codes: make(chan signin.Code, 2)}
	type out struct {
		r   *signin.Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := signin.SignIn(context.Background(), signin.Options{Hub: panelURL, Roots: roots,
			Client: client.DeviceTarget{ConfigDir: cfgDir}, Worker: longarmd.WorkerTarget{DataDir: dataDir},
			DeviceName: "lap", WorkerName: "lap-w"}, rec.events())
		done <- out{r, err}
	}()
	var c signin.Code
	select {
	case c = <-rec.codes:
	case o := <-done:
		t.Fatalf("no code: %v", o.err)
	}
	if c.Kind != "both" || c.URL != "http://"+h.Addr("http")+"/link" || time.Until(c.Expires) < 9*time.Minute {
		t.Fatalf("code: %+v", c)
	}
	page := approve(t, h, c.URL, c.Code)
	if !strings.Contains(page, "<code>lap</code>") || !strings.Contains(page, "<code>lap-w</code>") {
		t.Fatalf("confirmation: %s", page)
	}
	var o out
	select {
	case o = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("SignIn did not finish")
	}
	if o.err != nil {
		t.Fatal(o.err)
	}
	fp := pki.Fingerprint(h.CA().Cert)
	r := o.r
	if r.Hub != h.Addr("grpc") || r.CAFingerprint != fp || !r.CAFromHTTPS || r.User != "alice" || r.Method != signin.MethodLink ||
		r.Device == nil || r.Device.Name != "lap" || r.Device.Dir != cfgDir || r.Worker == nil || r.Worker.Name != "lap-w" ||
		r.Worker.Owner != "alice" || r.Worker.Dir != dataDir {
		t.Fatalf("result: %+v %+v %+v", r, r.Device, r.Worker)
	}
	if m := rec.saw(signin.StepCA); !strings.Contains(m, fp) || !strings.Contains(m, panelURL+"/ca.crt") {
		t.Fatalf("CA step: %q", m)
	}
	if rec.saw(signin.StepDone) == "" || !strings.Contains(rec.saw(signin.StepNote), "owned by alice") {
		t.Fatalf("steps: %+v", rec.steps)
	}
	for dir, want := range map[string]pki.Role{filepath.Join(cfgDir, client.TLSDir): pki.RoleClient, filepath.Join(dataDir, longarmd.TLSDir): pki.RoleWorker} {
		b, err := pki.LoadBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		if id, _ := pki.IdentityFromCert(b.Leaf()); id.Role != want {
			t.Fatalf("%s: %v", dir, id)
		}
	}
	if hc, _ := hubclient.LoadFile(dataDir); hc.Address != h.Addr("grpc") || hc.Owner != "alice" {
		t.Fatalf("worker hub.toml: %+v", hc)
	}

	// Signing in again over an enrolled worker needs Force; nothing is
	// asked of the hub before that check.
	_, err := signin.SignIn(context.Background(), signin.Options{Hub: panelURL, Roots: roots,
		Worker: longarmd.WorkerTarget{DataDir: dataDir}}, signin.Events{})
	if err == nil || !strings.Contains(err.Error(), "already holds a certificate") {
		t.Fatalf("enrolled again: %v", err)
	}
}

// TestSignInCancel: canceling the context ends a pairing.
func TestSignInCancel(t *testing.T) {
	h := testHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recorder{codes: make(chan signin.Code, 1)}
	done := make(chan error, 1)
	go func() {
		_, err := signin.SignIn(ctx, signin.Options{Hub: h.Addr("grpc"), Fingerprint: pki.Fingerprint(h.CA().Cert),
			Client: client.DeviceTarget{ConfigDir: t.TempDir()}}, rec.events())
		done <- err
	}()
	select {
	case <-rec.codes:
	case err := <-done:
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("SignIn ignored the cancellation")
	}
	if rec.saw(signin.StepCA) == "" || !strings.Contains(rec.saw(signin.StepCA), "given") {
		t.Fatalf("steps: %+v", rec.steps)
	}
	// no hub: nothing to do
	if _, err := signin.SignIn(context.Background(), signin.Options{Hub: h.Addr("grpc")}, signin.Events{}); err == nil {
		t.Fatal("no targets accepted")
	}
}

// adminTarget is an admin worker's target as the tray holds it: a CSR
// made elsewhere (its key stays with the LocalSystem longarmd), the
// certificate handed back.
type adminTarget struct {
	keyPEM, csrPEM []byte
	got            *hubclient.JoinResult
}

func newAdminTarget(t *testing.T) *adminTarget {
	k, c, err := pki.NewKeyAndCSR("", nil)
	if err != nil {
		t.Fatal(err)
	}
	return &adminTarget{keyPEM: k, csrPEM: c}
}

func (a *adminTarget) Prepare(string) (*pki.CSRTrust, error) { return nil, nil }
func (a *adminTarget) CSR(string) ([]byte, error)            { return a.csrPEM, nil }
func (a *adminTarget) Install(_ string, r *hubclient.JoinResult, _ func(string)) (string, error) {
	if r.KeyPEM != nil {
		return "", errors.New("a key for a pre-made CSR")
	}
	a.got = r
	return "admin-dir", nil
}

// TestSignInAdminWorker: the admin worker rides along on the one code
// (Code.AdminWorker says so), comes back with KeyPEM nil for its own key,
// and is marked; az can't sign one in; alone it is a kind "admin" pairing.
func TestSignInAdminWorker(t *testing.T) {
	h := testHub(t)
	fp := pki.Fingerprint(h.CA().Cert)
	if _, err := signin.SignIn(context.Background(), signin.Options{Hub: h.Addr("grpc"), Fingerprint: fp, Method: signin.MethodAz,
		Worker: longarmd.WorkerTarget{DataDir: t.TempDir()}, AdminWorker: newAdminTarget(t)}, signin.Events{}); err == nil ||
		!strings.Contains(err.Error(), "browser only") {
		t.Fatalf("az with an admin worker: %v", err)
	}
	for _, alone := range []bool{false, true} {
		h := testHub(t) // a fresh panel login each time
		fp := pki.Fingerprint(h.CA().Cert)
		at := newAdminTarget(t)
		o := signin.Options{Hub: h.Addr("grpc"), Fingerprint: fp, AdminWorker: at, Method: signin.MethodAuto}
		if !alone {
			o.Worker, o.WorkerName = longarmd.WorkerTarget{DataDir: t.TempDir()}, "dev-w"
		}
		rec := &recorder{codes: make(chan signin.Code, 1)}
		done := make(chan error, 1)
		var r *signin.Result
		go func() {
			var err error
			r, err = signin.SignIn(context.Background(), o, rec.events())
			done <- err
		}()
		var c signin.Code
		select {
		case c = <-rec.codes:
		case err := <-done:
			t.Fatalf("no code: %v", err)
		}
		if !c.AdminWorker || (alone && c.Kind != "admin") || (!alone && c.Kind != "worker") {
			t.Fatalf("code: %+v", c)
		}
		page := approve(t, h, c.URL, c.Code)
		if !strings.Contains(page, "ADMINISTRATOR worker") {
			t.Fatalf("confirmation: %s", page)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("SignIn did not finish")
		}
		want := "dev-w-admin"
		if alone {
			want = strings.ToLower(strings.SplitN(hostnameOf(t), ".", 2)[0]) + "-admin"
		}
		if r.AdminWorker == nil || r.AdminWorker.Name != want || r.AdminWorker.Dir != "admin-dir" || at.got == nil ||
			at.got.Resp.Labels["longarm.io/admin"] != "true" || r.User != "alice" || !strings.Contains(rec.saw(signin.StepDone), "admin worker "+want) {
			t.Fatalf("alone %v: %+v %+v", alone, r, r.AdminWorker)
		}
		if alone == (r.Worker != nil) {
			t.Fatalf("worker part: %+v", r.Worker)
		}
	}
}

func hostnameOf(t *testing.T) string {
	h, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	return h
}
