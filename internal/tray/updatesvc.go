package tray

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/lubobill1990/longarm/internal/updatesvc"
)

// This machine's Longarm Update service (Windows MSI installs;
// internal/updatesvc): the update banner (update.go) installs through it
// when the machine has it - no UAC prompt; the service installs only a
// newer MSI from the recorded hub, signed with the release key built into
// it - and falls back to its own download and msiexec with UAC otherwise.
//
//	GET  /trayapi/update/service   the service's status (Settings: automatic updates)
//	POST /trayapi/update/auto {"auto": true, "hours": 6}   (the owner or an administrator)

// updateService is the service's client (tests fake it); nil = none here.
var updateService = func() serviceClient {
	if !updatesvc.Installed() {
		return nil
	}
	return updatesvc.NewClient("longarm-tray")
}

type serviceClient interface {
	Do(ctx context.Context, req *updatesvc.Request) (*updatesvc.Response, error)
}

type localUpdateStatus struct {
	Service           bool              `json:"service"`
	Installed         string            `json:"installed,omitempty"`
	Available         string            `json:"available,omitempty"`
	State             string            `json:"state,omitempty"`
	Target            string            `json:"target,omitempty"`
	Message           string            `json:"message,omitempty"`
	Auto              bool              `json:"auto"`
	AutoIntervalHours int               `json:"auto_interval_hours,omitempty"`
	Last              *updatesvc.Result `json:"last,omitempty"`
	CanSetAuto        bool              `json:"can_set_auto"`
	Error             string            `json:"error,omitempty"`
}

func toLocalStatus(st *updatesvc.Status) localUpdateStatus {
	return localUpdateStatus{Service: true, Installed: st.Installed, Available: st.Available, State: st.State, Target: st.Target,
		Message: st.Message, Auto: st.Auto, AutoIntervalHours: st.AutoIntervalHours, Last: st.Last, CanSetAuto: updatesvc.MayChangeAuto()}
}

// installViaService asks the update service for version: handled = false
// when this machine has no service (the banner then runs msiexec itself).
// The service checks everything again (newer, signed, recorded hub).
func installViaService(ctx context.Context, version string) (handled bool, err error) {
	c := updateService()
	if c == nil {
		return false, nil
	}
	resp, err := c.Do(ctx, &updatesvc.Request{Op: updatesvc.OpInstall, Version: version, Latest: version == ""})
	if err == nil {
		err = resp.Err()
	}
	return true, err
}

func (s *Server) updateServiceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /trayapi/update/service", func(w http.ResponseWriter, r *http.Request) {
		c := updateService()
		if c == nil {
			writeJSON(w, localUpdateStatus{})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		resp, err := c.Do(ctx, &updatesvc.Request{Op: updatesvc.OpStatus})
		if err == nil {
			err = resp.Err()
		}
		if err != nil || resp.Status == nil {
			writeJSON(w, localUpdateStatus{Service: true, Error: errText(err)})
			return
		}
		writeJSON(w, toLocalStatus(resp.Status))
	})
	mux.HandleFunc("POST /trayapi/update/auto", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Auto  bool `json:"auto"`
			Hours int  `json:"hours"`
		}
		if !readBody(w, r, &body) {
			return
		}
		c := updateService()
		if c == nil {
			httpError(w, http.StatusConflict, updatesvc.ErrNoService.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		resp, err := c.Do(ctx, &updatesvc.Request{Op: updatesvc.OpSetAuto, Auto: body.Auto, IntervalHours: body.Hours})
		if err == nil {
			err = resp.Err()
		}
		if err != nil {
			httpError(w, http.StatusForbidden, err.Error())
			return
		}
		if resp.Status == nil {
			httpError(w, http.StatusBadGateway, "no status in the answer")
			return
		}
		writeJSON(w, toLocalStatus(resp.Status))
	})
}

func errText(err error) string {
	if err == nil {
		return "no status"
	}
	var e *updatesvc.Error
	if errors.As(err, &e) {
		return e.Msg
	}
	return err.Error()
}
