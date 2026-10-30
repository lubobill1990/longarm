package tray

import (
	"net/http"
	"net/url"
	"regexp"
)

// The Sessions window (docs/agent-sessions-ui.md section 5): the web UI's
// /sessions page, told what to show by its location hash
// (worker=..&agent=..&key=..).

// SessionsPath is the Sessions window's page.
const SessionsPath = "/sessions"

// validKey: a session key (<worker>/<agent>/<id>, <worker>/session/<name>).
var validKey = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

// sessionsHash is the page's location hash (without "#") for the target;
// false when a value is not valid.
func sessionsHash(worker, agent, key string) (string, bool) {
	if worker != "" && !validWorker(worker) || agent != "" && agent != "claude" && agent != "copilot" || key != "" && !validKey.MatchString(key) {
		return "", false
	}
	q := url.Values{}
	for _, kv := range [][2]string{{"worker", worker}, {"agent", agent}, {"key", key}} {
		if kv[1] != "" {
			q.Set(kv[0], kv[1])
		}
	}
	return q.Encode(), true
}

// sessionsOpen opens the Sessions window at the target (all optional):
// the shell's window (OpenSessions), else the default browser.
func (s *Server) sessionsOpen(w http.ResponseWriter, r *http.Request) {
	var body struct{ Worker, Agent, Key string }
	if !readBody(w, r, &body) {
		return
	}
	hash, ok := sessionsHash(body.Worker, body.Agent, body.Key)
	if !ok {
		httpError(w, http.StatusBadRequest, "bad worker, agent or key")
		return
	}
	if s.OpenSessions != nil {
		s.OpenSessions(hash)
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if err := s.openURL(s.SessionsURL(hash, true)); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// SessionsURL is the Sessions page with the location hash; fresh: with a
// one-time bootstrap code (a new window or browser), else for a window
// that already has the session cookie (only the hash changes: no reload).
func (s *Server) SessionsURL(hash string, fresh bool) string {
	u := "http://" + s.Addr() + SessionsPath
	if fresh {
		u = s.URL(SessionsPath)
	}
	if hash != "" {
		u += "#" + hash
	}
	return u
}
