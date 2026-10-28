package longarmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lubobill1990/longarm/internal/platform"
)

// Roles of a Windows machine the MSI installed, changed later from the
// tray's Account tab ("This computer: send jobs / run jobs / admin
// commands") without reinstalling. The MSI records them in
// HKLM\Software\Longarm (Roles, SessionUserSID, RunMode, AdminWorker), and
// its upgrades, repairs and uninstall act on what is recorded there: the
// elevated `longarmd install-helper roles` changes the machine (the
// user-session worker's task, the admin worker's service) and the record
// together, so that an uninstall still removes everything and a repair
// does not undo the change.

// Registry names of the MSI's role record (the Settings component writes
// them at install; the roles helper changes them afterwards).
const (
	RegRoles          = "Roles"
	RegSessionUserSID = "SessionUserSID"
	RegRunMode        = "RunMode"
	RegAdminWorker    = "AdminWorker"
	// RegRolesResult: the last roles change's outcome ("ok: ..." or
	// "error: ..."), for the tray that asked (an elevated process can't
	// hand its output back).
	RegRolesResult = "RolesResult"
)

// MachineRoles is what this machine is, as the MSI (and the roles helper)
// recorded it.
type MachineRoles struct {
	Client, Worker, Admin bool
	// SessionSID: the user whose logon starts the user-session worker
	// (the installing user); RunMode: session or service.
	SessionSID, RunMode string
	// Installed: an MSI recorded roles at all (else nothing to change).
	Installed bool
}

// RolesString is the MSI's ROLES value for client / worker.
func RolesString(client, worker bool) string {
	switch {
	case client && worker:
		return "both"
	case worker:
		return "worker"
	}
	return "client"
}

// LoadMachineRoles reads the record (s: MachineSettings()).
func LoadMachineRoles(s SettingsStore) MachineRoles {
	var m MachineRoles
	roles, ok := s.Get(RegRoles)
	m.Installed = ok && roles != ""
	m.Client = roles == "client" || roles == "both"
	m.Worker = roles == "worker" || roles == "both"
	m.SessionSID, _ = s.Get(RegSessionUserSID)
	m.RunMode, _ = s.Get(RegRunMode)
	if m.RunMode == "" {
		if acct, _ := s.Get("ServiceAccount"); acct != "" {
			m.RunMode = "service"
		}
	}
	a, _ := s.Get(RegAdminWorker)
	m.Admin = a == "1"
	return m
}

// RoleChange is what the tray asks for: the roles wanted, for user SID.
type RoleChange struct {
	Client, Worker, Admin bool
	SID                   string
}

// RolePlan is what the roles helper does for a RoleChange.
type RolePlan struct {
	AddWorker, RemoveWorker bool
	AddAdmin, RemoveAdmin   bool
	// Roles to record (RolesString), SessionSID to record.
	Roles, SessionSID string
	// DropTrayTaskOf: the previous session user's on-demand tray task, left
	// behind when the session user changes ("": none).
	DropTrayTaskOf string
}

// Empty: nothing changes.
func (p RolePlan) Empty(cur MachineRoles) bool {
	return !p.AddWorker && !p.RemoveWorker && !p.AddAdmin && !p.RemoveAdmin &&
		p.Roles == RolesString(cur.Client, cur.Worker) && strings.EqualFold(p.SessionSID, cur.SessionSID)
}

// PlanRoleChange checks a change against the machine's record:
//
//   - at least one of client and worker stays;
//   - the admin worker comes with the worker role (an install from before
//     that rule that has it without a worker keeps it, but can't add it);
//   - a worker that is a Windows service (RUN=service) is changed with the
//     MSI only;
//   - the user-session worker belongs to the user who adds it, and there
//     is one per install: it can move to another user only while the
//     recorded one has neither a worker nor an admin worker; only its own
//     user removes it (an administrator: the MSI).
func PlanRoleChange(cur MachineRoles, want RoleChange) (RolePlan, error) {
	if !cur.Installed {
		return RolePlan{}, errors.New("Longarm's installer recorded no roles on this computer (install the MSI first)")
	}
	if !want.Client && !want.Worker {
		return RolePlan{}, errors.New("keep at least one role: send jobs (client) or run jobs (worker); to remove Longarm, uninstall it")
	}
	if err := ValidUserSID(want.SID); err != nil {
		return RolePlan{}, err
	}
	if want.Admin && !want.Worker && !(cur.Admin && !cur.Worker) {
		return RolePlan{}, errors.New("the admin worker comes with the worker role: also choose \"run jobs\"")
	}
	p := RolePlan{Roles: RolesString(want.Client, want.Worker), SessionSID: cur.SessionSID}
	p.AddWorker = want.Worker && !cur.Worker
	p.RemoveWorker = !want.Worker && cur.Worker
	p.AddAdmin = want.Admin && !cur.Admin
	p.RemoveAdmin = !want.Admin && cur.Admin
	if (p.AddWorker || p.RemoveWorker) && cur.RunMode == "service" {
		return RolePlan{}, errors.New("this computer's worker is a Windows service: change it with the installer (uninstall, then install again)")
	}
	recorded := cur.SessionSID != "" && ValidUserSID(cur.SessionSID) == nil
	mine := strings.EqualFold(cur.SessionSID, want.SID)
	switch {
	case p.AddWorker && recorded && !mine:
		if cur.Admin && !p.RemoveAdmin {
			return RolePlan{}, fmt.Errorf("Longarm was installed by another user (%s), whose admin worker is set up here: only one user's worker per computer", cur.SessionSID)
		}
		p.SessionSID, p.DropTrayTaskOf = want.SID, cur.SessionSID
	case p.AddWorker && !recorded:
		p.SessionSID = want.SID
	case p.RemoveWorker && !mine:
		return RolePlan{}, fmt.Errorf("the worker here runs as another user (%s): only that user can remove it (or uninstall Longarm)", cur.SessionSID)
	}
	if p.AddAdmin && !strings.EqualFold(p.SessionSID, want.SID) {
		return RolePlan{}, fmt.Errorf("the admin worker is signed in by the user whose worker runs here (%s)", p.SessionSID)
	}
	return p, nil
}

// RolesArgs is the command line the tray runs elevated for a change: the
// installed longarmd's `install-helper roles` with its data after
// MSIDataMarker (parsed as strictly as the MSI's, MSIData).
func RolesArgs(c RoleChange) []string {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return ""
	}
	return []string{"install-helper", "roles", MSIDataMarker + "client=" + b(c.Client) + "|worker=" + b(c.Worker) + "|admin=" + b(c.Admin) + "|sid=" + c.SID}
}

// RolesKeys are the fields of `install-helper roles`.
var RolesKeys = []string{"client", "worker", "admin", "sid"}

// WorkerKind says how the worker of dataDir runs, for sign-in summaries:
// "user session" (the MSI's RUN=session, the user's own data dir),
// "service", or "" (not installed by the MSI, or another data dir).
func WorkerKind(dataDir string) string {
	mode, _ := InstalledWorker()
	a, err1 := filepath.Abs(dataDir)
	b, err2 := filepath.Abs(DefaultDataDir())
	if mode == "" || err1 != nil || err2 != nil || !samePath(a, b) {
		return ""
	}
	if mode == "session" {
		return "user session"
	}
	return mode
}

// Cloud PCs and Dev Boxes, where the installer suggests "This machine runs
// jobs": Windows 365 Cloud PCs and Microsoft Dev Boxes are named CPC-...,
// and a Dev Box runs the Dev Box agent service. longarm.wxs searches for
// the same names (TestMSICloudPCDetection).
const CloudPCNamePrefix = "CPC-"

// DevBoxAgentServices are the Dev Box agent's service names.
var DevBoxAgentServices = []string{"Microsoft.DevCenter.DevBoxAgent", "DevBoxAgent"}

// MachineProbe is what LooksLikeCloudPC looks at (the system: SystemProbe;
// tests: fakes).
type MachineProbe interface {
	ComputerName() string
	ServiceExists(name string) bool
}

// LooksLikeCloudPC reports whether this machine looks like a Windows 365
// Cloud PC or a Microsoft Dev Box - usually a worker, not an everyday
// computer - and why.
func LooksLikeCloudPC(p MachineProbe) (bool, string) {
	if n := p.ComputerName(); len(n) >= len(CloudPCNamePrefix) && strings.EqualFold(n[:len(CloudPCNamePrefix)], CloudPCNamePrefix) {
		return true, "the computer name " + n + " is a Cloud PC's / Dev Box's"
	}
	for _, s := range DevBoxAgentServices {
		if p.ServiceExists(s) {
			return true, "the Dev Box agent (" + s + ") runs here"
		}
	}
	return false, ""
}

// SystemProbe is the MachineProbe of this machine.
type SystemProbe struct{}

func (SystemProbe) ComputerName() string {
	if n := os.Getenv("COMPUTERNAME"); n != "" {
		return n
	}
	n, _ := os.Hostname()
	return n
}

func (SystemProbe) ServiceExists(name string) bool {
	st, err := platform.QueryService(name)
	return err == nil && st != platform.ServiceNotInstalled
}
