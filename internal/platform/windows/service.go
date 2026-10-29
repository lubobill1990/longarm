//go:build windows

package windows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
)

// InstallService registers longarmd with the SCM: automatic start, running as the
// given user account (so the user's PATH, %APPDATA%\npm, git/gh config are the
// ones jobs see), restart-on-failure recovery actions. The account is granted
// SeServiceLogonRight, which CreateService does not do by itself.
func InstallService(cfg ptypes.ServiceConfig) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to SCM (needs an elevated prompt): %w", err)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(cfg.Name); err == nil {
		s.Close()
		return fmt.Errorf("service %q already exists", cfg.Name)
	}
	user := normalizeAccount(cfg.User)
	if user != "" {
		if err := grantServiceLogonRight(user); err != nil {
			return fmt.Errorf("grant SeServiceLogonRight to %s: %w", user, err)
		}
	}
	s, err := m.CreateService(cfg.Name, cfg.Exe, mgr.Config{
		DisplayName:      cfg.DisplayName,
		Description:      cfg.Description,
		StartType:        mgr.StartAutomatic,
		ServiceStartName: user,
		Password:         cfg.Password,
	}, cfg.Args...)
	if err != nil {
		return fmt.Errorf("CreateService: %w", err)
	}
	defer s.Close()
	// Restart after 5 s, three times, failure counter reset after a day.
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, 86400); err != nil {
		return fmt.Errorf("SetRecoveryActions: %w", err)
	}
	return nil
}

// normalizeAccount turns "user" into ".\user"; the SCM wants a domain part.
func normalizeAccount(u string) string {
	if u == "" || strings.EqualFold(u, "LocalSystem") {
		return ""
	}
	if !strings.Contains(u, `\`) && !strings.Contains(u, "@") {
		return `.\` + u
	}
	return u
}

// UninstallService stops (if needed) and deletes the service.
func UninstallService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("open service %q: %w", name, err)
	}
	defer s.Close()
	_ = stop(s, 30*time.Second)
	return s.Delete()
}

// StartService starts the service and waits until it reports running.
func StartService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		return err
	}
	return waitState(s, svc.Running, 30*time.Second)
}

// StopService stops the service and waits until it reports stopped.
func StopService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()
	return stop(s, 30*time.Second)
}

func stop(s *mgr.Service, timeout time.Duration) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return err
	}
	return waitState(s, svc.Stopped, timeout)
}

func waitState(s *mgr.Service, want svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == want {
			return nil
		}
		if want == svc.Running && st.State == svc.Stopped {
			return errors.New("service stopped right after start (see longarmd.log in the data dir)")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for service state %d (now %d)", want, st.State)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// QueryService returns the coarse service status.
//
// It asks for the least access (SC_MANAGER_CONNECT, SERVICE_QUERY_STATUS),
// which every interactive user has: mgr.Connect / OpenService ask for
// all access, which only an elevated administrator gets - a user-session
// longarmd or the tray then saw no service at all.
func QueryService(name string) (ptypes.ServiceStatus, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return ptypes.ServiceUnknown, err
	}
	defer windows.CloseServiceHandle(scm)
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return ptypes.ServiceUnknown, err
	}
	h, err := windows.OpenService(scm, n, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) || errors.Is(err, windows.ERROR_INVALID_NAME) {
			return ptypes.ServiceNotInstalled, nil
		}
		return ptypes.ServiceUnknown, err
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &st); err != nil {
		return ptypes.ServiceUnknown, err
	}
	switch svc.State(st.CurrentState) {
	case svc.Running:
		return ptypes.ServiceRunning, nil
	case svc.Stopped:
		return ptypes.ServiceStopped, nil
	default:
		return ptypes.ServicePending, nil
	}
}

// RunAsService runs fn under the SCM if the current process was started by
// it. handled=false means "not a service, run in the foreground instead".
func RunAsService(name string, fn func(ctx context.Context) error) (handled bool, err error) {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return false, err
	}
	h := &handler{fn: fn}
	if err := svc.Run(name, h); err != nil {
		return true, err
	}
	return true, h.err
}

type handler struct {
	fn  func(ctx context.Context) error
	err error
}

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.fn(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			h.err = err
			status <- svc.Status{State: svc.StopPending}
			if err != nil {
				return true, 1 // non-zero => SCM recovery actions kick in
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case h.err = <-done:
				case <-time.After(20 * time.Second):
				}
				return false, 0
			}
		}
	}
}

// ---- LSA: grant SeServiceLogonRight -----------------------------------------

var (
	modadvapi32             = windows.NewLazySystemDLL("advapi32.dll")
	procLsaOpenPolicy       = modadvapi32.NewProc("LsaOpenPolicy")
	procLsaAddAccountRights = modadvapi32.NewProc("LsaAddAccountRights")
	procLsaClose            = modadvapi32.NewProc("LsaClose")
	procLsaNtStatusToWinErr = modadvapi32.NewProc("LsaNtStatusToWinError")
)

type lsaUnicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

type lsaObjectAttributes struct {
	Length                   uint32
	RootDirectory            windows.Handle
	ObjectName               *lsaUnicodeString
	Attributes               uint32
	SecurityDescriptor       uintptr
	SecurityQualityOfService uintptr
}

const (
	policyCreateAccount = 0x00000010
	policyLookupNames   = 0x00000800
)

func lsaErr(status uintptr) error {
	if status == 0 {
		return nil
	}
	code, _, _ := procLsaNtStatusToWinErr.Call(status)
	return windows.Errno(code)
}

func grantServiceLogonRight(account string) error {
	lookup := strings.TrimPrefix(account, `.\`)
	sid, _, _, err := windows.LookupSID("", lookup)
	if err != nil {
		return fmt.Errorf("lookup account %q: %w", lookup, err)
	}
	var attrs lsaObjectAttributes
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var policy windows.Handle
	st, _, _ := procLsaOpenPolicy.Call(0, uintptr(unsafe.Pointer(&attrs)),
		policyCreateAccount|policyLookupNames, uintptr(unsafe.Pointer(&policy)))
	if err := lsaErr(st); err != nil {
		return fmt.Errorf("LsaOpenPolicy: %w", err)
	}
	defer procLsaClose.Call(uintptr(policy))
	right, _ := windows.UTF16FromString("SeServiceLogonRight")
	us := lsaUnicodeString{
		Length:        uint16((len(right) - 1) * 2),
		MaximumLength: uint16(len(right) * 2),
		Buffer:        &right[0],
	}
	st, _, _ = procLsaAddAccountRights.Call(uintptr(policy), uintptr(unsafe.Pointer(sid)),
		uintptr(unsafe.Pointer(&us)), 1)
	if err := lsaErr(st); err != nil {
		return fmt.Errorf("LsaAddAccountRights: %w", err)
	}
	return nil
}

var procLsaRemoveAccountRights = modadvapi32.NewProc("LsaRemoveAccountRights")

// RevokeServiceLogonRight removes SeServiceLogonRight from account (used by
// tests to leave the machine clean; uninstall keeps the right because the
// account may run other services).
func RevokeServiceLogonRight(account string) error {
	lookup := strings.TrimPrefix(normalizeAccount(account), `.\`)
	sid, _, _, err := windows.LookupSID("", lookup)
	if err != nil {
		return err
	}
	var attrs lsaObjectAttributes
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var policy windows.Handle
	st, _, _ := procLsaOpenPolicy.Call(0, uintptr(unsafe.Pointer(&attrs)),
		policyCreateAccount|policyLookupNames, uintptr(unsafe.Pointer(&policy)))
	if err := lsaErr(st); err != nil {
		return err
	}
	defer procLsaClose.Call(uintptr(policy))
	right, _ := windows.UTF16FromString("SeServiceLogonRight")
	us := lsaUnicodeString{Length: uint16((len(right) - 1) * 2), MaximumLength: uint16(len(right) * 2), Buffer: &right[0]}
	st, _, _ = procLsaRemoveAccountRights.Call(uintptr(policy), uintptr(unsafe.Pointer(sid)), 0,
		uintptr(unsafe.Pointer(&us)), 1)
	return lsaErr(st)
}
