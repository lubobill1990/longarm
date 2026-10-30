package tray

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/signin"
)

// TestSignInAPI: the tray's "Sign in with Microsoft" signs in what this
// machine has (the user's device, the user-session worker whose data dir
// is writable, the admin worker when it asks), shows the code and the
// progress, can be canceled, reports errors, and only one runs at a time.
func TestSignInAPI(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	s, err := NewServer(Options{LongarmConfigDir: cfg, LongarmdDataDir: data, LongarmExe: "longarm", LongarmdExe: "longarmd"}, &fakeDesk{}, "")
	if err != nil {
		t.Fatal(err)
	}
	s.installedRolesFn = func() string { return "both" }
	s.installedWorker = func() (string, longarmd.AdminInstall) { return "session", longarmd.AdminInstall{} }
	s.runCmd = func(string, ...string) ([]byte, error) { return nil, nil }
	var mu sync.Mutex
	var got signin.Options
	release := make(chan error)
	s.signIn = func(ctx context.Context, o signin.Options, ev signin.Events) (*signin.Result, error) {
		mu.Lock()
		got = o
		mu.Unlock()
		ev.Progress(signin.Progress{Step: signin.StepResolve, Message: "looking up the hub"})
		ev.Code(signin.Code{URL: "https://longarm.example.com/link", Code: "BCDF-GHJK", Expires: time.Now().Add(10 * time.Minute), Kind: "both", Host: "box"})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-release:
			if err != nil {
				return nil, err
			}
		}
		return &signin.Result{Hub: "longarm.example.com:7443", User: "alice",
			Device: &signin.Enrolled{Name: "alice-box"}, Worker: &signin.Enrolled{Name: "box"}}, nil
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
	base := "http://" + s.Addr()
	call := func(method, path, body string, v any) int {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
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
	a := info.Account
	if !a.Needed || !a.Targets.Client || !a.Targets.Worker || a.Targets.AdminWorker || a.SignedIn || a.RunMode != "session" {
		t.Fatalf("account %+v", a)
	}
	// no hub anywhere (unless this machine's MSI or build has one): asked for
	if a.Hub == "" {
		if code := call("POST", "/trayapi/signin/start", `{}`, nil); code != 400 {
			t.Fatalf("start without a hub: %d", code)
		}
	}
	if code := call("POST", "/trayapi/signin/start", `{"hub":"bad hub;"}`, nil); code != 400 {
		t.Fatalf("bad hub accepted: %d", code)
	}
	if code := call("POST", "/trayapi/signin/start", `{"hub":"longarm.example.com","labels":"pool=devbox"}`, nil); code != 200 {
		t.Fatalf("start: %d", code)
	}
	if code := call("POST", "/trayapi/signin/start", `{"hub":"longarm.example.com"}`, nil); code != 409 {
		t.Fatalf("second start: %d", code)
	}
	var st SignInState
	waitState := func(want func() bool) {
		t.Helper()
		for i := 0; i < 100; i++ {
			st = SignInState{}
			call("GET", "/trayapi/signin/state", "", &st)
			if want() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("state %+v", st)
	}
	waitState(func() bool { return st.Code != nil })
	if st.State != "running" || st.Code.Code != "BCDF-GHJK" || len(st.Steps) == 0 || !st.Targets.Worker {
		t.Fatalf("running state %+v", st)
	}
	mu.Lock()
	if got.Hub != "longarm.example.com" || got.Client == nil || got.Worker == nil || got.AdminWorker != nil || !got.OpenBrowser || got.WorkerLabels["pool"] != "devbox" {
		t.Errorf("options %+v", got)
	}
	mu.Unlock()
	release <- nil
	waitState(func() bool { return st.State == "done" })
	if st.Result == nil || st.Result.User != "alice" || st.Result.Worker != "box" || st.Code != nil {
		t.Fatalf("done %+v", st)
	}

	// an error, then a cancel
	call("POST", "/trayapi/signin/start", `{"hub":"longarm.example.com"}`, nil)
	waitState(func() bool { return st.Code != nil })
	release <- errors.New("the pairing was denied")
	waitState(func() bool { return st.State == "error" })
	if !strings.Contains(st.Error, "denied") {
		t.Fatalf("error %+v", st)
	}
	call("POST", "/trayapi/signin/start", `{"hub":"longarm.example.com"}`, nil)
	waitState(func() bool { return st.Code != nil })
	call("POST", "/trayapi/signin/cancel", `{}`, nil)
	waitState(func() bool { return st.State == "canceled" })

	// the admin worker joins the sign-in when its service asks
	s.installedWorker = func() (string, longarmd.AdminInstall) {
		return "session", longarmd.AdminInstall{Enabled: true, Service: "none-such-service", Handoff: data, DataDir: data}
	}
	if err := os.WriteFile(filepath.Join(data, longarmd.AdminRequestFile), []byte(`{"csr_pem":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	call("GET", "/trayapi/info", "", &info)
	if !info.Account.Targets.AdminWorker || info.Account.Admin == nil || !info.Account.Admin.Pending {
		t.Fatalf("admin account %+v %+v", info.Account, info.Account.Admin)
	}
	call("POST", "/trayapi/signin/start", `{"hub":"longarm.example.com"}`, nil)
	waitState(func() bool { return st.Code != nil })
	mu.Lock()
	if _, ok := got.AdminWorker.(longarmd.AdminWorkerTarget); !ok {
		t.Errorf("admin target %T", got.AdminWorker)
	}
	mu.Unlock()
	call("POST", "/trayapi/signin/cancel", `{}`, nil)
	waitState(func() bool { return st.State == "canceled" })
}

// TestSignInConfirmReplace: on a machine signed in already (a join code,
// another account) the sign-in stops in state "confirm" naming what it
// would replace - nothing is replaced silently; "replace" starts it again
// with signin.Options.Replace (and the device's Force for another hub);
// "keep" ends the question.
func TestSignInConfirmReplace(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	s, err := NewServer(Options{LongarmConfigDir: cfg, LongarmdDataDir: data, LongarmExe: "longarm", LongarmdExe: "longarmd"}, &fakeDesk{}, "")
	if err != nil {
		t.Fatal(err)
	}
	s.installedRolesFn = func() string { return "both" }
	s.installedWorker = func() (string, longarmd.AdminInstall) { return "session", longarmd.AdminInstall{} }
	var mu sync.Mutex
	var calls []signin.Options
	s.signIn = func(ctx context.Context, o signin.Options, ev signin.Events) (*signin.Result, error) {
		mu.Lock()
		calls = append(calls, o)
		mu.Unlock()
		if !o.Replace {
			return nil, &signin.SignedInError{Hub: "longarm.example.com:7443", CAFingerprint: "ab", Current: []*signin.Current{
				{Kind: "device", Role: "admin", Name: "alice-box", User: "alice", CAFingerprint: "ab", NotAfter: time.Now().Add(time.Hour)},
				{Kind: "worker", Role: "worker", Name: "box", User: "alice", CAFingerprint: "ab", NotAfter: time.Now().Add(time.Hour)},
			}}
		}
		return &signin.Result{Hub: "longarm.example.com:7443", User: "alice2", Device: &signin.Enrolled{Name: "alice2-box"}, Worker: &signin.Enrolled{Name: "box"}}, nil
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
		req.Header.Set("Content-Type", "application/json")
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
	var st SignInState
	waitState := func(want string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			st = SignInState{}
			call("GET", "/trayapi/signin/state", "", &st)
			if st.State == want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("state %+v, want %s", st, want)
	}
	if code := call("POST", "/trayapi/signin/start", `{"hub":"longarm.example.com"}`, nil); code != 200 {
		t.Fatalf("start: %d", code)
	}
	waitState("confirm")
	cf := st.Confirm
	if cf == nil || cf.Hub != "longarm.example.com:7443" || cf.OtherHub || len(cf.Current) != 2 || cf.Current[0].Identity != "admin/alice-box" ||
		!cf.Current[0].SameHub || cf.Current[1].Kind != "worker" || cf.Request.Hub != "longarm.example.com" {
		t.Fatalf("confirm %+v", cf)
	}
	// keep: the question goes away, nothing else ran
	call("POST", "/trayapi/signin/cancel", `{}`, nil)
	waitState("idle")
	// replace: the same sign-in, now with Replace
	call("POST", "/trayapi/signin/start", `{"hub":"longarm.example.com"}`, nil)
	waitState("confirm")
	if code := call("POST", "/trayapi/signin/start", `{"hub":"longarm.example.com","replace":true}`, nil); code != 200 {
		t.Fatalf("replace: %d", code)
	}
	waitState("done")
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 || calls[0].Replace || calls[1].Replace || !calls[2].Replace {
		t.Fatalf("calls %+v", calls)
	}
	if d, ok := calls[2].Client.(client.DeviceTarget); !ok || !d.Force {
		t.Fatalf("device target %+v", calls[2].Client)
	}
	if _, ok := calls[2].Worker.(longarmd.WorkerTarget); !ok {
		t.Fatalf("worker target %T", calls[2].Worker)
	}
}
