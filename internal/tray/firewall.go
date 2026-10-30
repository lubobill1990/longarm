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
	"time"

	"github.com/lubobill1990/longarm/internal/localapi"
	"github.com/lubobill1990/longarm/internal/platform"
)

// The firewall card's "Allow" button (/trayapi/firewall/allow): Windows
// Firewall blocks inbound UDP to the longarm.exe the daemon runs from (or
// to longarmd.exe), so hole punching fails and connections stay on relays.
// The tray runs `longarm firewall allow --program <exe>` elevated - one
// administrator prompt, the same mechanism as a role change - then checks
// the rules again.
//
// What runs elevated is fixed: the longarm.exe installed with this tray,
// never a path from the request. The program to allow is the one the
// running daemon (or longarmd) reports through its local API - which may be
// a copy another app ships (TMIA Desktop starts the daemon from
// %USERPROFILE%\tmia-desktop-live\longarm-x.y.z\longarm.exe), and that copy
// is the one that needs the rule. It must be an existing longarm.exe or
// longarmd.exe; the request only says which of the two local APIs to ask.

// FirewallFix is what /trayapi/firewall/allow reports.
type FirewallFix struct {
	Program string `json:"program"`
	// OK: the program has an allow rule and no Block rule now.
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// firewallProgram checks a program path a local API reported: absolute, an
// existing file, named longarm or longarmd.
func firewallProgram(p string) (string, error) {
	if p == "" {
		return "", errors.New("no firewall problem is reported")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s is not an absolute path", p)
	}
	p = filepath.Clean(p)
	switch strings.TrimSuffix(strings.ToLower(filepath.Base(p)), ".exe") {
	case "longarm", "longarmd":
	default:
		return "", fmt.Errorf("%s is not longarm.exe or longarmd.exe", p)
	}
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s does not exist", p)
	}
	return p, nil
}

// reportedFirewall asks a local API (daemon.addr / longarmd.local) for its
// firewall verdict.
func reportedFirewall(ctx context.Context, addrFile string) (program string, ok bool, err error) {
	a, err := localapi.ReadAddr(addrFile)
	if err != nil {
		return "", false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://local/v1/status", nil)
	resp, err := localapi.HTTPClient(a).Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	var st struct {
		Firewall *struct {
			Program string `json:"program"`
			OK      bool   `json:"ok"`
		} `json:"firewall"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return "", false, err
	}
	if st.Firewall == nil {
		return "", true, nil
	}
	return st.Firewall.Program, st.Firewall.OK, nil
}

func (s *Server) firewallAllow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		For string `json:"for"` // "client" (longarm daemon) or "worker" (longarmd)
	}
	if !readBody(w, r, &body) {
		return
	}
	if s.runElevatedWait == nil {
		httpError(w, http.StatusNotFound, "only on Windows")
		return
	}
	var addrFile string
	switch body.For {
	case "client":
		addrFile = filepath.Join(s.opts.LongarmConfigDir, "daemon.addr")
	case "worker":
		addrFile = filepath.Join(s.opts.LongarmdDataDir, "longarmd.local")
	default:
		httpError(w, http.StatusBadRequest, `for: "client" or "worker"`)
		return
	}
	longarm := s.opts.LongarmExe
	if fi, err := os.Stat(longarm); err != nil || !filepath.IsAbs(longarm) || !fi.Mode().IsRegular() {
		httpError(w, http.StatusNotFound, "longarm.exe is not installed next to the tray")
		return
	}
	reported, ok, err := reportedFirewall(r.Context(), addrFile)
	if err != nil {
		httpError(w, http.StatusBadGateway, "local API unreachable: "+err.Error())
		return
	}
	if ok {
		writeJSON(w, FirewallFix{Program: reported, OK: true, Message: "Nothing to fix: the firewall lets it in."})
		return
	}
	program, err := firewallProgram(reported)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.fwBusy.TryLock() {
		httpError(w, http.StatusConflict, "the firewall is being fixed")
		return
	}
	defer s.fwBusy.Unlock()
	if err := s.runElevatedWait(longarm, "firewall", "allow", "--program", program); err != nil {
		if errors.Is(err, ErrElevationCanceled) {
			httpError(w, http.StatusConflict, "Canceled at the administrator prompt: nothing changed.")
			return
		}
		httpError(w, http.StatusInternalServerError, "longarm firewall allow failed: "+err.Error())
		return
	}
	check := s.checkFirewall
	if check == nil {
		check = platform.CheckFirewall
	}
	st, err := check(program)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "checking the firewall again: "+err.Error())
		return
	}
	res := FirewallFix{Program: program, OK: st.OK()}
	if res.OK {
		res.Message = "Allowed: inbound connections to " + program + " pass the firewall. Direct connections are tried again as connections are made."
	} else {
		res.Message = st.Problem()
	}
	writeJSON(w, res)
}
