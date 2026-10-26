package longarmd

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/portlink"
	"github.com/lubobill1990/longarm/internal/server"
	"github.com/lubobill1990/longarm/internal/supervisor"
	"github.com/lubobill1990/longarm/internal/userspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Port links on a worker (docs/usage.md "Port links"). The hub pushes the
// list of links naming this worker on the hub session; it is kept in
// port-links.json so links keep working while the hub is away.
//
//   - As "from" (A): listen on 127.0.0.1:<listen port>; every accepted
//     connection - with per-user isolation only from a process of the
//     owner's account - becomes a LinkTunnel.Dial stream to the target
//     worker, presenting the hub-signed grant. The target is reached like a
//     client reaches workers (transport race over its advertised endpoints),
//     with this worker's own longnet / tailscale nodes where it has them.
//   - As "to" (B): server.LinkTunnel checks the grant and asks Incoming
//     whether the link is (still) in the list.
//
// A link that leaves the list (removed, expired) is cut on both sides:
// listener closed, open connections closed. Expiry and the end of the job a
// link is bound to are also enforced here, without the hub: a link may only
// be bound to a job on its "from" worker, so A ends it when the job ends (the
// target cannot see A's jobs; with A no longer dialing, it has nothing to
// accept).

// PortLinksFile holds the hub's last list of port links naming this worker.
const PortLinksFile = "port-links.json"

// linkDialDir holds the link dialer's transport cache.
const linkDialDir = "links"

// Retry timing (variables for tests).
var (
	linkListenRetry   = 10 * time.Second // listen port busy
	linkInactiveRetry = 4 * time.Second  // the target has not got the hub's push yet
	linkReportEvery   = 2 * time.Second
)

type linkManager struct {
	dataDir string
	name    string // this worker
	bundle  *pki.Bundle
	spaces  *userspace.Spaces
	audit   *server.Audit
	log     *slog.Logger
	dialer  *client.Dialer
	// report sends link states to the hub (the agent's session; dropped
	// while offline).
	report func(*pb.PortLinkReport)
	// jobEnded says whether a job on this worker has ended (or is gone):
	// links bound to it end.
	jobEnded func(id string) bool
	// maxConns caps each link's open connections, on either side ([links]
	// max_conns; 0 = no cap).
	maxConns int

	mu      sync.Mutex
	links   map[string]*linkState  // every link naming this worker
	targets map[string]*linkTarget // gRPC connections to target workers
	lastRep string                 // last report sent
	ipv6    bool                   // [::1] works here: listeners bind it too
	ctx     context.Context
}

// linkState is one link; for "from" links also its listener and counters.
type linkState struct {
	link   *pb.PortLink           // fixed for the link's lifetime (sameLink)
	a      *pb.PortLinkAssignment // latest grant and target endpoints (m.mu)
	ctx    context.Context        // ends when the link goes
	cancel context.CancelFunc

	mu       sync.Mutex
	cctx     context.Context // the open connections': ends with ctx or cut
	ccancel  context.CancelFunc
	state    string // listening | error
	bound    bool   // the listener is up
	err      string
	path     string
	conns    int64
	refused  int64
	active   atomic.Int32
	sent     atomic.Int64
	received atomic.Int64
}

// linkTarget is a gRPC connection to a target worker, shared by the
// streams of every link to it. When the target's endpoints change, new
// streams get a new connection; the old one is closed once its last stream
// ended (an endpoint change - a tailnet leg coming up - must not cut open
// connections that still work).
type linkTarget struct {
	key     string // endpoints the connection was made for
	conn    *client.Conn
	refs    int  // streams using it (m.mu)
	retired bool // replaced or unused: close at refs == 0
}

func newLinkManager(ctx context.Context, dataDir, name string, bundle *pki.Bundle, spaces *userspace.Spaces, audit *server.Audit,
	log *slog.Logger, dialer *client.Dialer) *linkManager {
	m := &linkManager{ctx: ctx, dataDir: dataDir, name: name, bundle: bundle, spaces: spaces, audit: audit, log: log,
		dialer: dialer, links: map[string]*linkState{}, targets: map[string]*linkTarget{}, maxConns: DefaultLinkMaxConns}
	m.jobEnded = func(id string) bool {
		if spaces == nil {
			return false
		}
		_, j, err := spaces.FindJob(id)
		return errors.Is(err, supervisor.ErrNotFound) || (err == nil && j.Status.State.Terminal())
	}
	if ln, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		m.ipv6 = true
		ln.Close()
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, PortLinksFile)); err == nil {
		pl := &pb.PortLinks{}
		if err := protojson.Unmarshal(b, pl); err != nil {
			log.Warn("port links: unreadable "+PortLinksFile+"; waiting for the hub", "err", err)
		} else {
			m.apply(pl, false)
		}
	}
	return m
}

// Roots implement server.LinkRegistry.
func (m *linkManager) Roots() *x509.CertPool { return m.bundle.Pool }

// Incoming implements server.LinkRegistry.
func (m *linkManager) Incoming(id string) (*pb.PortLink, context.Context, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ls := m.links[id]
	if ls == nil || ls.link.To != m.name || expired(ls.link, time.Now()) {
		return nil, nil, nil, server.ErrLinkInactive
	}
	if err := m.take(ls); err != nil {
		return nil, nil, nil, err
	}
	return ls.link, ls.connCtx(), func() { ls.active.Add(-1) }, nil
}

// take counts one more open connection of ls (undo: ls.active.Add(-1)),
// unless it has maxConns.
func (m *linkManager) take(ls *linkState) error {
	if n := ls.active.Add(1); m.maxConns > 0 && int(n) > m.maxConns {
		ls.active.Add(-1)
		return fmt.Errorf("link %s has %d open connections (max %d, [links] max_conns on %s)", ls.link.Id, n-1, m.maxConns, m.name)
	}
	return nil
}

// connCtx is the context of ls's open connections.
func (ls *linkState) connCtx() context.Context {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.cctx
}

// cutConns closes the open connections of the links of the owners cond
// selects (disabled users); the links stay. It returns how many links had
// connections.
func (m *linkManager) cutConns(cond func(owner string) bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, ls := range m.links {
		if !cond(ls.link.Owner) {
			continue
		}
		if ls.active.Load() > 0 {
			n++
		}
		ls.mu.Lock()
		ls.ccancel()
		ls.cctx, ls.ccancel = context.WithCancel(ls.ctx)
		ls.mu.Unlock()
	}
	return n
}

// Env implements server.LinkRegistry: the owner's links from here.
func (m *linkManager) Env(owner string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ls []portlink.Link
	now := time.Now()
	for _, s := range m.links {
		l := s.link
		if l.From != m.name || l.Owner != owner || expired(l, now) {
			continue
		}
		// Only links whose listener is up: while another process holds the
		// port, the variable would point jobs at that process.
		s.mu.Lock()
		up := s.bound
		s.mu.Unlock()
		if up {
			ls = append(ls, portlink.Link{To: l.To, Port: l.Port, ListenPort: l.ListenPort})
		}
	}
	return portlink.Env(ls)
}

func expired(l *pb.PortLink, now time.Time) bool {
	return l.ExpiresAt != nil && !now.Before(l.ExpiresAt.AsTime())
}

// jobGone: l is bound to a job here that has ended (the hub ends such links
// too, but it may be away).
func (m *linkManager) jobGone(l *pb.PortLink) bool {
	return l.From == m.name && l.JobId != "" && m.jobEnded(l.JobId)
}

// apply makes pl (the hub's list) the current set of links; persist saves
// it for restarts.
func (m *linkManager) apply(pl *pb.PortLinks, persist bool) {
	if persist {
		if b, err := protojson.Marshal(pl); err == nil {
			if err := writeFileAtomic(filepath.Join(m.dataDir, PortLinksFile), b); err != nil {
				m.log.Warn("port links: save", "err", err)
			}
		}
	}
	now := time.Now()
	m.mu.Lock()
	keep := map[string]bool{}
	for _, a := range pl.Links {
		l := a.GetLink()
		if l == nil || !portlink.ValidID(l.Id) || (l.From != m.name && l.To != m.name) || expired(l, now) || m.jobGone(l) {
			continue
		}
		keep[l.Id] = true
		if old := m.links[l.Id]; old != nil {
			if sameLink(old.link, l) {
				old.a = a // a new grant / target endpoints: used by new connections
				continue
			}
			m.removeLocked(old, "changed")
		}
		ctx, cancel := context.WithCancel(m.ctx)
		ls := &linkState{link: l, a: a, ctx: ctx, cancel: cancel}
		ls.cctx, ls.ccancel = context.WithCancel(ctx)
		m.links[l.Id] = ls
		if l.From == m.name {
			m.log.Info("port link: listening for it", "link", l.Id, "owner", l.Owner, "listen", l.ListenPort, "to", fmt.Sprintf("%s:%d", l.To, l.Port))
			go m.listen(ls)
		} else {
			m.log.Info("port link: accepting it", "link", l.Id, "owner", l.Owner, "from", l.From, "port", l.Port)
		}
	}
	for id, ls := range m.links {
		if !keep[id] {
			m.removeLocked(ls, "removed")
		}
	}
	m.pruneTargetsLocked()
	m.mu.Unlock()
	m.sendReport(true)
}

// sameLink: the fields a listener or an accepted stream depends on.
func sameLink(a, b *pb.PortLink) bool {
	return a.Owner == b.Owner && a.From == b.From && a.To == b.To && a.Port == b.Port && a.ListenPort == b.ListenPort &&
		a.ExpiresAt.AsTime().Equal(b.ExpiresAt.AsTime())
}

// removeLocked ends a link: listener and connections (both sides).
func (m *linkManager) removeLocked(ls *linkState, why string) {
	delete(m.links, ls.link.Id)
	ls.cancel()
	m.log.Info("port link ended", "link", ls.link.Id, "reason", why)
}

// pruneTargetsLocked closes connections to workers no link targets any more.
func (m *linkManager) pruneTargetsLocked() {
	used := map[string]bool{}
	for _, ls := range m.links {
		if ls.link.From == m.name {
			used[ls.link.To] = true
		}
	}
	for to, t := range m.targets {
		if !used[to] {
			m.retireLocked(to, t)
		}
	}
}

// retireLocked takes t out of use; it closes when its last stream ends.
func (m *linkManager) retireLocked(to string, t *linkTarget) {
	if m.targets[to] == t {
		delete(m.targets, to)
	}
	t.retired = true
	if t.refs == 0 {
		t.conn.Close()
	}
}

// release ends one stream's use of t.
func (m *linkManager) release(t *linkTarget) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t.refs--
	if t.retired && t.refs == 0 {
		t.conn.Close()
	}
}

// run expires links and ends those of ended jobs locally (also without the
// hub) and reports the states of the links listened for.
func (m *linkManager) run(ctx context.Context) {
	t := time.NewTicker(linkReportEvery)
	defer t.Stop()
	for {
		var jobsChanged <-chan struct{}
		if m.spaces != nil {
			jobsChanged = m.spaces.Changed()
		}
		select {
		case <-jobsChanged:
		case <-ctx.Done():
			m.mu.Lock()
			for _, ls := range m.links {
				ls.cancel()
			}
			for to, t := range m.targets {
				m.retireLocked(to, t) // their streams end with the links
			}
			m.mu.Unlock()
			return
		case <-t.C:
		}
		now := time.Now()
		m.mu.Lock()
		for _, ls := range m.links {
			if expired(ls.link, now) {
				m.removeLocked(ls, "expired")
			} else if m.jobGone(ls.link) {
				m.removeLocked(ls, "job ended")
			}
		}
		m.pruneTargetsLocked()
		m.mu.Unlock()
		m.sendReport(false)
	}
}

// statusReport is the state of the links this worker listens for.
func (m *linkManager) statusReport() *pb.PortLinkReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	rep := &pb.PortLinkReport{}
	for _, ls := range m.links {
		if ls.link.From != m.name {
			continue
		}
		ls.mu.Lock()
		s := &pb.PortLinkStatus{Id: ls.link.Id, State: ls.state, Error: ls.err, Connections: ls.conns, Refused: ls.refused,
			Active: ls.active.Load(), BytesSent: ls.sent.Load(), BytesReceived: ls.received.Load(), Path: ls.path}
		ls.mu.Unlock()
		if s.State == "" {
			s.State = "pending"
		}
		rep.Links = append(rep.Links, s)
	}
	sort.Slice(rep.Links, func(i, j int) bool { return rep.Links[i].Id < rep.Links[j].Id })
	return rep
}

// sendReport sends the states when they changed (force: always).
func (m *linkManager) sendReport(force bool) {
	if m.report == nil {
		return
	}
	rep := m.statusReport()
	b, _ := proto.Marshal(rep)
	m.mu.Lock()
	same := string(b) == m.lastRep
	m.lastRep = string(b)
	m.mu.Unlock()
	if same && !force {
		return
	}
	m.report(rep)
}

func (ls *linkState) set(state, err string) {
	ls.mu.Lock()
	ls.state, ls.err = state, err
	ls.mu.Unlock()
}

// listen serves a "from" link until it ends.
func (m *linkManager) listen(ls *linkState) {
	l := ls.link
	port := strconv.Itoa(int(l.ListenPort))
	addrs := []string{net.JoinHostPort("127.0.0.1", port)}
	if m.ipv6 {
		// "localhost" may resolve to ::1 first: a port free there would let
		// another local process receive what the owner sends to localhost.
		addrs = append(addrs, net.JoinHostPort("::1", port))
	}
	for ls.ctx.Err() == nil {
		var lns []net.Listener
		var err error
		for _, a := range addrs {
			var ln net.Listener
			if ln, err = net.Listen("tcp", a); err != nil {
				err = fmt.Errorf("listen %s: %w (another process holds the port?)", a, err)
				break
			}
			lns = append(lns, ln)
		}
		if err != nil {
			for _, ln := range lns {
				ln.Close()
			}
			ls.set("error", err.Error())
			m.log.Warn("port link: listen", "link", l.Id, "err", err)
			select {
			case <-ls.ctx.Done():
			case <-time.After(linkListenRetry):
			}
			continue
		}
		ls.mu.Lock()
		ls.state, ls.err, ls.bound = "listening", "", true
		ls.mu.Unlock()
		stop := context.AfterFunc(ls.ctx, func() {
			for _, ln := range lns {
				ln.Close()
			}
		})
		var wg sync.WaitGroup
		for _, ln := range lns {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					c, err := ln.Accept()
					if err != nil {
						for _, ln := range lns { // one failed: listen anew on all
							ln.Close()
						}
						return
					}
					go m.serveConn(ls, c)
				}
			}()
		}
		wg.Wait()
		stop()
		ls.mu.Lock()
		ls.bound = false
		ls.mu.Unlock()
		if ls.ctx.Err() == nil {
			time.Sleep(time.Second) // accept failed: listen again
		}
	}
}

// serveConn carries one local connection to the target worker.
func (m *linkManager) serveConn(ls *linkState, c net.Conn) {
	defer c.Close()
	l := ls.link
	err := m.checkPeer(l, c)
	if err == nil {
		err = m.take(ls)
	}
	if err != nil {
		ls.mu.Lock()
		ls.refused++
		ls.mu.Unlock()
		m.audit.LogAs("longarmd", "link/refused", map[string]any{"link": l.Id, "owner": l.Owner, "peer": c.RemoteAddr().String(), "why": err.Error()})
		m.log.Info("port link: refused a local connection", "link", l.Id, "peer", c.RemoteAddr().String(), "why", err)
		return
	}
	defer ls.active.Add(-1)
	ls.mu.Lock()
	ls.conns++
	ls.mu.Unlock()
	ctx, cancel := context.WithCancel(ls.connCtx())
	defer cancel()
	stop := context.AfterFunc(ctx, func() { c.Close() }) // link removed or connections cut
	defer stop()
	st, t, err := m.open(ctx, ls)
	if err != nil {
		ls.set("error", err.Error())
		m.log.Warn("port link: dial target", "link", l.Id, "to", l.To, "err", err)
		return
	}
	defer m.release(t)
	ls.mu.Lock()
	ls.state, ls.err, ls.path = "listening", "", t.conn.Path().String()
	ls.mu.Unlock()
	go func() { // local -> target
		buf := make([]byte, 32<<10)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				if st.Send(&pb.ForwardFrame{Data: buf[:n]}) != nil {
					cancel()
					return
				}
				ls.sent.Add(int64(n))
			}
			if err != nil {
				st.CloseSend()
				return
			}
		}
	}()
	for { // target -> local
		f, err := st.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil && status.Code(err) != codes.Canceled {
				m.log.Debug("port link: stream", "link", l.Id, "err", err)
			}
			return
		}
		if len(f.Data) > 0 {
			if _, err := c.Write(f.Data); err != nil {
				return
			}
			ls.received.Add(int64(len(f.Data)))
		}
		if f.Eof {
			if tc, ok := c.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
		}
	}
}

// checkPeer: with per-user isolation only processes of the link owner's
// account may use the link (the listener belongs to longarmd).
func (m *linkManager) checkPeer(l *pb.PortLink, c net.Conn) error {
	if m.spaces == nil || !m.spaces.PerUser() {
		return nil
	}
	sp := m.spaces.Lookup(l.Owner)
	if sp == nil || sp.Account == nil {
		return fmt.Errorf("%s has no account on this worker", l.Owner)
	}
	ok, err := platform.PeerIsAccount(c, sp.Account)
	if err != nil {
		return fmt.Errorf("peer process: %w", err)
	}
	if !ok {
		return fmt.Errorf("not a process of %s", sp.Account.Name)
	}
	return nil
}

// open starts a LinkTunnel.Dial stream to the target and waits for its
// answer. A target that does not know the link yet (the hub's push to it
// is still on its way) is asked again for a few seconds.
func (m *linkManager) open(ctx context.Context, ls *linkState) (pb.LinkTunnel_DialClient, *linkTarget, error) {
	deadline := time.Now().Add(linkInactiveRetry)
	wait := 100 * time.Millisecond
	for {
		m.mu.Lock()
		a := ls.a
		m.mu.Unlock()
		t, err := m.target(a)
		if err != nil {
			return nil, nil, err
		}
		st, err := pb.NewLinkTunnelClient(t.conn.ClientConn).Dial(ctx)
		if err == nil {
			if err = st.Send(&pb.ForwardFrame{Grant: a.Grant}); err == nil {
				_, err = st.Recv() // the target's answer: an empty frame, or why not
			}
		}
		if err == nil {
			return st, t, nil // the caller releases t
		}
		m.release(t)
		if status.Code(err) != codes.FailedPrecondition || time.Now().After(deadline) || ctx.Err() != nil {
			return nil, nil, err
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Second)
	}
}

// target returns the connection to a's target worker over its current
// endpoints, counting one more stream on it (release it).
func (m *linkManager) target(a *pb.PortLinkAssignment) (*linkTarget, error) {
	to := a.Link.To
	key := strings.Join(a.TargetEndpoints, " ")
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.targets[to]; t != nil {
		if t.key == key {
			t.refs++
			return t, nil
		}
		m.retireLocked(to, t) // the target's endpoints changed: new streams get a new connection
	}
	w := client.Worker{Name: to, CertName: to}
	for _, s := range a.TargetEndpoints {
		e, err := client.ParseEndpoint(s)
		if err != nil {
			continue
		}
		if w.Address == "" {
			w.Transport, w.Address = e.Transport, e.Address
		} else {
			w.Alt = append(w.Alt, e)
		}
	}
	if w.Address == "" {
		return nil, fmt.Errorf("worker %s has no endpoints (has it ever been online?)", to)
	}
	conn, err := m.dialer.Dial(w)
	if err != nil {
		return nil, err
	}
	t := &linkTarget{key: key, conn: conn, refs: 1}
	m.targets[to] = t
	return t, nil
}

// writeFileAtomic writes b to path via a temporary file.
func writeFileAtomic(path string, b []byte) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := platform.ReplaceFile(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
