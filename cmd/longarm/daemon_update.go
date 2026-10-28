package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The tray's "Update to X" (Workers tab): longarm worker update <worker>
// --wait run by the daemon, its progress polled by the popup.
//
//	POST /v1/workers/{name}/update {"version": "0.1.9"} | {"latest": true}
//	GET  /v1/workers/{name}/update   the state: running | done | failed (+ the outcome)

// updateJob is one worker's update run by the daemon.
type updateJob struct {
	State    string         `json:"state"` // running | done | failed
	Progress []string       `json:"progress"`
	Outcome  *updateOutcome `json:"outcome,omitempty"`
	Error    string         `json:"error,omitempty"`
	ExitCode int            `json:"exit_code,omitempty"`
	Started  time.Time      `json:"started"`
}

type daemonUpdates struct {
	mu   sync.Mutex
	jobs map[string]*updateJob
}

func (u *daemonUpdates) get(name string) *updateJob {
	u.mu.Lock()
	defer u.mu.Unlock()
	j := u.jobs[name]
	if j == nil {
		return &updateJob{State: "idle"}
	}
	c := *j
	c.Progress = append([]string(nil), j.Progress...)
	return &c
}

func writeJSONResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (d *daemon) updateGateway(mux *http.ServeMux) {
	ups := &daemonUpdates{jobs: map[string]*updateJob{}}
	mux.HandleFunc("GET /v1/workers/{name}/update", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusOK, ups.get(r.PathValue("name")))
	})
	mux.HandleFunc("POST /v1/workers/{name}/update", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var body struct {
			Version string `json:"version"`
			Latest  bool   `json:"latest"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSONResp(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		req := &pb.MachineUpdateRequest{Version: body.Version, Latest: body.Version == ""}
		ups.mu.Lock()
		if j := ups.jobs[name]; j != nil && j.State == "running" {
			ups.mu.Unlock()
			writeJSONResp(w, http.StatusConflict, map[string]string{"error": "an update of " + name + " is running"})
			return
		}
		j := &updateJob{State: "running", Started: time.Now()}
		ups.jobs[name] = j
		ups.mu.Unlock()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
			defer cancel()
			out, err := runWorkerUpdate(ctx, d.s, name, req, true, 20*time.Minute, func(m string) {
				ups.mu.Lock()
				j.Progress = append(j.Progress, m)
				ups.mu.Unlock()
			})
			ups.mu.Lock()
			defer ups.mu.Unlock()
			j.Outcome = out
			if err != nil {
				j.State, j.Error, j.ExitCode = "failed", oneLine(err), exitCodeFor(err, true)
				return
			}
			j.State = "done"
		}()
		writeJSONResp(w, http.StatusAccepted, ups.get(name))
	})
}
