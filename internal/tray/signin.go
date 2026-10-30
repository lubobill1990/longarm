package tray

import (
	"context"
	"crypto/x509"
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
	// State: "idle", "running", "done", "error", "canceled" or "confirm"
	// (this machine is signed in already: Confirm says as what; the page
	// asks, then starts again with replace).
	State   string         `json:"state"`
	Confirm *SignInConfirm `json:"confirm,omitempty"`
	Hub     string         `json:"hub,omitempty"`
	Code    *SignInCode    `json:"code,omitempty"`
	Steps   []string       `json:"steps,omitempty"`
	Result  *SignInDone    `json:"result,omitempty"`
	Error   string         `json:"error,omitempty"`
	Started time.Time      `json:"started,omitempty"`
	Targets SignInTarget   `json:"targets"`
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

// SignInConfirm: what a sign-in would replace (signin.SignedInError).
type SignInConfirm struct {
	Hub string `json:"hub"`
	// OtherHub: something is signed in to another hub (it stays listed
	// there; only this machine's certificate is replaced).
	OtherHub bool         `json:"other_hub,omitempty"`
	Current  []SignInHeld `json:"current"`
	// AdminWorkerStays: the admin worker is signed in and not part of
	// this sign-in: it stays its current owner's.
	AdminWorkerStays bool `json:"admin_worker_stays,omitempty"`
	// Request: the hub and labels asked for, to start again with.
	Request SignInRequest `json:"request"`
}

// SignInHeld is one enrollment this machine holds.
type SignInHeld struct {
	Kind     string `json:"kind"`     // "device" or "worker"
	Identity string `json:"identity"` // "admin/alice-pc"
	User     string `json:"user,omitempty"`
	Hub      string `json:"hub,omitempty"`
	SameHub  bool   `json:"same_hub"`
	Expired  bool   `json:"expired,omitempty"`
}

// SignInRequest is a sign-in's input.
type SignInRequest struct {
	Hub    string `json:"hub,omitempty"`
	Labels string `json:"labels,omitempty"`
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
	only   *SignInTarget // the running / to-be-confirmed sign-in's
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
	// Expires: when this machine's sign-in (the client device's
	// certificate, else the worker's) ends; Expired: it has (SignedIn is
	// then false and a sign-in is Needed).
	Expires *time.Time `json:"expires,omitempty"`
	Expired bool       `json:"expired,omitempty"`
	// Roles: what this computer is (the installer's record, changed in the
	// Account tab when RolesEditable); RolesNote says why a role can't
	// change.
	Roles         AccountRoles `json:"roles"`
	RolesEditable bool         `json:"roles_editable"`
	RolesNote     string       `json:"roles_note,omitempty"`
	// CloudPC: why this machine looks like a Cloud PC / Dev Box (it
	// usually runs jobs: the tab suggests the worker role).
	CloudPC string `json:"cloud_pc,omitempty"`
}

// AccountRoles: send jobs (client), run jobs (worker), admin commands.
type AccountRoles struct {
	Client      bool `json:"client"`
	Worker      bool `json:"worker"`
	AdminWorker bool `json:"admin_worker"`
}

// account describes this machine's sign-in.
func (s *Server) account() Account {
	var a Account
	cfgDir, dataDir := s.opts.LongarmConfigDir, s.opts.LongarmdDataDir
	now := s.now()
	clientValid, workerValid := false, false
	if c := readCert(filepath.Join(cfgDir, client.TLSDir, pki.NodeCertFile)); c != nil {
		na := c.NotAfter
		a.Expires = &na
		clientValid = now.Before(na)
		if id, err := pki.IdentityFromCert(c); err == nil && clientValid {
			a.SignedIn, a.User = true, id.Owner()
		}
	}
	if c := readCert(filepath.Join(dataDir, longarmd.TLSDir, pki.NodeCertFile)); c != nil {
		if a.Expires == nil {
			na := c.NotAfter
			a.Expires = &na
		}
		workerValid = now.Before(c.NotAfter)
		if hf, err := hubclient.LoadFile(dataDir); err == nil && workerValid {
			a.SignedIn = true
			if a.User == "" {
				a.User = hf.Owner
			}
		}
	}
	a.Expired = a.Expires != nil && !now.Before(*a.Expires)
	a.Hub, _, a.HubFrom = s.signinHub()
	wantClient, _ := s.installedRoles()
	t := s.targets(nil)
	a.Targets = t
	a.Needed = (wantClient && t.Client && !clientValid) || (t.Worker && !workerValid) || t.AdminWorker
	s.accountRoles(&a)
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

func readCert(p string) *x509.Certificate {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	c, err := pki.ParseCertPEM(b)
	if err != nil {
		return nil
	}
	return c
}

// targets is what a sign-in would enroll now; only (a role change: the
// roles just added) narrows it.
func (s *Server) targets(only *SignInTarget) SignInTarget {
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
	if only != nil {
		t = SignInTarget{Client: t.Client && only.Client, Worker: t.Worker && only.Worker, AdminWorker: t.AdminWorker && only.AdminWorker}
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
		// Replace: the user confirmed replacing what this machine is
		// signed in as (state "confirm")
		Replace bool `json:"replace"`
	}
	if !readBody(w, r, &body) {
		return
	}
	var only *SignInTarget
	if body.Replace {
		s.signin.mu.Lock()
		if s.signin.st.State == "confirm" { // the role change it came from, if any
			only = s.signin.only
		}
		s.signin.mu.Unlock()
	}
	st, code, err := s.startSignIn(body.Hub, body.Labels, only, body.Replace)
	if err != nil {
		httpError(w, code, err.Error())
		return
	}
	writeJSON(w, st)
}

// startSignIn starts a sign-in to hubIn ("" = signinHub) of the targets
// (only narrows them); a refusal comes with its HTTP status. Without
// replace, a machine signed in already ends it in state "confirm".
func (s *Server) startSignIn(hubIn, labelsIn string, only *SignInTarget, replace bool) (SignInState, int, error) {
	hubIn = strings.TrimSpace(hubIn)
	if len(hubIn) > 256 || strings.ContainsAny(hubIn, " \t\"'`;&|") {
		return SignInState{}, http.StatusBadRequest, errors.New("bad hub address")
	}
	var labels map[string]string
	if labelsIn != "" {
		m, err := sched.ParseLabels([]string{labelsIn}, false)
		if err != nil {
			return SignInState{}, http.StatusBadRequest, err
		}
		labels = m
	}
	hub, fp, _ := s.signinHub()
	if hubIn != "" {
		hub, fp = hubIn, ""
	}
	if hub == "" {
		return SignInState{}, http.StatusBadRequest, errors.New("which hub? enter its address (e.g. longarm.example.com)")
	}
	t := s.targets(only)
	if !t.Client && !t.Worker && !t.AdminWorker {
		return SignInState{}, http.StatusConflict, errors.New("nothing to sign in")
	}
	so := signin.Options{Hub: hub, Fingerprint: fp, OpenBrowser: true, WorkerLabels: labels, Replace: replace}
	if t.Client {
		// replace: confirmed, also another hub's certificate
		so.Client = client.DeviceTarget{ConfigDir: s.opts.LongarmConfigDir, Force: replace}
	}
	if t.Worker {
		so.Worker = longarmd.WorkerTarget{DataDir: s.opts.LongarmdDataDir}
		if s.installedWorker != nil {
			if mode, _ := s.installedWorker(); mode == "session" {
				so.WorkerKind = "user session"
			}
		}
	}
	if t.AdminWorker {
		_, adm := s.installedWorker()
		so.AdminWorker = longarmd.AdminWorkerTarget{Handoff: adm.Handoff, DataDir: adm.DataDir}
	}
	run := &s.signin
	run.mu.Lock()
	if run.st.State == "running" {
		run.mu.Unlock()
		return SignInState{}, http.StatusConflict, errors.New("a sign-in is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel, run.only = cancel, only
	run.st = SignInState{State: "running", Hub: hub, Started: time.Now(), Targets: t}
	run.mu.Unlock()
	go s.runSignIn(ctx, so, SignInRequest{Hub: hubIn, Labels: labelsIn})
	return run.snapshot(), http.StatusOK, nil
}

// Busy reports whether a sign-in or a role change runs: the popup then
// stays open (it must not hide when the browser takes the focus).
func (s *Server) Busy() bool {
	return s.signin.snapshot().State == "running" || s.rolesBusy()
}

// AutoSignIn starts the sign-in by itself when this machine needs one and
// the hub is known (the tray the installer starts, the first start after
// an install): the code shows at once, no click on "Sign in". It reports
// whether it started one.
func (s *Server) AutoSignIn() bool {
	a := s.account()
	if !a.Needed || a.Hub == "" {
		return false
	}
	_, _, err := s.startSignIn("", "", nil, false)
	return err == nil
}

func (s *Server) runSignIn(ctx context.Context, so signin.Options, req SignInRequest) {
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
			sc := &SignInCode{URL: c.URL, Code: c.Code, Expires: c.Expires, Kind: c.Kind, Host: c.Host, AdminWorker: c.AdminWorker}
			run.update(func(st *SignInState) { st.Code = sc })
			if s.OnSignInCode != nil {
				go s.OnSignInCode(*sc) // the shell: keep the popup up, copy the code, a notification
			}
		},
	}
	signIn := s.signIn
	if signIn == nil {
		signIn = signin.SignIn
	}
	res, err := signIn(ctx, so, ev)
	var held *signin.SignedInError
	confirm := errors.As(err, &held)
	var cf *SignInConfirm
	if confirm {
		cf = s.confirmOf(held, so, req)
	}
	run.update(func(st *SignInState) {
		st.Code = nil
		switch {
		case confirm:
			st.State, st.Confirm = "confirm", cf
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

// confirmOf describes what a sign-in would replace, for the page to ask.
func (s *Server) confirmOf(e *signin.SignedInError, so signin.Options, req SignInRequest) *SignInConfirm {
	cf := &SignInConfirm{Hub: e.Hub, OtherHub: e.OtherHub(), Request: req}
	for _, c := range e.Current {
		cf.Current = append(cf.Current, SignInHeld{Kind: c.Kind, Identity: c.Identity(), User: c.User, Hub: c.Hub,
			SameHub: c.SameHub(e.CAFingerprint), Expired: c.Expired(s.now())})
	}
	if so.AdminWorker == nil && s.installedWorker != nil {
		if _, adm := s.installedWorker(); adm.Enabled {
			cf.AdminWorkerStays = longarmd.AdminWorkerStatus(adm, queryService).SignedIn
		}
	}
	return cf
}

func (s *Server) signinState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.signin.snapshot())
}

func (s *Server) signinCancel(w http.ResponseWriter, _ *http.Request) {
	s.signin.mu.Lock()
	if s.signin.cancel != nil {
		s.signin.cancel()
	}
	if s.signin.st.State == "confirm" { // "keep things as they are"
		s.signin.st = SignInState{State: "idle"}
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
