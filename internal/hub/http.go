package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The HTTP listener (127.0.0.1:7480 behind Caddy) serves:
//   - public: /healthz, /install.ps1, /install.sh, /releases/..., /ca.crt,
//     /.well-known/longarm-hub (gRPC address + CA fingerprint, domain-only sign-in)
//   - the JSON/SSE API under /api/ (mirrors the gRPC admin surface), for a
//     logged-in panel user (cookie session after password + TOTP)
//   - the web panel (internal/webui) at /, see panel.go; a minimal
//     server-rendered status page at /status
//
// CSRF: the session cookie is SameSite=Strict and HttpOnly, and state
// changing /api requests must be application/json (a cross-site form can't
// send that without a CORS preflight, which is never granted).

const sessionCookie = "longarm_hub_session"

var pj = protojson.MarshalOptions{UseProtoNames: true}

func (h *Hub) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /ca.crt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(h.ca.CertPEM)
	})
	// Domain-only sign-in (hubclient.ResolveHub): where nodes dial, and
	// the CA /ca.crt serves - read over the panel's WebPKI HTTPS.
	mux.HandleFunc("GET "+hubclient.WellKnownPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, hubclient.WellKnown{Address: h.hubAddr(r), CAFingerprint: pki.Fingerprint(h.ca.Cert), Name: h.cfg.Name})
	})
	mux.HandleFunc("GET /install.sh", h.installScript("sh"))
	mux.HandleFunc("GET /install.ps1", h.installScript("ps1"))
	mux.HandleFunc("GET /releases/latest.json", h.latestJSON)
	mux.HandleFunc("GET /releases/{version}/{file}", h.releaseFile)
	// a version's record, for the LongarmUpdate service (releasejson.go; the latest: latestJSON)
	mux.HandleFunc("GET /releases/{version}/release.json", h.releaseVersionJSON)

	mux.HandleFunc("POST /api/login", h.apiLogin)
	mux.HandleFunc("POST /login", h.formLogin)
	mux.HandleFunc("POST /logout", h.formLogout)
	// api: any logged-in panel account (users see only their own jobs);
	// admin: panel administrators only.
	api := func(pattern string, fn func(w http.ResponseWriter, r *http.Request, user string)) {
		mux.HandleFunc(pattern, h.requireUser(fn))
	}
	admin := func(pattern string, fn func(w http.ResponseWriter, r *http.Request, user string)) {
		mux.HandleFunc(pattern, h.requireUser(h.adminOnly(fn)))
	}
	api("POST /api/logout", h.apiLogout)
	api("GET /api/me", func(w http.ResponseWriter, r *http.Request, user string) {
		writeJSON(w, h.meInfo(r, user))
	})
	api("GET /api/nodes", func(w http.ResponseWriter, r *http.Request, user string) {
		writePB(w, &pb.ListNodesResponse{Nodes: h.ListNodesFor(h.panelViewer(user), r.URL.Query().Get("all") != "")})
	})
	nodeAction := func(fn func(by, name string) (*pb.NodeInfo, error)) func(http.ResponseWriter, *http.Request, string) {
		return func(w http.ResponseWriter, r *http.Request, user string) {
			info, err := fn("web/"+user, r.PathValue("name"))
			writeResult(w, info, err)
		}
	}
	admin("POST /api/nodes/{name}/approve", nodeAction(h.Approve))
	admin("POST /api/nodes/{name}/revoke", nodeAction(h.Revoke))
	admin("POST /api/nodes/{name}/drain", nodeAction(func(by, n string) (*pb.NodeInfo, error) { return h.SetDrain(by, n, true) }))
	admin("POST /api/nodes/{name}/undrain", nodeAction(func(by, n string) (*pb.NodeInfo, error) { return h.SetDrain(by, n, false) }))
	admin("PUT /api/nodes/{name}/labels", func(w http.ResponseWriter, r *http.Request, user string) {
		var labels map[string]string
		if !readJSON(w, r, &labels) {
			return
		}
		info, err := h.SetLabels("web/"+user, r.PathValue("name"), labels)
		writeResult(w, info, err)
	})
	admin("PUT /api/nodes/{name}/profile", func(w http.ResponseWriter, r *http.Request, user string) {
		var body struct {
			Profile string `json:"profile"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		info, err := h.AssignProfile("web/"+user, r.PathValue("name"), body.Profile)
		writeResult(w, info, err)
	})
	api("GET /api/jobs", func(w http.ResponseWriter, r *http.Request, user string) {
		q := r.URL.Query()
		req := &pb.FleetJobsRequest{Worker: q.Get("worker")}
		req.Limit = int32(atoiDefault(q.Get("limit"), 0))
		for _, s := range q["state"] {
			if v, ok := pb.JobState_value["JOB_STATE_"+strings.ToUpper(s)]; ok {
				req.States = append(req.States, pb.JobState(v))
			}
		}
		writePB(w, &pb.FleetJobsResponse{Jobs: h.ListJobsFor(h.panelViewer(user), req)})
	})
	api("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request, user string) {
		fj, err := h.GetJobFor(h.panelViewer(user), r.PathValue("id"), r.URL.Query().Get("worker"))
		writeResult(w, fj, err)
	})
	admin("POST /api/joincodes", func(w http.ResponseWriter, r *http.Request, user string) {
		var body struct {
			Role    string            `json:"role"`
			TTL     string            `json:"ttl"`
			Labels  map[string]string `json:"labels"`
			Profile string            `json:"profile"`
			Name    string            `json:"name"`
			User    string            `json:"user"`
			Owner   string            `json:"owner"` // worker codes: the user who will own the worker
		}
		if !readJSON(w, r, &body) {
			return
		}
		req := &pb.CreateJoinCodeRequest{Role: body.Role, Labels: body.Labels, Profile: body.Profile, Name: body.Name, User: body.User, Owner: body.Owner}
		if body.TTL != "" {
			d, err := time.ParseDuration(body.TTL)
			if err != nil {
				httpError(w, http.StatusBadRequest, "bad ttl")
				return
			}
			req.Ttl = durationpb.New(d)
		}
		jc, err := h.CreateJoinCode("web/"+user, req)
		writeResult(w, jc, err)
	})
	admin("GET /api/joincodes", func(w http.ResponseWriter, _ *http.Request, _ string) {
		res, err := h.ListJoinCodes()
		writeResult(w, res, err)
	})
	admin("GET /api/audit", func(w http.ResponseWriter, r *http.Request, _ string) {
		q := r.URL.Query()
		res, err := h.Audit(&pb.AuditRequest{Node: q.Get("node"), Limit: int32(atoiDefault(q.Get("limit"), 200))})
		writeResult(w, res, err)
	})
	admin("GET /api/profiles", func(w http.ResponseWriter, _ *http.Request, _ string) {
		writePB(w, &pb.ListProfilesResponse{Profiles: h.Profiles()})
	})
	admin("PUT /api/profiles/{name}", func(w http.ResponseWriter, r *http.Request, user string) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil || !jsonRequest(w, r) {
			return
		}
		p := &pb.Profile{}
		if err := protojson.Unmarshal(body, p); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		p.Name = r.PathValue("name")
		res, err := h.PutProfile("web/"+user, p)
		writeResult(w, res, err)
	})
	admin("GET /api/releases", func(w http.ResponseWriter, _ *http.Request, _ string) {
		rels, err := h.store.Releases()
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		writeJSON(w, rels)
	})
	api("GET /api/events", h.sse)
	h.panelRoutes(mux, api, admin)
	h.tenancyRoutes(api, admin)
	h.workerRoutes(api)
	h.entraRoutes(mux)
	h.linkRoutes(mux)
	h.portLinkRoutes(api)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		// same-origin, not no-referrer: under no-referrer a browser sends
		// "Origin: null" with form posts, and the hub's own forms (/link,
		// /status login) would fail their same-origin check; other sites
		// still get no referrer
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (h *Hub) healthz(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	online, workers := 0, 0
	for _, n := range h.nodes {
		if n.rec.Role == string(pki.RoleWorker) && n.rec.State == stateApproved {
			workers++
			if n.sess != nil {
				online++
			}
		}
	}
	h.mu.Unlock()
	resp := map[string]any{"ok": true, "version": version.Version, "workers": workers, "online": online,
		"uptime_seconds": int(time.Since(h.started).Seconds()), "schema": h.store.SchemaVersion()}
	// Headscale's state (ok, starting, down; the detail is in longarm admin
	// tailnet status): "ok" stays the hub's own, a container probe must not
	// restart the hub for its sidecar
	if st, _ := h.headscaleHealth(); st != "" {
		resp["headscale"] = st
	}
	writeJSON(w, resp)
}

// ---------------------------------------------------------------- login

func (h *Hub) login(r *http.Request, user, pass, code string) (string, error) {
	if !h.loginLimit.allow(h.clientIP(r)) {
		return "", errTooMany
	}
	h.loginMu.Lock() // the TOTP step check and update must not interleave
	defer h.loginMu.Unlock()
	u, err := h.store.GetUser(user)
	if err != nil {
		checkPassword(hashPassword("x"), pass) // same cost for unknown users
		return "", errBadLogin
	}
	if !checkPassword(u.PassHash, pass) {
		return "", errBadLogin
	}
	step, err := checkTOTP(u.TOTPSecret, code, u.LastTOTP, time.Now())
	if err != nil {
		return "", errBadLogin
	}
	u.LastTOTP = step
	if err := h.store.PutUser(u); err != nil {
		return "", err
	}
	tok := randomToken()
	if err := h.store.AddWebSession(sessionHash(tok), u.Name, time.Now().Add(h.cfg.Web.SessionTTL.Duration)); err != nil {
		return "", err
	}
	h.log.Info("panel login", "user", u.Name, "remote", h.clientIP(r))
	return tok, nil
}

var (
	errBadLogin = errors.New("wrong user, password or code")
	errTooMany  = errors.New("too many attempts; wait a minute")
)

// fromProxy reports whether r came through a trusted reverse proxy (then
// its X-Forwarded-* headers are believed).
func (h *Hub) fromProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	for _, p := range h.cfg.Web.TrustedProxies {
		if _, n, err := net.ParseCIDR(p); err == nil && ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP is the client's address (the proxy's X-Forwarded-For when the
// request came through a trusted proxy).
func (h *Hub) clientIP(r *http.Request) string {
	if h.fromProxy(r) {
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			parts := strings.Split(xf, ",")
			return strings.TrimSpace(parts[len(parts)-1]) // the address the proxy saw
		}
	}
	return r.RemoteAddr
}

// forwardedHost is the Host the client used.
func (h *Hub) forwardedHost(r *http.Request) string {
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" && h.fromProxy(r) {
		host = fh
	}
	if !hostRE.MatchString(host) { // it ends up in generated scripts
		return "localhost"
	}
	return host
}

var hostRE = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$|^[[0-9A-Fa-f:.]+](:[0-9]{1,5})?$`)

func (h *Hub) setSession(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: !h.cfg.Web.InsecureCookie, SameSite: http.SameSiteStrictMode, MaxAge: int(h.cfg.Web.SessionTTL.Seconds())})
}

func (h *Hub) apiLogin(w http.ResponseWriter, r *http.Request) {
	var body struct{ User, Password, TOTP string }
	if !readJSON(w, r, &body) {
		return
	}
	tok, err := h.login(r, body.User, body.Password, body.TOTP)
	switch {
	case errors.Is(err, errTooMany):
		httpError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, errBadLogin):
		httpError(w, http.StatusUnauthorized, err.Error())
	case err != nil:
		httpError(w, 500, err.Error())
	default:
		h.setSession(w, tok)
		writeJSON(w, map[string]string{"user": body.User})
	}
}

// sameOrigin rejects cross-site form posts (Origin is sent with POSTs).
func (h *Hub) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return o == ""
	}
	o = strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://")
	return o == h.forwardedHost(r)
}

func (h *Hub) formLogin(w http.ResponseWriter, r *http.Request) {
	if !h.sameOrigin(r) {
		httpError(w, http.StatusForbidden, "cross-origin login refused")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	tok, err := h.login(r, r.FormValue("user"), r.FormValue("password"), r.FormValue("totp"))
	if err != nil {
		h.renderLogin(w, err.Error())
		return
	}
	h.setSession(w, tok)
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}

func (h *Hub) apiLogout(w http.ResponseWriter, r *http.Request, _ string) {
	h.logout(w, r)
	writeJSON(w, map[string]bool{"ok": true})
}

func (h *Hub) formLogout(w http.ResponseWriter, r *http.Request) {
	if !h.sameOrigin(r) {
		httpError(w, http.StatusForbidden, "cross-origin logout refused")
		return
	}
	h.logout(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Hub) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		h.store.DeleteWebSession(sessionHash(c.Value), time.Now())
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: !h.cfg.Web.InsecureCookie, SameSite: http.SameSiteStrictMode})
	if h.idpOn() {
		h.setEntraMark(w, "", -1)
	}
}

func (h *Hub) sessionUser(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	u, err := h.store.WebSession(sessionHash(c.Value), time.Now())
	if err != nil {
		return ""
	}
	return u
}

func (h *Hub) requireUser(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := h.sessionUser(r)
		if user == "" {
			httpError(w, http.StatusUnauthorized, "login required")
			return
		}
		if h.panelDisabled(user) {
			httpError(w, http.StatusForbidden, "this user is disabled")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !jsonRequest(w, r) {
			return
		}
		fn(w, r, user)
	}
}

// ---------------------------------------------------------------- SSE

func (h *Hub) sse(w http.ResponseWriter, r *http.Request, user string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpError(w, 500, "streaming unsupported")
		return
	}
	q := r.URL.Query()
	var kinds []string
	if k := q.Get("kinds"); k != "" {
		kinds = strings.Split(k, ",")
	}
	if len(kinds) == 0 {
		kinds = []string{"job", "node", "queue"}
	}
	// Subscribe before the headers go out: a client that sees the stream
	// open must not miss an event it triggers right after.
	sub := h.bus.subscribeFiltered(kinds, q.Get("job"), eventFilter(h.panelViewer(user)))
	defer h.bus.unsubscribe(sub)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, ": longarm-hub events\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second) // keeps proxies from timing the stream out
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if h.sessionUser(r) != user { // logged out, or the login was disabled meanwhile
				return
			}
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			b, err := pj.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, b); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// ---------------------------------------------------------------- helpers

func jsonRequest(w http.ResponseWriter, r *http.Request) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		httpError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	return true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if !jsonRequest(w, r) {
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writePB(w http.ResponseWriter, m proto.Message) {
	b, err := pj.Marshal(m)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func writeResult(w http.ResponseWriter, m proto.Message, err error) {
	if err != nil {
		code := http.StatusInternalServerError
		switch status.Code(err) {
		case codes.NotFound:
			code = http.StatusNotFound
		case codes.InvalidArgument:
			code = http.StatusBadRequest
		case codes.AlreadyExists, codes.FailedPrecondition:
			code = http.StatusConflict
		case codes.PermissionDenied:
			code = http.StatusForbidden
		case codes.ResourceExhausted:
			code = http.StatusTooManyRequests
		}
		httpError(w, code, status.Convert(err).Message())
		return
	}
	writePB(w, m)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func atoiDefault(s string, d int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return d
}

// ---------------------------------------------------------------- page

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"gb": func(b uint64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<30)) },
	"state": func(s pb.JobState) string {
		return strings.ToLower(strings.TrimPrefix(s.String(), "JOB_STATE_"))
	},
	"ago": func(t interface{ AsTime() time.Time }) string {
		if t == nil {
			return "-"
		}
		return time.Since(t.AsTime()).Round(time.Second).String()
	},
}).Parse(`<!doctype html><html><head><meta charset="utf-8"><title>longarm-hub</title>
<style>body{font:14px system-ui,sans-serif;margin:2em}table{border-collapse:collapse}td,th{padding:3px 10px;border-bottom:1px solid #ddd;text-align:left}.off{color:#999}</style>
</head><body>
{{if .Login}}<h1>longarm-hub</h1>{{if .Error}}<p style="color:#b00">{{.Error}}</p>{{end}}
<form method="post" action="/login"><p><input name="user" placeholder="user" autocomplete="username"></p>
<p><input name="password" type="password" placeholder="password" autocomplete="current-password"></p>
<p><input name="totp" placeholder="TOTP code" autocomplete="one-time-code" inputmode="numeric"></p><p><button>Log in</button></p></form>
{{range .Providers}}<p><a href="/auth/login?provider={{.ID}}&amp;next=/status">Sign in with {{.Label}}</a></p>{{end}}
{{else}}<form method="post" action="/logout" style="float:right"><button>Log out {{.User}}</button></form>
<h1>longarm-hub {{.Version}}</h1><h2>Workers</h2><table><tr><th>name</th><th>state</th><th>online</th><th>cpu%</th><th>mem GB</th><th>jobs</th><th>reserved GB</th><th>labels</th><th>version</th></tr>
{{range .Nodes}}<tr{{if not .Online}} class="off"{{end}}><td>{{.Name}}</td><td>{{.State}}{{if .Draining}} (draining){{end}}</td><td>{{.Online}}</td>
<td>{{with .Metrics}}{{printf "%.0f" .CpuPercent}}{{end}}</td><td>{{with .Metrics}}{{gb .MemUsedBytes}}/{{gb .MemTotalBytes}}{{end}}</td>
<td>{{.JobsActive}}</td><td>{{printf "%.1f" .ReservedGb}}</td><td>{{range $k, $v := .Labels}}{{$k}}={{$v}} {{end}}</td><td>{{.Version}}</td></tr>{{end}}</table>
<h2>Recent jobs</h2><table><tr><th>worker</th><th>id</th><th>name</th><th>state</th><th>exit</th><th>created</th></tr>
{{range .Jobs}}<tr><td>{{.Worker}}</td><td>{{.Job.Spec.Id}}</td><td>{{.Job.Spec.Name}}</td><td>{{state .Job.State}}</td><td>{{if .Job.HasExitCode}}{{.Job.ExitCode}}{{end}}</td><td>{{ago .Job.CreatedAt}} ago</td></tr>{{end}}</table>
<p>API: <code>/api/nodes</code>, <code>/api/jobs</code>, <code>/api/events</code> (SSE), <code>/api/audit</code>, <code>/api/profiles</code>, <code>/api/joincodes</code>.</p>{{end}}
</body></html>`))

func (h *Hub) renderLogin(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pageTmpl.Execute(w, map[string]any{"Login": true, "Error": msg, "Providers": h.webProviders()})
}

func (h *Hub) statusPage(w http.ResponseWriter, r *http.Request) {
	user := h.sessionUser(r)
	if user == "" {
		h.renderLogin(w, "")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	v := h.panelViewer(user)
	pageTmpl.Execute(w, map[string]any{"User": user, "Version": version.Version, "Nodes": h.ListNodesFor(v, false),
		"Jobs": h.ListJobsFor(v, &pb.FleetJobsRequest{Limit: 50})})
}
