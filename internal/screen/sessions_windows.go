//go:build windows

package screen

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modWtsapi32Sessions       = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSEnumerateSessionsW = modWtsapi32Sessions.NewProc("WTSEnumerateSessionsW")
	procWTSQuerySessionInfoW  = modWtsapi32Sessions.NewProc("WTSQuerySessionInformationW")
	procWTSFreeMemory         = modWtsapi32Sessions.NewProc("WTSFreeMemory")
	procWTSConnectSessionW    = modWtsapi32Sessions.NewProc("WTSConnectSessionW")
)

// WTS_INFO_CLASS values and WTS_CONNECTSTATE_CLASS names.
const (
	wtsUserName   = 5
	wtsDomainName = 7
)

var wtsStates = []string{"active", "connected", "connect-query", "shadow", "disconnected", "idle", "listen", "reset", "down", "init"}

// wtsSessionInfo is WTS_SESSION_INFOW.
type wtsSessionInfo struct {
	SessionID   uint32
	StationName *uint16
	State       uint32
}

func sessions() ([]Session, error) {
	var list *wtsSessionInfo
	var n uint32
	if r, _, err := procWTSEnumerateSessionsW.Call(0, 0, 1, uintptr(unsafe.Pointer(&list)), uintptr(unsafe.Pointer(&n))); r == 0 {
		return nil, fmt.Errorf("WTSEnumerateSessions: %w", err)
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(list)))
	console := ConsoleSession()
	var out []Session
	for _, s := range unsafe.Slice(list, n) {
		if s.SessionID == 0 || s.SessionID >= 65536 { // services; listeners
			continue
		}
		state := "unknown"
		if int(s.State) < len(wtsStates) {
			state = wtsStates[s.State]
		}
		ss := Session{ID: s.SessionID, State: state, Station: windows.UTF16PtrToString(s.StationName), Console: s.SessionID == console}
		if user := sessionString(s.SessionID, wtsUserName); user != "" {
			ss.User = user
			if dom := sessionString(s.SessionID, wtsDomainName); dom != "" {
				ss.User = dom + `\` + user
			}
		}
		// the sign-in screen's session (nobody) is only of interest on the console
		if ss.User == "" && !ss.Console {
			continue
		}
		out = append(out, ss)
	}
	return out, nil
}

func sessionString(id uint32, class uintptr) string {
	var p *uint16
	var size uint32
	if r, _, _ := procWTSQuerySessionInfoW.Call(0, uintptr(id), class, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&size))); r == 0 || p == nil {
		return ""
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(p)))
	return windows.UTF16PtrToString(p)
}

func connectToConsole(id uint32) error {
	console := ConsoleSession()
	if console == 0xFFFFFFFF {
		return fmt.Errorf("the console is switching sessions")
	}
	if id == console {
		return nil
	}
	empty, _ := windows.UTF16PtrFromString("")
	// SYSTEM needs no password (tscon does the same).
	if r, _, err := procWTSConnectSessionW.Call(uintptr(id), uintptr(console), uintptr(unsafe.Pointer(empty)), 1); r == 0 {
		return fmt.Errorf("WTSConnectSession(%d to the console %d): %w", id, console, err)
	}
	return nil
}
