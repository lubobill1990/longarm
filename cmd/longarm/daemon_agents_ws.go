package main

// The terminal of the Sessions window (docs/agent-sessions-ui.md section
// 3.4): one Sessions.Attach stream per WebSocket, replay included
// (internal/termws: the protocol, shared with the phone app).

import (
	"net/http"

	"longarm/internal/termws"
)

// attachWS is GET /v1/workers/{worker}/sessions/{name}/attach?cols=&rows=
// and GET /v1/workers/{worker}/jobs/{id}/attach (a steerable run).
func (d *daemon) attachWS(w http.ResponseWriter, r *http.Request) {
	conn, err := d.conn(r.PathValue("worker"))
	if err != nil {
		agentError(w, err)
		return
	}
	open := termws.Open(r, r.PathValue("name"), false)
	if id := r.PathValue("id"); id != "" {
		open = termws.Open(r, id, true)
	}
	// any origin: the daemon's local API is the user's alone; the tray
	// checks the browser's origin
	termws.Serve(w, r, conn.Sessions(), open)
}
