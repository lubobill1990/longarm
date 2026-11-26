// Package desktopsvc is the Longarm Desktop service (Windows;
// docs/remote-desktop.md "Decisions"): a LocalSystem service, installed
// with the worker role, that lets longarmd show the screen a person at
// the machine sees, UAC prompts and the lock screen included, and send
// Ctrl+Alt+Del.
//
// A process can only capture and drive the desktop of its own session,
// and only SYSTEM may open the Winlogon desktop (UAC's secure desktop, the
// lock and Ctrl+Alt+Del screens). So the service (session 0) keeps an
// agent - `longarmd desktop-agent`, SYSTEM - running in the console
// session, moving it when the console session changes. The agent serves
// the RemoteDesktop gRPC service (internal/rdesk) on a named pipe
// (AgentPipe) that only SYSTEM, administrators and the accounts the
// installer recorded (the worker's: Clients) may open; longarmd relays its
// owner's stream there (server.DesktopAgent). Windows takes Ctrl+Alt+Del
// (SendSAS) only from session 0: the agent asks the service over SASPipe,
// which only SYSTEM may open.
package desktopsvc

import "errors"

const (
	// ServiceName of the Windows service.
	ServiceName = "LongarmDesktop"
	// AgentPipe: the agent's RemoteDesktop service.
	AgentPipe = `\\.\pipe\LongarmDesktopAgent`
	// SASPipe: the service sends Ctrl+Alt+Del for the agent.
	SASPipe = `\\.\pipe\LongarmDesktopSAS`
	// RegKey (HKLM) holds Clients (REG_MULTI_SZ: SIDs that may use the
	// agent besides SYSTEM and administrators) and whether the installer
	// turned the SoftwareSASGeneration policy on (to turn it off again).
	RegKey = `Software\Longarm\Desktop`
)

// ErrUnsupported: not on this OS.
var ErrUnsupported = errors.New("the Longarm Desktop service is Windows only")
