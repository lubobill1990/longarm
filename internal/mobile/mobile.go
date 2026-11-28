// Package mobile is the Longarm phone app's core (docs/mobile.md): what
// `longarm` and the tray do on a computer, in the app's own process (a phone
// runs no daemons). It serves the app's WebView the web frontend (its /m
// shell, internal/webui) and a local HTTP API on 127.0.0.1 - sign-in, the
// owner's machines, their sessions and terminals - and holds the
// connections to the hub and the workers; the native desktop viewer gets
// its stream from Desktop (desktop.go).
//
// The session routes are the longarm daemon's (/local/longarm/v1/workers/
// {w}/sessions..., the tray's Sessions window): the frontend's
// api/sessions.ts works against either. The API wants the token Start gave
// (X-Longarm-Token, or ?t= for WebSockets); the frontend's files don't (the
// same public bundle the hub serves).
//
// Only the signed-in user's own machines: the workers whose owner they are.
package mobile

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"longarm/internal/client"
	"longarm/internal/hubclient"
	"longarm/internal/signin"
	"longarm/internal/termws"
	"longarm/internal/version"
	"longarm/internal/webui"
	pb "longarm/proto/longarm/v1"
)

// Core is a running phone core.
type Core struct {
	dir   string // longarm's config dir in the app's storage
	token string
	ln    net.Listener
	srv   *http.Server
	log   *slog.Logger

	mu      sync.Mutex
	dialer  *client.Dialer
	cfg     *client.Config
	hub     *hubclient.Conn
	user    string                  // the signed-in user (Whoami)
	conns   map[string]*client.Conn // per worker, reused
	nodes   []*pb.NodeInfo          // the user's workers, as last listed
	nodesAt time.Time
	signin  *signinState
}

// Start serves on a free 127.0.0.1 port; dir is where longarm's config
// (hub.toml, tls/) lives in the app's storage.
func Start(dir string) (*Core, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	rand.Read(b)
	c := &Core{dir: dir, token: hex.EncodeToString(b), conns: map[string]*client.Conn{}, log: slog.Default()}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	c.ln = ln
	c.srv = &http.Server{Handler: c.routes(), ReadHeaderTimeout: 10 * time.Second}
	go c.srv.Serve(ln)
	return c, nil
}

// URL is the app's page, token included.
func (c *Core) URL() string { return "http://" + c.ln.Addr().String() + "/m?t=" + c.token }

// Stop ends the server and the connections.
func (c *Core) Stop() {
	c.srv.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked()
}

// Reconnect drops the connections (the app came back to the front: iOS
// ended them while it was away); the next request dials again.
func (c *Core) Reconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked()
}

func (c *Core) dropLocked() {
	for _, cn := range c.conns {
		cn.Close()
	}
	c.conns = map[string]*client.Conn{}
	if c.hub != nil {
		c.hub.Close()
		c.hub = nil
	}
	if c.dialer != nil {
		c.dialer.Close()
		c.dialer = nil
	}
	c.user, c.nodes = "", nil
}

func (c *Core) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /mapi/status", c.status)
	api.HandleFunc("POST /mapi/signin", c.startSignIn)
	api.HandleFunc("GET /mapi/signin", c.signInState)
	api.HandleFunc("POST /mapi/signout", c.signOut)
	api.HandleFunc("GET /mapi/machines", c.machines)
	const v1 = "/local/longarm/v1/workers/{worker}/sessions"
	api.HandleFunc("GET "+v1, c.listSessions)
	api.HandleFunc("POST "+v1, c.startSession)
	api.HandleFunc("GET "+v1+"/{name}", c.getSession)
	api.HandleFunc("POST "+v1+"/{name}/stop", c.stopSession)
	api.HandleFunc("GET "+v1+"/{name}/attach", c.attach)
	authed := c.auth(api)
	files := webui.Handler("/mapi/", "/local/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/mapi/") || strings.HasPrefix(r.URL.Path, "/local/") {
			authed.ServeHTTP(w, r)
			return
		}
		// the page's URL (URL) carries the token once: a cookie from then
		// on, so the frontend's fetch and WebSockets need nothing of their own
		if t := r.URL.Query().Get("t"); t != "" && c.valid(t) {
			http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: c.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		}
		files.ServeHTTP(w, r)
	})
}

const tokenCookie = "longarm_m"

func (c *Core) valid(t string) bool { return subtle.ConstantTimeCompare([]byte(t), []byte(c.token)) == 1 }

// auth: the token Start gave, as the cookie the page got, X-Longarm-Token
// or ?t=; other apps on the phone can reach 127.0.0.1 too.
func (c *Core) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := r.Header.Get("X-Longarm-Token")
		if t == "" {
			t = r.URL.Query().Get("t")
		}
		if ck, err := r.Cookie(tokenCookie); t == "" && err == nil {
			t = ck.Value
		}
		if !c.valid(t) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// session opens (once) the dialer and the hub connection with the device
// certificate in dir.
func (c *Core) session(ctx context.Context) (*hubclient.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hub != nil {
		return c.hub, nil
	}
	cfg, err := client.LoadConfig(c.dir)
	if err != nil {
		return nil, err
	}
	if !cfg.Hub.Enabled() {
		return nil, errNotSignedIn
	}
	d, err := client.NewDialer(cfg)
	if err != nil {
		return nil, errNotSignedIn
	}
	h, err := hubclient.Dial(cfg.Hub.Address, d.Bundle())
	if err != nil {
		d.Close()
		return nil, err
	}
	me, err := h.Fleet().Whoami(ctx, &pb.WhoamiRequest{})
	if err != nil {
		h.Close()
		d.Close()
		return nil, err
	}
	c.cfg, c.dialer, c.hub, c.user = cfg, d, h, me.GetUser()
	return h, nil
}

var errNotSignedIn = errors.New("not signed in")

func (c *Core) status(w http.ResponseWriter, r *http.Request) {
	st := map[string]any{"version": version.Version}
	hub, _ := signin.DefaultHub()
	if cfg, err := client.LoadConfig(c.dir); err == nil && cfg.Hub.Address != "" {
		hub = cfg.Hub.Address
	}
	st["hub"] = hub
	if _, err := c.session(r.Context()); err == nil {
		c.mu.Lock()
		st["signedIn"], st["user"] = true, c.user
		c.mu.Unlock()
	} else {
		st["signedIn"] = false
		if !errors.Is(err, errNotSignedIn) {
			st["error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// owned is the user's own workers (the hub's list, filtered); fresh
// within maxAge.
func (c *Core) owned(ctx context.Context, maxAge time.Duration) ([]*pb.NodeInfo, error) {
	c.mu.Lock()
	if c.nodes != nil && time.Since(c.nodesAt) < maxAge {
		n := c.nodes
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	h, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.Fleet().ListNodes(ctx, &pb.ListNodesRequest{NodeIds: true})
	if err != nil {
		if status.Code(err) == codes.Unavailable {
			c.Reconnect() // the hub connection broke: a new one next time
		}
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*pb.NodeInfo
	for _, n := range r.GetNodes() {
		if n.GetRole() == "worker" && n.GetOwner() == c.user && n.GetState() == "approved" {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	c.nodes, c.nodesAt = out, time.Now()
	return out, nil
}

var mo = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}

// machines is GET /mapi/machines: the user's workers as the hub knows them
// (NodeInfo with its metrics; the frontend's PoolWorker shape).
func (c *Core) machines(w http.ResponseWriter, r *http.Request) {
	nodes, err := c.owned(r.Context(), 0)
	if err != nil {
		httpError(w, err)
		return
	}
	writeProto(w, http.StatusOK, &pb.ListNodesResponse{Nodes: nodes})
}

// conn dials one of the user's own workers (once; reused).
func (c *Core) conn(ctx context.Context, name string) (*client.Conn, error) {
	c.mu.Lock()
	if cn := c.conns[name]; cn != nil {
		c.mu.Unlock()
		return cn, nil
	}
	c.mu.Unlock()
	nodes, err := c.owned(ctx, time.Minute)
	if err != nil {
		return nil, err
	}
	var node *pb.NodeInfo
	for _, n := range nodes {
		if n.GetName() == name {
			node = n
		}
	}
	if node == nil {
		return nil, status.Error(codes.NotFound, "not one of your machines: "+name)
	}
	wk, ok := client.WorkerFromNode(node)
	if !ok {
		return nil, status.Error(codes.Unavailable, name+" has no endpoint this phone can reach")
	}
	wk.TrustName = name
	c.mu.Lock()
	d := c.dialer
	c.mu.Unlock()
	if d == nil {
		return nil, errNotSignedIn
	}
	cn, err := d.Dial(wk)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.conns[name]; old != nil { // dialed twice at once
		cn.Close()
		return old, nil
	}
	c.conns[name] = cn
	return cn, nil
}

// forget drops a worker's connection when a call on it failed for the
// connection's sake (dial again next time).
func (c *Core) forget(name string, err error) {
	if s := status.Code(err); s != codes.Unavailable && s != codes.DeadlineExceeded && s != codes.Canceled {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cn := c.conns[name]; cn != nil {
		cn.Close()
		delete(c.conns, name)
	}
}

// sessionsCall runs f on the worker's Sessions service.
func (c *Core) sessionsCall(r *http.Request, f func(ctx context.Context, sc pb.SessionsClient) (proto.Message, error)) (proto.Message, error) {
	worker := r.PathValue("worker")
	cn, err := c.conn(r.Context(), worker)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	m, err := f(ctx, cn.Sessions())
	if err != nil {
		c.forget(worker, err)
	}
	return m, err
}

func (c *Core) listSessions(w http.ResponseWriter, r *http.Request) {
	m, err := c.sessionsCall(r, func(ctx context.Context, sc pb.SessionsClient) (proto.Message, error) {
		return sc.List(ctx, &pb.ListSessionsRequest{})
	})
	if err != nil {
		httpError(w, err)
		return
	}
	writeProto(w, http.StatusOK, m)
}

func (c *Core) getSession(w http.ResponseWriter, r *http.Request) {
	m, err := c.sessionsCall(r, func(ctx context.Context, sc pb.SessionsClient) (proto.Message, error) {
		return sc.Get(ctx, &pb.GetSessionRequest{Name: r.PathValue("name")})
	})
	if err != nil {
		httpError(w, err)
		return
	}
	writeProto(w, http.StatusOK, m)
}

var sessionNameUnsafe = regexp.MustCompile(`[^0-9A-Za-z_-]+`)

// startSession is POST .../sessions: {name?, argv?, agent?, cwd?, cols,
// rows} (the daemon's startAgentReq). No argv and no agent: the worker's
// shell. An agent runs with the worker's own sign-in (the phone forwards
// no credentials).
func (c *Core) startSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string   `json:"name"`
		Agent string   `json:"agent"`
		Argv  []string `json:"argv"`
		Cwd   string   `json:"cwd"`
		Cols  uint32   `json:"cols"`
		Rows  uint32   `json:"rows"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	argv := req.Argv
	switch req.Agent {
	case "":
	case "claude", "copilot":
		argv = []string{req.Agent}
	default:
		http.Error(w, "agent: claude or copilot", http.StatusBadRequest)
		return
	}
	name := req.Name
	if name == "" {
		p := "shell"
		if len(argv) > 0 {
			p = strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(argv[0], `\`, "/"))), ".exe")
		}
		if p = sessionNameUnsafe.ReplaceAllString(p, ""); p == "" || len(p) > 32 {
			p = "session"
		}
		b := make([]byte, 2)
		rand.Read(b)
		name = p + "-" + hex.EncodeToString(b)
	}
	sreq := &pb.StartSessionRequest{Name: name, Argv: argv, Cwd: req.Cwd}
	if req.Cols > 0 && req.Rows > 0 {
		sreq.Size = &pb.TermSize{Cols: req.Cols, Rows: req.Rows}
	}
	m, err := c.sessionsCall(r, func(ctx context.Context, sc pb.SessionsClient) (proto.Message, error) {
		return sc.Start(ctx, sreq)
	})
	if err != nil {
		httpError(w, err)
		return
	}
	st := m.(*pb.AttachStarted)
	code := http.StatusOK
	if st.GetCreated() {
		code = http.StatusCreated
	}
	writeProto(w, code, st.GetSession())
}

func (c *Core) stopSession(w http.ResponseWriter, r *http.Request) {
	m, err := c.sessionsCall(r, func(ctx context.Context, sc pb.SessionsClient) (proto.Message, error) {
		return sc.Stop(ctx, &pb.StopSessionRequest{Name: r.PathValue("name"), Grace: durationpb.New(10 * time.Second)})
	})
	if err != nil {
		httpError(w, err)
		return
	}
	writeProto(w, http.StatusOK, m)
}

func (c *Core) attach(w http.ResponseWriter, r *http.Request) {
	cn, err := c.conn(r.Context(), r.PathValue("worker"))
	if err != nil {
		httpError(w, err)
		return
	}
	termws.Serve(w, r, cn.Sessions(), termws.Open(r, r.PathValue("name"), false))
}

// signinState is the sign-in in progress (one at a time).
type signinState struct {
	Step    string `json:"step"`
	Message string `json:"message"`
	URL     string `json:"url,omitempty"`  // open it in the browser
	Code    string `json:"code,omitempty"` // and enter it there
	Done    bool   `json:"done"`
	Error   string `json:"error,omitempty"`
}

// startSignIn signs this phone in as a device of the user (browser
// pairing: a link and a code to approve with).
func (c *Core) startSignIn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hub  string `json:"hub"`
		Name string `json:"name"` // this phone's name at the hub
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
	if req.Hub == "" {
		req.Hub, _ = signin.DefaultHub()
	}
	if req.Hub == "" {
		http.Error(w, "which hub?", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	if c.signin != nil && !c.signin.Done {
		c.mu.Unlock()
		http.Error(w, "signing in already", http.StatusConflict)
		return
	}
	st := &signinState{Step: "starting"}
	c.signin = st
	c.dropLocked() // the new certificate replaces the old one
	c.mu.Unlock()
	set := func(f func(*signinState)) {
		c.mu.Lock()
		f(st)
		c.mu.Unlock()
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		_, err := signin.SignIn(ctx, signin.Options{
			Hub:        req.Hub,
			Client:     client.DeviceTarget{ConfigDir: c.dir, Force: true},
			DeviceName: req.Name,
			Method:     signin.MethodLink,
			Replace:    true,
		}, signin.Events{
			Progress: func(p signin.Progress) { set(func(s *signinState) { s.Step, s.Message = string(p.Step), p.Message }) },
			Code:     func(k signin.Code) { set(func(s *signinState) { s.URL, s.Code = k.URL, k.Code }) },
		})
		set(func(s *signinState) {
			s.Done = true
			if err != nil {
				s.Error = err.Error()
			}
		})
	}()
	writeJSON(w, http.StatusOK, st)
}

func (c *Core) signInState(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	var st signinState
	if c.signin != nil {
		st = *c.signin
	}
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, st)
}

// signOut forgets this phone's certificate and hub (the hub keeps the
// device until it is removed there: longarm admin nodes).
func (c *Core) signOut(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.dropLocked()
	c.mu.Unlock()
	for _, p := range []string{"tls", "hub.toml", "workers.toml"} {
		os.RemoveAll(filepath.Join(c.dir, p))
	}
	writeJSON(w, http.StatusOK, map[string]bool{"signedIn": false})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeProto(w http.ResponseWriter, code int, m proto.Message) {
	b, err := mo.Marshal(m)
	if err != nil {
		httpError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

// httpError answers with the gRPC status's message and a matching HTTP
// code (the frontend shows the message).
func httpError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	switch {
	case errors.Is(err, errNotSignedIn):
		code = http.StatusUnauthorized
	case status.Code(err) == codes.NotFound:
		code = http.StatusNotFound
	case status.Code(err) == codes.PermissionDenied:
		code = http.StatusForbidden
	case status.Code(err) == codes.InvalidArgument || status.Code(err) == codes.AlreadyExists:
		code = http.StatusBadRequest
	}
	writeJSON(w, code, map[string]string{"error": status.Convert(err).Message()})
}

// ConfigDir is the config dir under the app's files dir.
func ConfigDir(filesDir string) string { return filepath.Join(filesDir, "longarm") }
