package hub

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/version"
	"github.com/lubobill1990/longarm/internal/webui"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The web panel (P1.5): the shared frontend (internal/webui) is served at /
// (client-side routes fall back to index.html); the server-rendered status
// page moved to /status. API additions for the panel, each mirroring a CLI
// command:
//
//	GET    /api/me                         + hub_address, ca_fingerprint, base_url
//	POST   /api/jobs                       longarm admin run   {worker, argv | command, name, cwd, env, require, mem_gb}
//	POST   /api/jobs/{id}/kill?worker=     longarm admin kill
//	GET    /api/jobs/{id}/logs?follow=1    longarm admin logs  (SSE, event "log": LogChunk, then "end" or "error")
//	POST   /api/nodes/{name}/toolchain/apply  longarm admin toolchain-apply
//	GET    /api/history?node=              metrics history (sparklines)
//	GET    /api/links                      longarm admin links (per client -> worker path / RTT)
//	GET    /api/install?code=&role=        the one-line install commands for a join code
//	GET    /api/users                      longarm-hub user ls
//	POST   /api/users                      longarm-hub user add  {name, password, reset, current_password} -> totp secret + otpauth URI (once)
//	DELETE /api/users/{name}               longarm-hub user rm   {current_password}
//	(account changes need the caller's current password and are audited as node "hub")

// panelCSP: everything from this origin; inline styles for the charts.
const panelCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func (h *Hub) panelRoutes(mux *http.ServeMux, api, admin func(string, func(http.ResponseWriter, *http.Request, string))) {
	spa := webui.Handler("/api/", "/releases/")
	mux.Handle("GET /{$}", withCSP(spa))
	mux.Handle("GET /assets/", withCSP(spa))
	for _, p := range []string{"nodes", "jobs", "joincodes", "profiles", "audit", "users", "links", "login", "tray", "tenants", "me"} {
		mux.Handle("GET /"+p, withCSP(spa))
		mux.Handle("GET /"+p+"/", withCSP(spa))
	}
	mux.Handle("GET /favicon.svg", withCSP(spa))
	mux.HandleFunc("GET /status", h.statusPage)

	api("POST /api/jobs", func(w http.ResponseWriter, r *http.Request, user string) {
		var body struct {
			Worker  string            `json:"worker"`
			Argv    []string          `json:"argv"`
			Command string            `json:"command"`
			Name    string            `json:"name"`
			Cwd     string            `json:"cwd"`
			Env     map[string]string `json:"env"`
			Require map[string]string `json:"require"`
			MemGB   float64           `json:"mem_gb"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		argv := body.Argv
		if len(argv) == 0 {
			argv = splitCommand(body.Command)
		}
		fj, err := h.SubmitJobAs(r.Context(), "web/"+user, h.panelViewer(user).User, h.panelAdmin(user), &pb.HubSubmitRequest{Worker: body.Worker, Argv: argv, Name: body.Name,
			Cwd: body.Cwd, Env: body.Env, Require: body.Require, MemGb: body.MemGB})
		writeResult(w, fj, err)
	})
	api("POST /api/jobs/{id}/kill", func(w http.ResponseWriter, r *http.Request, user string) {
		id, worker := r.PathValue("id"), r.URL.Query().Get("worker")
		if err := h.mayControl(h.panelViewer(user), id, worker); err != nil {
			writeResult(w, nil, err)
			return
		}
		fj, err := h.KillJob(r.Context(), "web/"+user, h.panelViewer(user).User, id, worker)
		writeResult(w, fj, err)
	})
	api("GET /api/jobs/{id}/logs", func(w http.ResponseWriter, r *http.Request, user string) {
		q := r.URL.Query()
		off, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
		req := &pb.HubLogsRequest{Id: r.PathValue("id"), Worker: q.Get("worker"), Offset: off, Follow: q.Get("follow") == "1" || q.Get("follow") == "true"}
		if _, err := h.GetJobFor(h.panelViewer(user), req.Id, req.Worker); err != nil {
			writeResult(w, nil, err)
			return
		}
		h.sseRun(w, r, func(send func(kind string, m any) error) error {
			return h.JobLogs(r.Context(), "web/"+user, h.panelViewer(user).User, req, func(c *pb.LogChunk) error { return send("log", c) })
		})
	})
	admin("POST /api/nodes/{name}/toolchain/apply", func(w http.ResponseWriter, r *http.Request, user string) {
		info, err := h.ToolchainApply(r.Context(), "web/"+user, r.PathValue("name"))
		writeResult(w, info, err)
	})
	api("GET /api/history", func(w http.ResponseWriter, r *http.Request, user string) {
		writePB(w, h.HistoryFor(h.panelViewer(user), r.URL.Query().Get("node")))
	})
	admin("GET /api/links", func(w http.ResponseWriter, r *http.Request, _ string) {
		writePB(w, h.Links())
	})
	admin("GET /api/install", func(w http.ResponseWriter, r *http.Request, _ string) {
		q := r.URL.Query()
		code, role := q.Get("code"), q.Get("role")
		if code == "" {
			code = "CODE"
		}
		if !joinCodeArgRE(code) || (role != "" && role != "worker" && role != "client" && role != "admin") {
			httpError(w, http.StatusBadRequest, "bad code or role")
			return
		}
		writeJSON(w, installCommands(h.baseURL(r), code, role))
	})
	admin("GET /api/users", func(w http.ResponseWriter, _ *http.Request, _ string) {
		names, err := h.store.Users()
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		type userInfo struct {
			Name      string    `json:"name"`
			CreatedAt time.Time `json:"created_at"`
			Role      string    `json:"role"`
			Tenant    string    `json:"tenant,omitempty"`
		}
		out := []userInfo{}
		for _, n := range names {
			ui := userInfo{Name: n, Role: "admin"}
			if u, err := h.store.GetUser(n); err == nil {
				ui.CreatedAt = u.CreatedAt
				if !u.Admin() {
					ui.Role, ui.Tenant = u.Role, u.Tenant
				}
			}
			out = append(out, ui)
		}
		writeJSON(w, map[string]any{"users": out})
	})
	admin("POST /api/users", func(w http.ResponseWriter, r *http.Request, user string) {
		var body struct {
			Name     string `json:"name"`
			Password string `json:"password"`
			Reset    bool   `json:"reset"`
			Current  string `json:"current_password"`
		}
		if !readJSON(w, r, &body) || !h.reauth(w, r, user, body.Current) {
			return
		}
		secret, err := AddUser(h.store, body.Name, body.Password, body.Reset)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.Reset { // a reset ends the account's sessions
			h.store.DeleteUserSessions(body.Name)
		}
		h.auditHub("web/"+user, map[bool]string{true: "users/reset", false: "users/add"}[body.Reset], map[string]any{"user": body.Name})
		h.log.Info("panel user added", "user", body.Name, "by", "web/"+user, "reset", body.Reset)
		writeJSON(w, map[string]string{"name": body.Name, "totp_secret": secret, "otpauth": TOTPURI(body.Name, secret)})
	})
	admin("DELETE /api/users/{name}", func(w http.ResponseWriter, r *http.Request, user string) {
		var body struct {
			Current string `json:"current_password"`
		}
		if !readJSON(w, r, &body) || !h.reauth(w, r, user, body.Current) {
			return
		}
		name := r.PathValue("name")
		if name == user {
			httpError(w, http.StatusConflict, "you cannot delete your own account")
			return
		}
		names, err := h.store.Users()
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		if !slices.Contains(names, name) {
			httpError(w, http.StatusNotFound, "no user "+name)
			return
		}
		if err := h.store.DeleteUser(name); err != nil {
			httpError(w, 500, err.Error())
			return
		}
		h.auditHub("web/"+user, "users/delete", map[string]any{"user": name})
		h.log.Info("panel user deleted", "user", name, "by", "web/"+user)
		writeJSON(w, map[string]bool{"ok": true})
	})
}

// reauth checks the panel user's current password before account changes
// (a stolen session alone cannot create or reset accounts).
func (h *Hub) reauth(w http.ResponseWriter, r *http.Request, user, password string) bool {
	ip := h.clientIP(r)
	if h.loginLimit.blocked(ip) { // failures count like failed logins
		httpError(w, http.StatusTooManyRequests, errTooMany.Error())
		return false
	}
	u, err := h.store.GetUser(user)
	if err != nil || !checkPassword(u.PassHash, password) {
		h.loginLimit.allow(ip)
		httpError(w, http.StatusForbidden, "your current password is required for account changes")
		return false
	}
	return true
}

// auditHub records an action taken on the hub itself (node "hub").
func (h *Hub) auditHub(who, method string, fields map[string]any) {
	rec := map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "who": who, "method": method}
	for k, v := range fields {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	if err := h.store.AddAudit([]store.AuditRow{{Node: "hub", Time: time.Now(), Who: who, Method: method, Line: string(b)}}); err != nil {
		h.log.Warn("audit", "err", err)
	}
}

func withCSP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", panelCSP)
		next.ServeHTTP(w, r)
	})
}

// meInfo is GET /api/me (+ role: admin | user; for users their Longarm
// user, teams, quota and usage).
func (h *Hub) meInfo(r *http.Request, user string) map[string]any {
	v := h.panelViewer(user)
	role := "admin"
	if !h.panelAdmin(user) {
		role = "user"
	}
	m := map[string]any{"user": user, "hub": h.cfg.Name, "version": version.Version,
		"hub_address": h.hubAddr(r), "ca_fingerprint": pki.Fingerprint(h.ca.Cert), "base_url": h.baseURL(r),
		"role": role, "tenant": v.User, "teams": v.Teams}
	h.mu.Lock()
	m["tenancy"] = h.tenancyOnLocked()
	if q := h.quotaLocked(v.User); !tenancy.QuotaEmpty(q) {
		m["quota"] = json.RawMessage(protoJSON(q))
	}
	m["usage"] = json.RawMessage(protoJSON(h.usageLocked(v.User)))
	if t := h.tenants[v.User]; t != nil {
		m["can_share"] = t.u.CanShare
	}
	h.mu.Unlock()
	return m
}

// joinCodeArgRE accepts what may go into a generated command line: join
// codes ("K7P-Q2M") or the placeholder.
func joinCodeArgRE(s string) bool {
	if len(s) == 0 || len(s) > 16 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// installCommands are the one-line installs shown with a new join code
// (the scripts are served by the hub: GET /install.ps1, /install.sh).
func installCommands(base, code, role string) map[string]string {
	ps, sh := "", ""
	if role != "" && role != "worker" {
		ps = " -Role " + role
		sh = " --role " + role
	}
	return map[string]string{
		"windows":  "& ([scriptblock]::Create((irm " + base + "/install.ps1))) -Join " + code + ps,
		"linux":    "curl -fsSL " + base + "/install.sh | sh -s -- --join " + code + sh,
		"longarmd": "longarmd join <hub-address> " + code + " --ca-fingerprint <fingerprint>",
	}
}

// splitCommand splits a command line on spaces, honouring "double" and
// 'single' quotes (no escapes): the panel's one-line command field.
func splitCommand(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, c := range s {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '"' || c == '\'':
			quote, inWord = c, true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// sseRun streams events from run (send(kind, proto message or value)); an
// error ends the stream with an "error" event, success with "end".
func (h *Hub) sseRun(w http.ResponseWriter, r *http.Request, run func(send func(kind string, m any) error) error) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpError(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Write([]byte(": longarm-hub\n\n"))
	fl.Flush()
	err := run(func(kind string, m any) error {
		b, err := marshalAny(m)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte("event: " + kind + "\ndata: " + string(b) + "\n\n")); err != nil {
			return err
		}
		fl.Flush()
		return nil
	})
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		b, _ := marshalAny(map[string]string{"error": errMessage(err)})
		w.Write([]byte("event: error\ndata: " + string(b) + "\n\n"))
	} else {
		w.Write([]byte("event: end\ndata: {}\n\n"))
	}
	fl.Flush()
}

func marshalAny(m any) ([]byte, error) {
	if pm, ok := m.(proto.Message); ok {
		return pj.Marshal(pm)
	}
	return json.Marshal(m)
}

func errMessage(err error) string { return status.Convert(err).Message() }
