package tray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/longarmd"
)

// The Account tab's "This computer: [x] send jobs (client) [x] run jobs
// (worker) [ ] admin commands (admin worker)" (/trayapi/roles): a change
// runs `longarmd install-helper roles` elevated - one administrator prompt
// - which changes the machine and the installer's record together
// (internal/longarmd/roles.go), then this tray signs the added roles in
// with one approval. A removed client leaves the hub as this user (its
// credentials are the user's); the helper takes the worker and the admin
// worker off the hub.

// RolesState is what /trayapi/roles/state reports.
type RolesState struct {
	// State: "idle", "applying" (the administrator prompt, the helper),
	// "done" or "error".
	State string       `json:"state"`
	Want  AccountRoles `json:"want"`
	// Message: what happened (warnings included, e.g. a hub that could not
	// be told); SignIn: a sign-in of the added roles started.
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
	SignIn  bool   `json:"signin,omitempty"`
}

type rolesRun struct {
	mu sync.Mutex
	st RolesState
}

func (r *rolesRun) snapshot() RolesState {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.st
	if st.State == "" {
		st.State = "idle"
	}
	return st
}

func (r *rolesRun) set(st RolesState) {
	r.mu.Lock()
	r.st = st
	r.mu.Unlock()
}

func (s *Server) rolesBusy() bool { return s.roleRun.snapshot().State == "applying" }

func (s *Server) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// accountRoles fills the roles part of the account.
func (s *Server) accountRoles(a *Account) {
	c, w := s.installedRoles()
	a.Roles = AccountRoles{Client: c, Worker: w}
	if s.installedWorker != nil {
		_, adm := s.installedWorker()
		a.Roles.AdminWorker = adm.Enabled
	}
	if !w && s.cloudPC != nil {
		a.CloudPC = s.cloudPC()
	}
	if s.machineRoles == nil {
		return
	}
	m := s.machineRoles()
	if !m.Installed {
		a.RolesNote = "Longarm was not installed with the Windows installer here: the roles follow how longarm and longarmd run."
		return
	}
	a.RolesEditable = s.currentSID != nil && s.runElevatedWait != nil
	me := ""
	if s.currentSID != nil {
		me, _ = s.currentSID()
	}
	switch {
	case m.Worker && m.RunMode == "service":
		a.RolesNote = "The worker here is a Windows service: change it with the installer (uninstall, then install again)."
	case m.Worker && me != "" && longarmd.ValidUserSID(m.SessionSID) == nil && !strings.EqualFold(m.SessionSID, me):
		a.RolesNote = "The worker here runs as the user who installed Longarm, not as you."
	}
}

// rolesChange starts a role change (one at a time; not during a sign-in).
func (s *Server) rolesChange(w http.ResponseWriter, r *http.Request) {
	var body AccountRoles
	if !readBody(w, r, &body) {
		return
	}
	if s.machineRoles == nil || s.currentSID == nil || s.runElevatedWait == nil {
		httpError(w, http.StatusNotFound, "changing roles needs Longarm's Windows installer")
		return
	}
	me, err := s.currentSID()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cur := s.machineRoles()
	want := longarmd.RoleChange{Client: body.Client, Worker: body.Worker, Admin: body.AdminWorker, SID: me}
	plan, err := longarmd.PlanRoleChange(cur, want)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if plan.Empty(cur) {
		httpError(w, http.StatusBadRequest, "nothing to change")
		return
	}
	s.roleRun.mu.Lock()
	if s.roleRun.st.State == "applying" || s.signin.snapshot().State == "running" {
		s.roleRun.mu.Unlock()
		httpError(w, http.StatusConflict, "a sign-in or a role change is running")
		return
	}
	s.roleRun.st = RolesState{State: "applying", Want: body}
	s.roleRun.mu.Unlock()
	go s.applyRoles(cur, want, plan, body)
	writeJSON(w, s.roleRun.snapshot())
}

func (s *Server) rolesState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.roleRun.snapshot())
}

// applyRoles: the administrator prompt and the helper, then what this user
// does: a removed client leaves, the added roles sign in.
func (s *Server) applyRoles(cur longarmd.MachineRoles, want longarmd.RoleChange, plan longarmd.RolePlan, body AccountRoles) {
	err := s.runElevatedWait(s.opts.LongarmdExe, longarmd.RolesArgs(want)...)
	result := ""
	if s.rolesResult != nil {
		result = s.rolesResult()
	}
	if err != nil {
		msg := "the change failed: " + err.Error()
		switch {
		case errors.Is(err, ErrElevationCanceled):
			msg = "Canceled at the administrator prompt: nothing changed."
		case strings.HasPrefix(result, "error: "):
			msg = strings.TrimPrefix(result, "error: ")
		}
		s.roleRun.set(RolesState{State: "error", Want: body, Error: msg})
		return
	}
	var notes []string
	if r := strings.TrimPrefix(result, "ok"); strings.HasPrefix(r, "; ") {
		notes = append(notes, strings.TrimPrefix(r, "; "))
	}
	if cur.Client && !want.Client && exists(s.opts.LongarmConfigDir, "tls", "node.crt") {
		// the client's credentials are this user's: leave as this user
		// (a hub that can't be reached: removed here, a warning)
		s.runCmd(s.opts.LongarmExe, s.longarmArgs("daemon", "stop")[1:]...)
		o, err := s.runCmd(s.opts.LongarmExe, s.longarmArgs("leave")[1:]...)
		if err != nil {
			notes = append(notes, "longarm leave: "+err.Error()+" "+strings.TrimSpace(string(o)))
		}
	}
	only := SignInTarget{Client: want.Client && !cur.Client, Worker: plan.AddWorker, AdminWorker: plan.AddAdmin}
	if plan.AddWorker {
		// the worker waits for this sign-in (its host does the same)
		if hub, fp, _ := s.signinHub(); hub != "" {
			if err := longarmd.SetUpSignIn(s.opts.LongarmdDataDir, hub, fp); err != nil {
				notes = append(notes, "preparing the worker's sign-in: "+err.Error())
			}
		}
	}
	if plan.AddAdmin {
		s.waitAdminRequest(60 * time.Second)
	}
	st := RolesState{State: "done", Want: body, Message: "Done: " + rolesText(body) + "."}
	if len(notes) > 0 {
		st.Message += " " + strings.Join(notes, "; ")
	}
	if only.Client || only.Worker || only.AdminWorker {
		if _, _, err := s.startSignIn("", "", &only); err != nil {
			st.Message += " Sign in from this tab: " + err.Error()
		} else {
			st.SignIn = true
		}
	}
	s.roleRun.set(st)
}

// waitAdminRequest waits for the admin worker's service to ask for its
// sign-in (it writes its request when it starts).
func (s *Server) waitAdminRequest(max time.Duration) {
	if s.installedWorker == nil {
		return
	}
	for end := s.now().Add(max); s.now().Before(end); time.Sleep(time.Second) {
		if _, adm := s.installedWorker(); adm.Handoff != "" && exists(adm.Handoff, longarmd.AdminRequestFile) {
			return
		}
	}
}

func rolesText(r AccountRoles) string {
	var parts []string
	if r.Client {
		parts = append(parts, "sends jobs (client)")
	}
	if r.Worker {
		parts = append(parts, "runs jobs (worker)")
	}
	if r.AdminWorker {
		parts = append(parts, "runs admin commands (admin worker)")
	}
	return "this computer " + strings.Join(parts, ", ")
}

// ExpiryNotice is the notification for a sign-in that ends soon or has
// ended (ok false: none). Certificates renew while longarm / longarmd run;
// one that is about to end means renewal is failing.
func ExpiryNotice(a Account, now time.Time) (title, body string, ok bool) {
	if a.Expires == nil {
		return "", "", false
	}
	day := a.Expires.Local().Format("Mon 2 Jan 15:04")
	switch left := a.Expires.Sub(now); {
	case left <= 0:
		return "Longarm: sign in again", "Your sign-in expired on " + day + ". Open the Longarm tray and sign in again.", true
	case left < 3*24*time.Hour:
		return "Longarm: your sign-in ends soon", fmt.Sprintf("Your sign-in ends on %s and could not be renewed (is the hub reachable?). Sign in again from the Longarm tray.", day), true
	}
	return "", "", false
}

// WatchSignIn notifies (at most once a day) when this machine's sign-in
// ends within three days or has ended, until ctx ends.
func (s *Server) WatchSignIn(ctx context.Context, notify func(title, body string)) {
	var last time.Time
	for {
		if t, b, ok := ExpiryNotice(s.account(), s.now()); ok && s.now().Sub(last) > 24*time.Hour {
			last = s.now()
			notify(t, b)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// WindowPrefs are the popup's settings this user chose (in the client's
// config dir, tray-window.json).
type WindowPrefs struct {
	// AutoHide: hide the popup when it loses the focus (off by default:
	// the popup stays until closed - pinned).
	AutoHide bool `json:"auto_hide"`
}

const windowPrefsFile = "tray-window.json"

// LoadWindowPrefs reads the prefs (the defaults when there are none).
func LoadWindowPrefs(dir string) WindowPrefs {
	var p WindowPrefs
	if b, err := os.ReadFile(filepath.Join(dir, windowPrefsFile)); err == nil {
		json.Unmarshal(b, &p)
	}
	return p
}

// SaveWindowPrefs writes them.
func SaveWindowPrefs(dir string, p WindowPrefs) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, windowPrefsFile), mustJSON(p), 0o600)
}

const autoSignInFile = "tray-autosignin"

// FirstStart reports whether this is the tray's first start for this
// version in this config dir (after an install or an upgrade), and
// remembers it: the tray then starts the sign-in by itself when one is
// needed, as when the installer starts it (--signin).
func FirstStart(dir, version string) bool {
	p := filepath.Join(dir, autoSignInFile)
	if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == version {
		return false
	}
	os.MkdirAll(dir, 0o700)
	os.WriteFile(p, []byte(version+"\n"), 0o600)
	return true
}

func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}
