package tray

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/localapi"
	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/signin"
	"github.com/lubobill1990/longarm/internal/webui"
)

// Desktop is what the shell (window, tray, OS) does for the server.
type Desktop interface {
	Hide()
	SetPinned(bool)
	Pinned() bool
}

// Options configure the tray.
type Options struct {
	LongarmConfigDir string // longarm's config dir (daemon.addr, tls/)
	LongarmdDataDir  string // longarmd's data dir (longarmd.local, tls/)
	LongarmExe       string // longarm binary (CLI actions)
	LongarmdExe      string // longarmd binary
	HubURL           string // the hub's web panel ("" = derived from hub.toml)
	Roles            string // "client,worker" override ("" = detect)
	Version          string
	Log              *slog.Logger
	// ShowSignIn (--signin, after an install): open the popup at once when
	// a sign-in is needed.
	ShowSignIn bool
}

// Server is longarm-tray's loopback HTTP server: the web UI, the local APIs of
// longarm daemon (/local/longarm/) and longarmd (/local/longarmd/) and the tray's actions
// (/trayapi/). Only whoever opens a URL minted by URL() gets in: its
// ?token= is a one-time bootstrap code (valid for BootstrapTTL), exchanged
// for a session cookie with a separate random value (SameSite=Strict,
// HttpOnly), so a code seen on a command line or in browser history is
// worthless once used;
// Host must be the loopback address (no DNS rebinding); state-changing
// requests must be JSON (no cross-site forms).
type Server struct {
	opts    Options
	desk    Desktop
	session string // cookie value
	ln      net.Listener
	srv     *http.Server
	runCmd  func(name string, args ...string) ([]byte, error) // for tests
	openURL func(string) error
	term    func(argv []string) error
	// sign-in (signin.go); hooks for tests
	signin           signinRun
	signIn           func(context.Context, signin.Options, signin.Events) (*signin.Result, error)
	installedRolesFn func() string
	installedWorker  func() (string, longarmd.AdminInstall)
	runElevated      func(exe string, args ...string) error
	afterSignIn      func()
	// role changes (roles.go); hooks for tests
	roleRun         rolesRun
	machineRoles    func() longarmd.MachineRoles
	currentSID      func() (string, error)
	runElevatedWait func(exe string, args ...string) error
	rolesResult     func() string
	cloudPC         func() string
	nowFn           func() time.Time
	// OnSignInCode (the shell): a sign-in shows its code - keep the popup
	// up, copy the code, notify.
	OnSignInCode func(SignInCode)

	mu       sync.Mutex
	headless bool
	codes    map[string]time.Time // bootstrap codes -> expiry
}

// BootstrapTTL is how long a URL from URL() can be opened (once).
var BootstrapTTL = 10 * time.Minute

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const cookieName = "longarm_tray"

// NewServer listens on addr ("" = 127.0.0.1:0).
func NewServer(opts Options, desk Desktop, addr string) (*Server, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("longarm-tray serves on loopback only, not %s", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{opts: opts, desk: desk, session: randomHex(24), ln: ln, codes: map[string]time.Time{},
		runCmd: func(name string, args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := hiddenCommand(ctx, name, args...)
			return cmd.CombinedOutput()
		},
		openURL: OpenURL, term: OpenTerminal,
		installedRolesFn: signin.InstalledRoles, installedWorker: longarmd.InstalledWorker, runElevated: RunElevated}
	if runtime.GOOS == "windows" {
		s.machineRoles = func() longarmd.MachineRoles { return longarmd.LoadMachineRoles(longarmd.MachineSettings()) }
		s.currentSID = longarmd.CurrentUserSID
		s.runElevatedWait = RunElevatedWait
		s.rolesResult = func() string { v, _ := longarmd.MachineSettings().Get(longarmd.RegRolesResult); return v }
		var once sync.Once
		var why string
		s.cloudPC = func() string {
			once.Do(func() { _, why = longarmd.LooksLikeCloudPC(longarmd.SystemProbe{}) })
			return why
		}
	}
	s.afterSignIn = func() {
		// the daemon loads the new certificate when it starts
		s.runCmd(s.opts.LongarmExe, s.longarmArgs("daemon", "stop")[1:]...)
		s.runCmd(s.opts.LongarmExe, s.longarmArgs("daemon", "start")[1:]...)
	}
	s.srv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return s, nil
}

// Serve serves until Close.
func (s *Server) Serve() error {
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Close() error { return s.srv.Close() }

// Addr is the listening address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// URL is the page at path (which may carry a query) with a fresh one-time
// bootstrap code.
func (s *Server) URL(path string) string {
	code := randomHex(16)
	s.mu.Lock()
	now := time.Now()
	for c, exp := range s.codes {
		if now.After(exp) {
			delete(s.codes, c)
		}
	}
	s.codes[code] = now.Add(BootstrapTTL)
	s.mu.Unlock()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return "http://" + s.Addr() + path + sep + "token=" + code
}

// redeem consumes a bootstrap code.
func (s *Server) redeem(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c, exp := range s.codes {
		if subtle.ConstantTimeCompare([]byte(c), []byte(code)) == 1 {
			delete(s.codes, c)
			return time.Now().Before(exp)
		}
	}
	return false
}

// SetHeadless marks the smoke mode (shown in /trayapi/info).
func (s *Server) SetHeadless(v bool) {
	s.mu.Lock()
	s.headless = v
	s.mu.Unlock()
}

// Handler is the full handler (with the access checks).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/local/longarm/", s.proxy("/local/longarm", func() string { return filepath.Join(s.opts.LongarmConfigDir, "daemon.addr") }))
	mux.Handle("/local/longarmd/", s.proxy("/local/longarmd", func() string { return filepath.Join(s.opts.LongarmdDataDir, "longarmd.local") }))
	mux.HandleFunc("GET /trayapi/info", s.info)
	mux.HandleFunc("POST /trayapi/attach", s.attach)
	mux.HandleFunc("POST /trayapi/ssh", s.ssh)
	mux.HandleFunc("POST /trayapi/open", s.open)
	mux.HandleFunc("POST /trayapi/panel", s.panel)
	mux.HandleFunc("POST /trayapi/pin", s.pin)
	mux.HandleFunc("POST /trayapi/hide", func(w http.ResponseWriter, _ *http.Request) {
		if s.desk != nil {
			s.desk.Hide()
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /trayapi/daemon/start", s.startDaemon)
	mux.HandleFunc("POST /trayapi/join", s.join)
	mux.HandleFunc("GET /trayapi/signin/state", s.signinState)
	mux.HandleFunc("POST /trayapi/signin/start", s.signinStart)
	mux.HandleFunc("POST /trayapi/signin/cancel", s.signinCancel)
	mux.HandleFunc("POST /trayapi/signout", s.signout)
	mux.HandleFunc("POST /trayapi/admin/disable", s.adminDisable)
	mux.HandleFunc("POST /trayapi/roles", s.rolesChange)
	mux.HandleFunc("GET /trayapi/roles/state", s.rolesState)
	s.settingsRoutes(mux) // prefs.go
	mux.Handle("/", webui.Handler("/local/", "/trayapi/"))
	return s.guard(mux)
}

// guard enforces the loopback Host, the token and the JSON rule.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, port, err := net.SplitHostPort(r.Host)
		_, myPort, _ := net.SplitHostPort(s.Addr())
		if err != nil || port != myPort || (host != "127.0.0.1" && host != "localhost" && host != "[::1]" && host != "::1") {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		if t := r.URL.Query().Get("token"); t != "" {
			if !s.redeem(t) {
				// used or expired: an existing session still gets in
				if c, err := r.Cookie(cookieName); err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session)) != 1 {
					http.Error(w, "this link was already used or has expired; open longarm-tray from its tray icon", http.StatusForbidden)
					return
				}
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			q := r.URL.Query()
			q.Del("token")
			u := *r.URL
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session)) != 1 {
			http.Error(w, "open longarm-tray from its tray icon (missing token)", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// proxy forwards prefix/... to the local API whose address is in addrFile
// (read per request: the daemon or longarmd may restart on a new channel).
func (s *Server) proxy(prefix string, addrFile func() string) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "local", "local"
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
			pr.Out.URL.RawPath = ""
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				a, err := localapi.ReadAddr(addrFile())
				if err != nil {
					return nil, err
				}
				return platform.DialLocal(ctx, a)
			},
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
		},
		FlushInterval: -1, // SSE
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": "local API unreachable: " + err.Error()})
		},
	}
	return rp
}

// ---------------------------------------------------------------- /trayapi

// Roles reports which roles are installed here: client (longarm has a
// certificate or a running daemon) and worker (longarmd has a data dir with a
// config, certificate or running instance).
func (s *Server) Roles() (client, worker bool) {
	if r := s.opts.Roles; r != "" {
		return strings.Contains(r, "client"), strings.Contains(r, "worker")
	}
	exists := func(p ...string) bool { _, err := os.Stat(filepath.Join(p...)); return err == nil }
	client = exists(s.opts.LongarmConfigDir, "tls", "node.crt") || exists(s.opts.LongarmConfigDir, "daemon.addr")
	worker = exists(s.opts.LongarmdDataDir, "longarmd.toml") || exists(s.opts.LongarmdDataDir, "tls", "node.crt") ||
		exists(s.opts.LongarmdDataDir, "longarmd.local") || exists(s.opts.LongarmdDataDir, "hub.toml")
	if !client && !worker {
		client = true
	}
	return client, worker
}

func (s *Server) localUp(addrFile string) error {
	a, err := localapi.ReadAddr(addrFile)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := platform.DialLocal(ctx, a)
	if err != nil {
		return err
	}
	c.Close()
	return nil
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	client, worker := s.Roles()
	errText := func(err error) string {
		if err == nil {
			return ""
		}
		if errors.Is(err, os.ErrNotExist) {
			return "not running"
		}
		return err.Error()
	}
	cerr := s.localUp(filepath.Join(s.opts.LongarmConfigDir, "daemon.addr"))
	werr := s.localUp(filepath.Join(s.opts.LongarmdDataDir, "longarmd.local"))
	_, terr := os.Stat(filepath.Join(s.opts.LongarmdDataDir, "tls", "node.crt"))
	_, herr := os.Stat(filepath.Join(s.opts.LongarmdDataDir, "hub.toml"))
	pinned := false
	if s.desk != nil {
		pinned = s.desk.Pinned()
	}
	s.mu.Lock()
	headless := s.headless
	s.mu.Unlock()
	writeJSON(w, map[string]any{
		"version": s.opts.Version,
		"roles":   map[string]bool{"client": client, "worker": worker},
		"client":  map[string]any{"running": cerr == nil, "error": errText(cerr), "config_dir": s.opts.LongarmConfigDir},
		"worker":  map[string]any{"running": werr == nil, "enrolled": terr == nil || herr == nil, "error": errText(werr), "data_dir": s.opts.LongarmdDataDir},
		"hub_url": s.hubURL(), "pinned": pinned, "headless": headless,
		"account": s.account(),
	})
}

func (s *Server) hubURL() string { return HubURL(s.opts) }

// NeedsSignIn reports whether a sign-in would enroll something here.
func (s *Server) NeedsSignIn() bool { return s.account().Needed }

// HubURL is the web panel: --hub-url, else https://<hub host>/ from hub.toml.
func HubURL(o Options) string {
	if o.HubURL != "" {
		return o.HubURL
	}
	for _, dir := range []string{o.LongarmConfigDir, o.LongarmdDataDir} {
		b, err := os.ReadFile(filepath.Join(dir, "hub.toml"))
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(l, "=")
			if ok && strings.TrimSpace(k) == "address" {
				addr := strings.Trim(strings.TrimSpace(v), `"`)
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					host = addr
				}
				if host != "" {
					return "https://" + host + "/"
				}
			}
		}
	}
	return ""
}

func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

// validWorker: names go on command lines.
func validWorker(n string) bool {
	if n == "" || len(n) > 64 || strings.HasPrefix(n, "-") {
		return false
	}
	for _, c := range n {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (s *Server) longarmArgs(args ...string) []string {
	argv := []string{s.opts.LongarmExe}
	if s.opts.LongarmConfigDir != "" {
		argv = append(argv, "--config-dir", s.opts.LongarmConfigDir)
	}
	return append(argv, args...)
}

// attach opens a terminal running `longarm attach <worker> [--session S]`.
func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	var body struct{ Worker, Session string }
	if !readBody(w, r, &body) {
		return
	}
	if !validWorker(body.Worker) || (body.Session != "" && !validWorker(body.Session)) {
		httpError(w, http.StatusBadRequest, "bad worker or session name")
		return
	}
	args := []string{"attach", body.Worker}
	if body.Session != "" {
		args = append(args, "--session", body.Session)
	}
	if err := s.term(s.longarmArgs(args...)); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ssh opens a terminal running `longarm ssh <worker>`: OpenSSH with longarm
// proxy as its ProxyCommand, built on the fly - no ~/.ssh/config entries
// (longarm ssh-config) needed.
func (s *Server) ssh(w http.ResponseWriter, r *http.Request) {
	var body struct{ Worker string }
	if !readBody(w, r, &body) {
		return
	}
	if !validWorker(body.Worker) {
		httpError(w, http.StatusBadRequest, "bad worker name")
		return
	}
	if err := s.term(s.longarmArgs("ssh", body.Worker)); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// open opens an http(s) URL in the default browser.
func (s *Server) open(w http.ResponseWriter, r *http.Request) {
	var body struct{ URL string }
	if !readBody(w, r, &body) {
		return
	}
	u, err := url.Parse(body.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		httpError(w, http.StatusBadRequest, "only http(s) URLs")
		return
	}
	if err := s.openURL(u.String()); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// panel opens the hub's web panel.
func (s *Server) panel(w http.ResponseWriter, r *http.Request) {
	var body struct{ Path string }
	if !readBody(w, r, &body) {
		return
	}
	base := s.hubURL()
	if base == "" {
		httpError(w, http.StatusNotFound, "no hub configured")
		return
	}
	p := strings.TrimPrefix(body.Path, "/")
	if strings.Contains(p, "://") || strings.HasPrefix(p, "/") {
		httpError(w, http.StatusBadRequest, "bad path")
		return
	}
	if err := s.openURL(strings.TrimSuffix(base, "/") + "/" + p); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) pin(w http.ResponseWriter, r *http.Request) {
	var body struct{ Pinned bool }
	if !readBody(w, r, &body) {
		return
	}
	if s.desk != nil {
		s.desk.SetPinned(body.Pinned)
		body.Pinned = s.desk.Pinned()
	}
	writeJSON(w, map[string]bool{"pinned": body.Pinned})
}

// startDaemon runs `longarm daemon start`.
func (s *Server) startDaemon(w http.ResponseWriter, _ *http.Request) {
	out, err := s.runCmd(s.opts.LongarmExe, s.longarmArgs("daemon", "start")[1:]...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, strings.TrimSpace(string(out)+" "+err.Error()))
		return
	}
	writeJSON(w, map[string]string{"output": string(out)})
}

// join runs `longarmd join <hub> <code> --no-wait [--ca-fingerprint F] [--name N]`.
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var body struct{ Hub, Code, Fingerprint, Name string }
	if !readBody(w, r, &body) {
		return
	}
	safe := func(v string, max int) bool {
		if len(v) > max || strings.HasPrefix(v, "-") {
			return false
		}
		for _, c := range v {
			if c <= ' ' || c == '"' || c == '\'' || c == '`' || c == ';' || c == '&' || c == '|' {
				return false
			}
		}
		return true
	}
	if body.Hub == "" || body.Code == "" || !safe(body.Hub, 256) || !safe(body.Code, 16) || !safe(body.Fingerprint, 128) || (body.Name != "" && !validWorker(body.Name)) {
		httpError(w, http.StatusBadRequest, "bad hub, code, fingerprint or name")
		return
	}
	args := []string{"join", body.Hub, body.Code, "--no-wait", "--data-dir", s.opts.LongarmdDataDir}
	if body.Fingerprint != "" {
		args = append(args, "--ca-fingerprint", body.Fingerprint)
	}
	if body.Name != "" {
		args = append(args, "--name", body.Name)
	}
	out, err := s.runCmd(s.opts.LongarmdExe, args...)
	if err != nil {
		httpError(w, http.StatusBadRequest, strings.TrimSpace(string(out)+" "+err.Error()))
		return
	}
	writeJSON(w, map[string]string{"output": string(out)})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
