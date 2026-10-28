package tray

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/signin"
)

// TestRolesAPI: the Account tab's role change - "Also run jobs on this
// computer" on a client - runs the installed longarmd's `install-helper
// roles` elevated with the tray user's SID, then signs in only what was
// added (the worker), with one approval; refusals (admin worker without
// the worker, nothing to change) never reach the administrator prompt, and
// a declined prompt changes nothing.
func TestRolesAPI(t *testing.T) {
	const me = "S-1-12-1-111-222-333-444"
	cfg, data := t.TempDir(), t.TempDir()
	s, err := NewServer(Options{LongarmConfigDir: cfg, LongarmdDataDir: data, LongarmExe: "longarm", LongarmdExe: `C:\Program Files\Longarm\longarmd.exe`}, &fakeDesk{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// the client's hub (where the added worker signs in)
	if err := hubclient.SaveFile(cfg, hubclient.Config{Address: "longarm.example.com:7443"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	machine := longarmd.MachineRoles{Installed: true, Client: true, SessionSID: me, RunMode: "session"}
	var elevated [][]string
	var elevErr error
	var signedIn []signin.Options
	s.machineRoles = func() longarmd.MachineRoles { mu.Lock(); defer mu.Unlock(); return machine }
	s.installedRolesFn = func() string {
		mu.Lock()
		defer mu.Unlock()
		return longarmd.RolesString(machine.Client, machine.Worker)
	}
	s.installedWorker = func() (string, longarmd.AdminInstall) { return "session", longarmd.AdminInstall{} }
	s.currentSID = func() (string, error) { return me, nil }
	s.cloudPC = func() string { return "the computer name CPC-alice-1 is a Cloud PC's / Dev Box's" }
	s.rolesResult = func() string { return "ok" }
	s.runCmd = func(string, ...string) ([]byte, error) { return nil, nil }
	s.runElevatedWait = func(exe string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		elevated = append(elevated, append([]string{exe}, args...))
		if elevErr != nil {
			return elevErr
		}
		d, err := longarmd.MSIData(strings.Join(args, " "), longarmd.RolesKeys...)
		if err != nil {
			return err
		}
		machine.Client, machine.Worker, machine.Admin = d["client"] == "1", d["worker"] == "1", d["admin"] == "1"
		return nil
	}
	s.signIn = func(ctx context.Context, o signin.Options, ev signin.Events) (*signin.Result, error) {
		mu.Lock()
		signedIn = append(signedIn, o)
		mu.Unlock()
		return &signin.Result{Hub: "h:7443", User: "alice", Worker: &signin.Enrolled{Name: "box"}, WorkerKind: o.WorkerKind}, nil
	}
	go s.Serve()
	defer s.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if resp, err := c.Get(s.URL("/tray")); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	call := func(method, path, body string, v any) int {
		t.Helper()
		req, _ := http.NewRequest(method, "http://"+s.Addr()+path, strings.NewReader(body))
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if v != nil {
			json.NewDecoder(resp.Body).Decode(v)
		}
		return resp.StatusCode
	}
	var info struct{ Account Account }
	call("GET", "/trayapi/info", "", &info)
	if a := info.Account; !a.RolesEditable || !a.Roles.Client || a.Roles.Worker || a.CloudPC == "" {
		t.Fatalf("account %+v", a)
	}
	// refused before any prompt
	if code := call("POST", "/trayapi/roles", `{"client":true,"admin_worker":true}`, nil); code != 400 {
		t.Fatalf("admin worker without worker: %d", code)
	}
	if code := call("POST", "/trayapi/roles", `{"client":true}`, nil); code != 400 {
		t.Fatalf("no change: %d", code)
	}
	// a declined prompt
	elevErr = ErrElevationCanceled
	var st RolesState
	wait := func(want string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			st = RolesState{}
			call("GET", "/trayapi/roles/state", "", &st)
			if st.State == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("roles state %+v, want %s", st, want)
	}
	if code := call("POST", "/trayapi/roles", `{"client":true,"worker":true}`, nil); code != 200 {
		t.Fatalf("change: %d", code)
	}
	wait("error")
	if !strings.Contains(st.Error, "nothing changed") || machine.Worker {
		t.Fatalf("declined: %+v %+v", st, machine)
	}
	// "Also run jobs on this computer"
	elevErr = nil
	call("POST", "/trayapi/roles", `{"client":true,"worker":true}`, nil)
	wait("done")
	mu.Lock()
	last := elevated[len(elevated)-1]
	mu.Unlock()
	if last[0] != `C:\Program Files\Longarm\longarmd.exe` || strings.Join(last[1:], " ") != "install-helper roles "+longarmd.MSIDataMarker+"client=1|worker=1|admin=|sid="+me {
		t.Fatalf("elevated %q", last)
	}
	if !st.SignIn || !machine.Worker {
		t.Fatalf("done %+v", st)
	}
	for i := 0; i < 100 && s.SigningIn(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	if len(signedIn) != 1 || signedIn[0].Worker == nil || signedIn[0].Client != nil || signedIn[0].WorkerKind != "user session" {
		t.Fatalf("sign-in of the added worker only: %+v", signedIn)
	}
	mu.Unlock()
	// the worker now waits for its sign-in in this user's data dir
	if b, err := os.ReadFile(filepath.Join(data, "hub.toml")); err == nil && !strings.Contains(string(b), "sign_in") {
		t.Errorf("hub.toml %s", b)
	}
	var ss SignInState
	call("GET", "/trayapi/signin/state", "", &ss)
	if ss.Result == nil || ss.Result.Summary != "Signed in as alice: worker box (user session)" {
		t.Fatalf("summary %+v", ss.Result)
	}
}

func (s *Server) SigningIn() bool { return s.signin.snapshot().State == "running" }

func writeTestCert(t *testing.T, path string, notAfter time.Time) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "alice-box"},
		NotBefore: notAfter.Add(-30 * 24 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestExpiredSignIn: a certificate past its NotAfter is not "signed in": the
// account says expired (and when), a sign-in is needed, and a notification
// comes three days before and after.
func TestExpiredSignIn(t *testing.T) {
	cfg := t.TempDir()
	s, err := NewServer(Options{LongarmConfigDir: cfg, LongarmdDataDir: t.TempDir(), Roles: "client"}, &fakeDesk{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	writeTestCert(t, filepath.Join(cfg, "tls", "node.crt"), now.Add(-time.Hour))
	a := s.account()
	if a.SignedIn || !a.Expired || a.Expires == nil || !a.Needed {
		t.Fatalf("expired: %+v", a)
	}
	if title, body, ok := ExpiryNotice(a, now); !ok || !strings.Contains(title, "sign in again") || !strings.Contains(body, "expired on") {
		t.Fatalf("notice %q %q", title, body)
	}
	writeTestCert(t, filepath.Join(cfg, "tls", "node.crt"), now.Add(48*time.Hour))
	a = s.account()
	if a.Expired {
		t.Fatalf("valid: %+v", a)
	}
	if _, body, ok := ExpiryNotice(a, now); !ok || !strings.Contains(body, "could not be renewed") {
		t.Fatalf("soon: %q", body)
	}
	if _, _, ok := ExpiryNotice(a, now.Add(-10*24*time.Hour)); ok {
		t.Fatal("notice ten days early")
	}
}

// TestWindowPrefs: the popup stays open by default (no auto-hide), the
// choice is remembered; FirstStart is true once per version.
func TestWindowPrefs(t *testing.T) {
	dir := t.TempDir()
	if LoadWindowPrefs(dir).AutoHide {
		t.Fatal("auto-hide by default")
	}
	if err := SaveWindowPrefs(dir, WindowPrefs{AutoHide: true}); err != nil || !LoadWindowPrefs(dir).AutoHide {
		t.Fatalf("saved: %v", err)
	}
	if !FirstStart(dir, "0.1.4") || FirstStart(dir, "0.1.4") || !FirstStart(dir, "0.1.5") {
		t.Fatal("first start")
	}
}
