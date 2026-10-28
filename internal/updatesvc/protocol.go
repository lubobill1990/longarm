// Package updatesvc is the Longarm Update service (LongarmUpdate, Windows,
// every MSI install; docs/security.md "Update service"): a small
// LocalSystem service whose only capability is installing a Longarm MSI
// that is
//
//   - downloaded from the hub the installer recorded in HKLM (never a hub
//     from a request),
//   - signed with the release key built into the service binary, the
//     signature's trusted comment naming that file and version,
//   - listed in the hub's release record with the same sha256 and size,
//   - strictly newer than the installed version (no downgrade; the same
//     version only for a local administrator), and
//   - for this installation's architecture.
//
// Local programs talk to it over a named pipe (PipeName): any local,
// authenticated user may ask for "install version X" / "install the latest
// stable" (harmless: only newer signed releases install) and for the
// status; turning automatic updates on or off is for the machine's owner
// (the user the MSI was installed for) and local administrators only.
// Clients check that the pipe's server is the service (its process image is
// the installed longarm-update.exe and it runs as SYSTEM) before they trust
// an answer.
//
// The logic (Service) runs against interfaces - the release source, the
// download workspace, the installer launcher, the settings - so it is
// tested on every platform; windows.go has the Windows pieces.
package updatesvc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	// ServiceName of the Windows service.
	ServiceName = "LongarmUpdate"
	// BinaryName in the install dir.
	BinaryName = "longarm-update.exe"
	// PipeName is the local API.
	PipeName = `\\.\pipe\LongarmUpdate`
	// maxMessage caps a request or response line.
	maxMessage = 64 << 10
)

// Ops of Request.
const (
	OpStatus   = "status"   // anybody local
	OpInstall  = "install"  // anybody local: only newer signed releases install
	OpSetAuto  = "set-auto" // the owner or an administrator
	OpActivity = "activity" // a worker started a job (auto-update waits for a quiet moment)
)

// Codes of a refused or failed request (Response.Code).
const (
	CodeDenied      = "denied"       // not allowed for this caller
	CodeRefused     = "refused"      // a downgrade, a package for another machine
	CodeNotFound    = "not_found"    // the hub has no such version / no MSI for this architecture
	CodeBusy        = "busy"         // an install is in progress
	CodeRateLimited = "rate_limited" // too many requests
	CodeUnavailable = "unavailable"  // no hub recorded, hub unreachable, no release key
	CodeInvalid     = "invalid"      // a malformed request; a package that fails verification
	CodeFailed      = "failed"       // the installer failed
)

// Phases of Status.State and Response.Phase.
const (
	PhaseIdle        = "idle"
	PhaseChecking    = "checking"
	PhaseDownloading = "downloading"
	PhaseVerifying   = "verifying"
	PhaseInstalling  = "installing" // msiexec runs (Longarm restarts meanwhile)
	PhaseUpToDate    = "up_to_date"
	PhaseDone        = "done"
	PhaseFailed      = "failed"
)

// Request is one line a client sends.
type Request struct {
	Op string `json:"op"`
	// install: Version, or Latest (the hub's latest stable release).
	Version string `json:"version,omitempty"`
	Latest  bool   `json:"latest,omitempty"`
	// install: the installed version again (a repair) - a local
	// administrator only.
	Reinstall bool `json:"reinstall,omitempty"`
	// set-auto
	Auto          bool `json:"auto,omitempty"`
	IntervalHours int  `json:"interval_hours,omitempty"`
	// Requester: who asked, for the log (e.g. "longarm worker update by
	// alice@example.com"); informational only.
	Requester string `json:"requester,omitempty"`
}

// Result of a finished install.
type Result struct {
	From  string    `json:"from"`
	To    string    `json:"to"`
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"`
	Code  int       `json:"exit_code,omitempty"` // msiexec's
	At    time.Time `json:"at"`
	By    string    `json:"by,omitempty"`
}

// Status of the service.
type Status struct {
	Installed string    `json:"installed"`
	Available string    `json:"available,omitempty"` // the hub's latest stable ("" = unknown)
	Checked   time.Time `json:"checked,omitempty"`
	Hub       string    `json:"hub,omitempty"`
	Arch      string    `json:"arch"`
	State     string    `json:"state"` // a Phase
	Target    string    `json:"target,omitempty"`
	Started   time.Time `json:"started,omitempty"`
	Message   string    `json:"message,omitempty"`
	Last      *Result   `json:"last,omitempty"`
	// Auto: automatic updates (off by default), every AutoIntervalHours.
	Auto              bool      `json:"auto"`
	AutoIntervalHours int       `json:"auto_interval_hours"`
	AutoNext          time.Time `json:"auto_next,omitempty"`
	// CanVerify: the binary has a release key (else nothing installs).
	CanVerify bool `json:"can_verify"`
}

// Response is the service's answer.
type Response struct {
	OK      bool    `json:"ok"`
	Phase   string  `json:"phase,omitempty"`
	Code    string  `json:"code,omitempty"`
	Error   string  `json:"error,omitempty"`
	Message string  `json:"message,omitempty"`
	From    string  `json:"from,omitempty"`
	To      string  `json:"to,omitempty"`
	Status  *Status `json:"status,omitempty"`
}

// Error is a refused or failed request as an error (clients).
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Err returns the response's error (nil when OK).
func (r *Response) Err() error {
	if r.OK {
		return nil
	}
	msg := r.Error
	if msg == "" {
		msg = "the update service refused the request (" + r.Code + ")"
	}
	return &Error{Code: r.Code, Msg: msg}
}

// ErrorCode is err's Code ("" when err is no *Error).
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func writeMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) >= maxMessage {
		return errors.New("message too large")
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// readMessage reads one line into v; strict (requests, on the service's
// side): unknown fields are refused.
func readMessage(r io.Reader, v any, strict bool) error {
	br := bufio.NewReaderSize(io.LimitReader(r, maxMessage), 4096)
	line, err := br.ReadBytes('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("no message (closed or larger than %d bytes)", maxMessage)
		}
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	if strict {
		dec.DisallowUnknownFields()
	}
	return dec.Decode(v)
}
