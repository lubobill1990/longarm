// Package mobile is the Longarm phone app's core (docs/mobile.md): what
// `longarm` and the tray do on a computer, in the app's own process (a phone
// runs no daemons). It serves the app's web UI a local HTTP API on
// 127.0.0.1 - sign-in, the owner's machines, their sessions, terminal
// attach - and holds the connections to the hub and the workers. The app
// reaches it with the token Start gives (a header, or ?t= for WebSockets).
//
// Only the signed-in user's own machines are listed: the workers whose owner
// they are.
package mobile

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"longarm/internal/client"
	"longarm/internal/hubclient"
	"longarm/internal/signin"
	"longarm/internal/termws"
	"longarm/internal/version"
	pb "longarm/proto/longarm/v1"
)

//go:embed page
var page embed.FS

// Core is a running phone core.
type Core struct {
	dir   string // longarm's config dir in the app's storage
	token string
	ln    net.Listener
	srv   *http.Server
	log   *slog.Logger

	mu     sync.Mutex
	dialer *client.Dialer
	cfg    *client.Config
	hub    *hubclient.Conn
	user   string                  // the signed-in user (Whoami)
	conns  map[string]*client.Conn // per worker, reused
	signin *signinState
}

// Start serves the API on a free 127.0.0.1 port; dir is where longarm's
// config (hub.toml, tls/) lives in the app's storage.
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
func (c *Core) URL() string { return "http://" + c.ln.Addr().String() + "/?t=" + c.token }

// Stop ends the server and the connections.
func (c *Core) Stop() {
	c.srv.Close()
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
	c.user = ""
}

func (c *Core) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(page, "page")
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("GET /api/status", c.status)
	mux.HandleFunc("POST /api/signin", c.startSignIn)
	mux.HandleFunc("GET /api/signin", c.signInState)
	mux.HandleFunc("GET /api/machines", c.machines)
	mux.HandleFunc("GET /api/machines/{worker}/sessions", c.sessions)
	mux.HandleFunc("GET /api/machines/{worker}/sessions/{name}/attach", c.attach)
	return c.auth(mux)
}

// auth: the token Start gave, as X-Longarm-Token or ?t= (the page and
// WebSockets); other apps on the phone can reach 127.0.0.1 too.
func (c *Core) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := r.Header.Get("X-Longarm-Token")
		if t == "" {
			t = r.URL.Query().Get("t")
		}
		if subtle.ConstantTimeCompare([]byte(t), []byte(c.token)) != 1 {
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

// Machine is a row of the machine list.
type Machine struct {
	Name     string `json:"name"`
	OS       string `json:"os"`
	Online   bool   `json:"online"`
	Version  string `json:"version"`
	Admin    bool   `json:"admin"` // an admin worker (runs as SYSTEM / root)
	LastSeen string `json:"lastSeen,omitempty"`
}

func (c *Core) status(w http.ResponseWriter, r *http.Request) {
	st := map[string]any{"version": version.Version}
	if _, err := c.session(r.Context()); err == nil {
		c.mu.Lock()
		st["signedIn"], st["user"], st["hub"] = true, c.user, c.cfg.Hub.Address
		c.mu.Unlock()
	} else {
		st["signedIn"] = false
		if !errors.Is(err, errNotSignedIn) {
			st["error"] = err.Error()
		}
	}
	writeJSON(w, st)
}

// owned is the user's own workers (the hub's list, filtered).
func (c *Core) owned(ctx context.Context) ([]*pb.NodeInfo, error) {
	h, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.Fleet().ListNodes(ctx, &pb.ListNodesRequest{NodeIds: true})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	me := c.user
	c.mu.Unlock()
	var out []*pb.NodeInfo
	for _, n := range r.GetNodes() {
		if n.GetRole() == "worker" && n.GetOwner() == me && n.GetState() == "approved" {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, nil
}

func (c *Core) machines(w http.ResponseWriter, r *http.Request) {
	nodes, err := c.owned(r.Context())
	if err != nil {
		httpError(w, err)
		return
	}
	out := []Machine{}
	for _, n := range nodes {
		m := Machine{Name: n.GetName(), OS: n.GetOs(), Online: n.GetOnline(), Version: n.GetVersion(), Admin: n.GetLabels()["longarm.io/admin"] == "true"}
		if t := n.GetLastSeen(); t != nil && !n.GetOnline() {
			m.LastSeen = t.AsTime().Format(time.RFC3339)
		}
		out = append(out, m)
	}
	writeJSON(w, out)
}

// conn dials one of the user's own workers (once; reused).
func (c *Core) conn(ctx context.Context, name string) (*client.Conn, error) {
	nodes, err := c.owned(ctx)
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
		return nil, errors.New("not one of your machines: " + name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cn := c.conns[name]; cn != nil {
		return cn, nil
	}
	wk, ok := client.WorkerFromNode(node)
	if !ok {
		return nil, errors.New(name + " has no endpoint this phone can reach")
	}
	wk.TrustName = name
	cn, err := c.dialer.Dial(wk)
	if err != nil {
		return nil, err
	}
	c.conns[name] = cn
	return cn, nil
}

var mo = protojson.MarshalOptions{UseProtoNames: true}

func (c *Core) sessions(w http.ResponseWriter, r *http.Request) {
	cn, err := c.conn(r.Context(), r.PathValue("worker"))
	if err != nil {
		httpError(w, err)
		return
	}
	resp, err := cn.Sessions().List(r.Context(), &pb.ListSessionsRequest{})
	if err != nil {
		c.forget(r.PathValue("worker"))
		httpError(w, err)
		return
	}
	b, _ := mo.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (c *Core) attach(w http.ResponseWriter, r *http.Request) {
	cn, err := c.conn(r.Context(), r.PathValue("worker"))
	if err != nil {
		httpError(w, err)
		return
	}
	termws.Serve(w, r, cn.Sessions(), termws.Open(r, r.PathValue("name"), false))
}

// forget drops a worker's connection (it failed: dial again next time).
func (c *Core) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cn := c.conns[name]; cn != nil {
		cn.Close()
		delete(c.conns, name)
	}
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
	json.NewDecoder(r.Body).Decode(&req)
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
	writeJSON(w, st)
}

func (c *Core) signInState(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	var st signinState
	if c.signin != nil {
		st = *c.signin
	}
	c.mu.Unlock()
	writeJSON(w, st)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	if errors.Is(err, errNotSignedIn) {
		code = http.StatusUnauthorized
	}
	http.Error(w, err.Error(), code)
}

// ConfigDir is the config dir under the app's files dir.
func ConfigDir(filesDir string) string { return filepath.Join(filesDir, "longarm") }
