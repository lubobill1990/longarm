package tray

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/sched"
	"github.com/lubobill1990/longarm/internal/signin"
)

// The tray's "Sign in with Microsoft" (/trayapi/signin/...): signin.SignIn
// with the targets this machine has - the user's client device, the
// user-session worker (its data dir is the user's), the admin worker (its
// hand-off dir) - one code, one approval. The page polls the state.

// SignInState is what /trayapi/signin/state reports.
type SignInState struct {
	// State: "idle", "running", "done", "error" or "canceled".
	State   string       `json:"state"`
	Hub     string       `json:"hub,omitempty"`
	Code    *SignInCode  `json:"code,omitempty"`
	Steps   []string     `json:"steps,omitempty"`
	Result  *SignInDone  `json:"result,omitempty"`
	Error   string       `json:"error,omitempty"`
	Started time.Time    `json:"started,omitempty"`
	Targets SignInTarget `json:"targets"`
}

// SignInCode is the pairing code to show.
type SignInCode struct {
	URL         string    `json:"url"`
	Code        string    `json:"code"`
	Expires     time.Time `json:"expires"`
	Kind        string    `json:"kind"`
	Host        string    `json:"host"`
	AdminWorker bool      `json:"admin_worker,omitempty"`
}

// SignInDone is a finished sign-in.
type SignInDone struct {
	User        string `json:"user,omitempty"`
	Hub         string `json:"hub"`
	Device      string `json:"device,omitempty"`
	Worker      string `json:"worker,omitempty"`
	AdminWorker string `json:"admin_worker,omitempty"`
	Summary     string `json:"summary"`
}

// SignInTarget: what a sign-in enrolls.
type SignInTarget struct {
	Client      bool `json:"client"`
	Worker      bool `json:"worker"`
	AdminWorker bool `json:"admin_worker"`
}

type signinRun struct {
	mu     sync.Mutex
	st     SignInState
	cancel context.CancelFunc
}

func (r *signinRun) snapshot() SignInState {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.st
	st.Steps = append([]string(nil), r.st.Steps...)
	if st.State == "" {
		st.State = "idle"
	}
	return st
}

func (r *signinRun) update(f func(*SignInState)) {
	r.mu.Lock()
	f(&r.st)
	r.mu.Unlock()
}

// workerSignIn reports whether the tray can sign the worker in itself: a
// worker role, and its data dir writable for this user (the user-session
// worker's; a service's data dir needs `longarmd login` elevated).
func (s *Server) workerSignIn() bool {
	if _, worker := s.installedRoles(); !worker {
		return false
	}
	return dirWritable(s.opts.LongarmdDataDir)
}

// installedRoles: the roles the installer recorded, else Roles().
func (s *Server) installedRoles() (client, worker bool) {
	switch s.roles() {
	case "client":
		return true, false
	case "worker":
		return false, true
	case "both":
		return true, true
	}
	return s.Roles()
}

func (s *Server) roles() string {
	if s.opts.Roles != "" {
		c, w := strings.Contains(s.opts.Roles, "client"), strings.Contains(s.opts.Roles, "worker")
		switch {
		case c && w:
			return "both"
		case w:
			return "worker"
		}
		return "client"
	}
	if s.installedRolesFn != nil {
		return s.installedRolesFn()
	}
	return ""
}

func dirWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".longarm-tray-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

func exists(p ...string) bool { _, err := os.Stat(filepath.Join(p...)); return err == nil }

// Account is the "signed in as" part of /trayapi/info.
type Account struct {
	SignedIn bool   `json:"signed_in"`
	User     string `json:"user,omitempty"`
	Hub      string `json:"hub,omitempty"`
	// HubFrom: where the hub to sign in to comes from: "config" (this
	// machine's hub.toml), "installer", "built-in" or "" (none: ask).
	HubFrom string `json:"hub_from,omitempty"`
	// Needed: a sign-in would enroll something (Targets).
	Needed  bool         `json:"needed"`
	Targets SignInTarget `json:"targets"`
	// RunMode of the worker: "session", "service" or "".
	RunMode string                `json:"run_mode,omitempty"`
	Admin   *longarmd.AdminStatus `json:"admin,omitempty"`
}

// account describes this machine's sign-in.
func (s *Server) account() Account {
	var a Account
	cfgDir, dataDir := s.opts.LongarmConfigDir, s.opts.LongarmdDataDir
	clientCert := filepath.Join(cfgDir, client.TLSDir, pki.NodeCertFile)
	if b, err := os.ReadFile(clientCert); err == nil {
		if c, err := pki.ParseCertPEM(b); err == nil {
			if id, err := pki.IdentityFromCert(c); err == nil {
				a.SignedIn, a.User = true, id.Owner()
			}
		}
	}
	workerCert := exists(dataDir, longarmd.TLSDir, pki.NodeCertFile)
	if hf, err := hubclient.LoadFile(dataDir); err == nil && workerCert {
		a.SignedIn = true
		if a.User == "" {
			a.User = hf.Owner
		}
	}
	a.Hub, _, a.HubFrom = s.signinHub()
	wantClient, _ := s.installedRoles()
	t := s.targets()
	a.Targets = t
	a.Needed = (wantClient && t.Client && !exists(clientCert)) || (t.Worker && !workerCert) || t.AdminWorker
	if s.installedWorker != nil {
		mode, adm := s.installedWorker()
		a.RunMode = mode
		if adm.Enabled {
			st := longarmd.AdminWorkerStatus(adm, queryService)
			a.Admin = &st
		}
	}
	return a
}

func queryService(name string) (string, error) {
	st, err := platform.QueryService(name)
	if err != nil {
		return "", err
	}
	return string(st), nil
}

// targets is what a sign-in would enroll now.
func (s *Server) targets() SignInTarget {
	c, _ := s.installedRoles()
	t := SignInTarget{Client: c, Worker: s.workerSignIn()}
	if !t.Client && !t.Worker {
		t.Client = true
	}
	if s.installedWorker != nil {
		if _, adm := s.installedWorker(); adm.Enabled && adm.Handoff != "" {
			t.AdminWorker = exists(adm.Handoff, longarmd.AdminRequestFile)
		}
	}
	return t
}

// signinHub is the hub a sign-in goes to: the worker's hub.toml (a sign-in
// to come), the client's, the installer's, the build's default.
func (s *Server) signinHub() (hub, fp, from string) {
	for _, dir := range []string{s.opts.LongarmdDataDir, s.opts.LongarmConfigDir} {
		if hf, err := hubclient.LoadFile(dir); err == nil && hf.Address != "" {
			return hf.Address, hf.CAFingerprint, "config"
		}
	}
	return signin.FallbackHub()
}

// signinStart starts a sign-in (one at a time).
func (s *Server) signinStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hub    string `json:"hub"`
		Labels string `json:"labels"`
	}
	if !readBody(w, r, &body) {
		return
	}
	body.Hub = strings.TrimSpace(body.Hub)
	if len(body.Hub) > 256 || strings.ContainsAny(body.Hub, " \t\"'`;&|") {
		httpError(w, http.StatusBadRequest, "bad hub address")
		return
	}
	var labels map[string]string
	if body.Labels != "" {
		m, err := sched.ParseLabels([]string{body.Labels}, false)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		labels = m
	}
	hub, fp, _ := s.signinHub()
	if body.Hub != "" {
		hub, fp = body.Hub, ""
	}
	if hub == "" {
		httpError(w, http.StatusBadRequest, "which hub? enter its address (e.g. longarm.example.com)")
		return
	}
	t := s.targets()
	so := signin.Options{Hub: hub, Fingerprint: fp, OpenBrowser: true, WorkerLabels: labels}
	if t.Client {
		so.Client = client.DeviceTarget{ConfigDir: s.opts.LongarmConfigDir}
	}
	if t.Worker {
		so.Worker = longarmd.WorkerTarget{DataDir: s.opts.LongarmdDataDir}
	}
	if t.AdminWorker {
		_, adm := s.installedWorker()
		so.AdminWorker = longarmd.AdminWorkerTarget{Handoff: adm.Handoff, DataDir: adm.DataDir}
	}
	run := &s.signin
	run.mu.Lock()
	if run.st.State == "running" {
		run.mu.Unlock()
		httpError(w, http.StatusConflict, "a sign-in is already running")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	run.st = SignInState{State: "running", Hub: hub, Started: time.Now(), Targets: t}
	run.mu.Unlock()
	go s.runSignIn(ctx, so)
	writeJSON(w, run.snapshot())
}

func (s *Server) runSignIn(ctx context.Context, so signin.Options) {
	run := &s.signin
	defer run.cancel()
	ev := signin.Events{
		Progress: func(p signin.Progress) {
			run.update(func(st *SignInState) {
				st.Steps = append(st.Steps, p.Message)
				if len(st.Steps) > 30 {
					st.Steps = st.Steps[len(st.Steps)-30:]
				}
			})
		},
		Code: func(c signin.Code) {
			run.update(func(st *SignInState) {
				st.Code = &SignInCode{URL: c.URL, Code: c.Code, Expires: c.Expires, Kind: c.Kind, Host: c.Host, AdminWorker: c.AdminWorker}
			})
		},
	}
	signIn := s.signIn
	if signIn == nil {
		signIn = signin.SignIn
	}
	res, err := signIn(ctx, so, ev)
	run.update(func(st *SignInState) {
		st.Code = nil
		switch {
		case errors.Is(err, context.Canceled):
			st.State, st.Error = "canceled", "canceled"
		case err != nil:
			st.State, st.Error = "error", err.Error()
		default:
			st.State = "done"
			d := &SignInDone{User: res.User, Hub: res.Hub, Summary: res.Summary()}
			if res.Device != nil {
				d.Device = res.Device.Name
			}
			if res.Worker != nil {
				d.Worker = res.Worker.Name
			}
			if res.AdminWorker != nil {
				d.AdminWorker = res.AdminWorker.Name
			}
			st.Result = d
		}
	})
	if err == nil && so.Client != nil && s.afterSignIn != nil {
		s.afterSignIn() // the client's daemon picks the certificate up
	}
}

func (s *Server) signinState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.signin.snapshot())
}

func (s *Server) signinCancel(w http.ResponseWriter, _ *http.Request) {
	s.signin.mu.Lock()
	if s.signin.cancel != nil {
		s.signin.cancel()
	}
	s.signin.mu.Unlock()
	writeJSON(w, map[string]bool{"ok": true})
}

// signout leaves the hub: the client (longarm leave) and the user-session
// worker (longarmd leave: its host then waits for the next sign-in). The
// admin worker stays (disable it separately).
func (s *Server) signout(w http.ResponseWriter, _ *http.Request) {
	var out []string
	var errs []string
	if exists(s.opts.LongarmConfigDir, client.TLSDir, pki.NodeCertFile) {
		s.runCmd(s.opts.LongarmExe, s.longarmArgs("daemon", "stop")[1:]...)
		o, err := s.runCmd(s.opts.LongarmExe, s.longarmArgs("leave")[1:]...)
		out = append(out, strings.TrimSpace(string(o)))
		if err != nil {
			errs = append(errs, "longarm leave: "+err.Error())
		}
	}
	if s.workerSignIn() && exists(s.opts.LongarmdDataDir, longarmd.TLSDir, pki.NodeCertFile) {
		o, err := s.runCmd(s.opts.LongarmdExe, "leave", "--data-dir", s.opts.LongarmdDataDir)
		out = append(out, strings.TrimSpace(string(o)))
		if err != nil {
			errs = append(errs, "longarmd leave: "+err.Error())
		}
	}
	if len(errs) > 0 {
		httpError(w, http.StatusInternalServerError, strings.Join(append(errs, out...), "\n"))
		return
	}
	writeJSON(w, map[string]string{"output": strings.Join(out, "\n")})
}

// adminDisable removes the admin worker: `longarmd admin-worker disable`,
// elevated (a UAC prompt).
func (s *Server) adminDisable(w http.ResponseWriter, _ *http.Request) {
	if s.installedWorker == nil {
		httpError(w, http.StatusNotFound, "no admin worker")
		return
	}
	if _, adm := s.installedWorker(); !adm.Enabled {
		httpError(w, http.StatusNotFound, "no admin worker")
		return
	}
	if err := s.runElevated(s.opts.LongarmdExe, "admin-worker", "disable"); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"output": "confirm the administrator prompt; the admin worker is then removed"})
}
