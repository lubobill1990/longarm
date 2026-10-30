package longarmd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/portlink"
	"github.com/lubobill1990/longarm/internal/server"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// linkPair is worker "wa" (listens for links) and worker "wb" (their
// target, serving LinkTunnel) - and no hub: the lists are applied directly.
type linkPair struct {
	a, b     *linkManager
	hub      *pki.Bundle // signs grants
	endpoint string      // wb's
	echo     int         // a port on wb answering "hello " + what it reads
	ca       *pki.CA
}

func newLinkPair(t *testing.T) *linkPair {
	t.Helper()
	old := linkReportEvery
	linkReportEvery = 50 * time.Millisecond
	t.Cleanup(func() { linkReportEvery = old })
	root := t.TempDir()
	ca, err := pki.InitCA(filepath.Join(root, "ca"), "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bundle := func(o pki.IssueOptions) *pki.Bundle {
		certPEM, keyPEM, err := ca.Issue(o)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, o.Name)
		if err := pki.WriteBundle(dir, ca.CertPEM, certPEM, keyPEM); err != nil {
			t.Fatal(err)
		}
		b, err := pki.LoadBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	p := &linkPair{hub: bundle(pki.IssueOptions{Role: pki.RoleHub, Name: "hub"}), ca: ca}
	wa := bundle(pki.IssueOptions{Role: pki.RoleWorker, Name: "wa"})
	wb := bundle(pki.IssueOptions{Role: pki.RoleWorker, Name: "wb", Hosts: []string{"127.0.0.1"}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	dialer := client.NewDialerFor(&client.Config{Dir: filepath.Join(root, "dial")}, wa)
	t.Cleanup(func() { dialer.Close() })
	p.a = newLinkManager(ctx, filepath.Join(root, "a"), "wa", wa, nil, server.NewAudit(filepath.Join(root, "a-audit.log")), log, dialer)
	p.b = newLinkManager(ctx, filepath.Join(root, "b"), "wb", wb, nil, server.NewAudit(filepath.Join(root, "b-audit.log")), log, nil)
	go p.a.run(ctx)
	go p.b.run(ctx)

	revoked, _ := pki.LoadRevocationList("")
	srv := &server.Server{Name: "wb", DataDir: filepath.Join(root, "b"), StartedAt: time.Now(), Links: p.b,
		Audit: server.NewAudit(filepath.Join(root, "b-audit.log")), Revoked: revoked, Log: log}
	tc, err := wb.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	g := srv.GRPC(tc)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go g.Serve(ln)
	t.Cleanup(g.Stop)
	p.endpoint = "tcp://" + ln.Addr().String()

	eln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eln.Close() })
	go func() {
		for {
			c, err := eln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.Write([]byte("hello "))
				io.Copy(c, c)
			}()
		}
	}()
	p.echo = eln.Addr().(*net.TCPAddr).Port
	return p
}

// link is alice's link wa -> wb:echo on a free port, expiring at expires,
// with a grant valid until grantExpires.
func (p *linkPair) link(t *testing.T, jobID string, expires, grantExpires time.Time) *pb.PortLinks {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	l := &pb.PortLink{Id: portlink.NewID(), Owner: "alice", From: "wa", To: "wb", Port: int32(p.echo), ListenPort: int32(listen),
		CreatedAt: timestamppb.Now(), ExpiresAt: timestamppb.New(expires), JobId: jobID}
	g, err := portlink.Sign(p.hub.Certificate(), &pb.PortLinkGrant{Id: l.Id, Owner: l.Owner, From: l.From, To: l.To, Port: l.Port,
		ListenPort: l.ListenPort, ExpiresUnix: grantExpires.Unix(), IssuedUnix: time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return &pb.PortLinks{Version: 1, Links: []*pb.PortLinkAssignment{{Link: l, Grant: g, TargetEndpoints: []string{p.endpoint}}}}
}

// connect opens a connection through the link and checks that data flows.
func connect(t *testing.T, l *pb.PortLink) net.Conn {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(l.ListenPort)))
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.SetDeadline(time.Now().Add(10 * time.Second))
			c.Write([]byte("world"))
			var got []byte
			buf := make([]byte, 64)
			for !strings.Contains(string(got), "hello world") {
				n, err := c.Read(buf)
				if err != nil {
					t.Fatalf("through the link: %v (got %q)", err, got)
				}
				got = append(got, buf[:n]...)
			}
			c.SetDeadline(time.Time{})
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("link not listening: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitCut fails unless c is closed within d.
func waitCut(t *testing.T, c net.Conn, d time.Duration) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 64)
	for {
		if _, err := c.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("the link's open connection was not cut")
			}
			return
		}
	}
}

// listening reports whether the link's port on wa accepts connections.
func listening(l *pb.PortLink) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(l.ListenPort))), time.Second)
	if err == nil {
		c.Close()
	}
	return err == nil
}

// TestLinkExpiryWithoutHub: a link expires on both workers by itself - open
// connections are cut, the listener closes, the link variables go and the
// target refuses new streams, even with a grant that is still valid.
func TestLinkExpiryWithoutHub(t *testing.T) {
	p := newLinkPair(t)
	expires := time.Now().Add(3 * time.Second)
	pl := p.link(t, "", expires, time.Now().Add(time.Hour))
	l := pl.Links[0].Link
	p.a.apply(pl, false)
	p.b.apply(pl, false)
	c := connect(t, l)
	defer c.Close()
	if env := p.a.Env("alice"); env[portlink.EnvLinks] == "" {
		t.Fatalf("env before expiry: %v", env)
	}

	waitCut(t, c, time.Until(expires)+5*time.Second)
	if time.Now().Before(expires) {
		t.Fatal("cut before the link expired")
	}
	if _, _, _, err := p.b.Incoming(l.Id); !errors.Is(err, server.ErrLinkInactive) {
		t.Fatalf("wb still accepts the expired link: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for listening(l) {
		if time.Now().After(deadline) {
			t.Fatal("wa still listens for the expired link")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if env := p.a.Env("alice"); len(env) != 0 {
		t.Fatalf("env after expiry: %v", env)
	}

	// wa (or anybody with the grant) dials wb directly: refused
	tg, err := p.a.target(pl.Links[0])
	if err != nil {
		t.Fatal(err)
	}
	defer p.a.release(tg)
	st, err := pb.NewLinkTunnelClient(tg.conn.ClientConn).Dial(t.Context())
	if err == nil {
		if err = st.Send(&pb.ForwardFrame{Grant: pl.Links[0].Grant}); err == nil {
			_, err = st.Recv()
		}
	}
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("dial after expiry: %v", err)
	}
}

// TestLinkJobEndWithoutHub: a link bound to a job on wa ends there when the
// job ends, without the hub: connections cut, listener closed, variables
// gone; the (stale) list does not bring it back.
func TestLinkJobEndWithoutHub(t *testing.T) {
	p := newLinkPair(t)
	var ended atomic.Bool
	p.a.jobEnded = func(id string) bool { return id == "J1" && ended.Load() }
	pl := p.link(t, "J1", time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	l := pl.Links[0].Link
	p.a.apply(pl, false)
	p.b.apply(pl, false)
	c := connect(t, l)
	defer c.Close()

	ended.Store(true)
	waitCut(t, c, 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for listening(l) {
		if time.Now().After(deadline) {
			t.Fatal("wa still listens for the job's link")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if env := p.a.Env("alice"); len(env) != 0 {
		t.Fatalf("env after the job ended: %v", env)
	}
	p.a.apply(pl, false) // e.g. port-links.json at a restart, or the hub's list before it noticed
	p.a.mu.Lock()
	_, back := p.a.links[l.Id]
	p.a.mu.Unlock()
	if back {
		t.Fatal("the ended job's link came back")
	}
}

// TestLinkConnCap: [links] max_conns caps a link's open connections on
// the source (further local connections are refused) and on the target
// (further streams are refused).
func TestLinkConnCap(t *testing.T) {
	p := newLinkPair(t)
	p.a.maxConns = 2
	pl := p.link(t, "", time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	l := pl.Links[0].Link
	p.a.apply(pl, false)
	p.b.apply(pl, false)
	c1, c2 := connect(t, l), connect(t, l)
	defer c1.Close()
	defer c2.Close()
	c3, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(l.ListenPort))))
	if err != nil {
		t.Fatal(err)
	}
	waitCut(t, c3, 5*time.Second)
	if rep := p.a.statusReport(); len(rep.Links) != 1 || rep.Links[0].Refused != 1 || rep.Links[0].Active != 2 {
		t.Fatalf("status %v", rep)
	}
	c2.Close()
	deadline := time.Now().Add(5 * time.Second)
	for p.a.statusReport().Links[0].Active != 1 {
		if time.Now().After(deadline) {
			t.Fatal("closed connection still counted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	connect(t, l).Close() // a slot is free again

	// the target: wb's slots are those of its streams
	p.b.maxConns = 1
	_, _, release, err := p.b.Incoming(l.Id) // c1's stream holds one already
	if err == nil || errors.Is(err, server.ErrLinkInactive) || !strings.Contains(err.Error(), "max 1") {
		t.Fatalf("over the target's cap: %v", err)
	}
	c1.Close()
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, _, release, err = p.b.Incoming(l.Id)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("target slot not freed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	release()
}

// TestLinkCutConns: disabling the owner cuts the link's open connections
// on both workers; the link itself stays.
func TestLinkCutConns(t *testing.T) {
	for _, side := range []string{"source", "target"} {
		t.Run(side, func(t *testing.T) {
			p := newLinkPair(t)
			pl := p.link(t, "", time.Now().Add(time.Hour), time.Now().Add(time.Hour))
			l := pl.Links[0].Link
			p.a.apply(pl, false)
			p.b.apply(pl, false)
			c := connect(t, l)
			defer c.Close()
			m := p.a
			if side == "target" {
				m = p.b
			}
			if n := m.cutConns(func(owner string) bool { return owner == "bob" }); n != 0 {
				t.Fatalf("cut %d links of bob", n)
			}
			if n := m.cutConns(func(owner string) bool { return owner == "alice" }); n != 1 {
				t.Fatalf("cut %d links of alice", n)
			}
			waitCut(t, c, 5*time.Second)
			connect(t, l).Close() // the link still works (re-enabled; the target checks the policy)
		})
	}
}

// TestLinkSourcePolicy: the source worker's own policy wins over the hub's
// list - no listener (and no variables) while it refuses the owner, local
// connections refused once it does again.
func TestLinkSourcePolicy(t *testing.T) {
	old := linkListenRetry
	linkListenRetry = 100 * time.Millisecond
	t.Cleanup(func() { linkListenRetry = old })
	p := newLinkPair(t)
	var deny atomic.Value
	deny.Store("the link's owner is not a user of this worker ([access])")
	p.a.ownerDeny = func(string) string { return deny.Load().(string) }
	pl := p.link(t, "", time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	l := pl.Links[0].Link
	p.a.apply(pl, false)
	p.b.apply(pl, false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := p.a.statusReport().GetLinks()
		if len(st) == 1 && st[0].State == "error" && strings.Contains(st[0].Error, "refused by this worker's policy") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state while refused: %v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if listening(l) || len(p.a.Env("alice")) != 0 {
		t.Fatal("listening (or variables) while the policy refuses the owner")
	}
	deny.Store("")
	connect(t, l).Close()
	deny.Store("user alice is disabled")
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(l.ListenPort))))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitCut(t, c, 5*time.Second)
	if st := p.a.statusReport().GetLinks(); len(st) != 1 || st[0].Refused == 0 {
		t.Fatalf("refused not counted: %v", st)
	}
}

// TestLinkSourceConsent: with end-to-end trust the source listens only for
// links its owner signed: not for one the hub made alone, nor for one
// changed after signing.
func TestLinkSourceConsent(t *testing.T) {
	old := linkListenRetry
	linkListenRetry = 100 * time.Millisecond
	t.Cleanup(func() { linkListenRetry = old })
	p := newLinkPair(t)
	_, alice, _ := ed25519.GenerateKey(rand.Reader)
	p.a.trust = &pki.TrustSource{}
	p.a.trust.Store(&pki.TrustPolicy{Users: map[string]ed25519.PublicKey{"alice": alice.Public().(ed25519.PublicKey)}})
	// wa (the listening end) is alice's endorsed worker: a consent names
	// its ends, and each end holds itself to its own endorsement
	p.a.bundle = endorsedBundle(t, p.ca, "wa", alice)
	_, nk, _ := ed25519.GenerateKey(rand.Reader)
	e, err := pki.Endorse(alice, "alice", pki.KindDevice, "alice-laptop", nk.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(l *pb.PortLink) {
		c, err := portlink.SignConsent(nk, e, &pb.PortLinkSpec{Id: l.Id, From: l.From, To: l.To, Port: l.Port, ListenPort: l.ListenPort,
			ExpiresUnix: l.ExpiresAt.AsTime().Unix()})
		if err != nil {
			t.Fatal(err)
		}
		l.Consent = c
	}
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	forged := p.link(t, "", exp, exp).Links[0]
	signed := p.link(t, "", exp, exp).Links[0]
	sign(signed.Link)
	changed := p.link(t, "", exp, exp).Links[0]
	sign(changed.Link)
	changed.Link.ListenPort = forged.Link.ListenPort + 1 // after signing
	p.a.apply(&pb.PortLinks{Version: 1, Links: []*pb.PortLinkAssignment{forged, signed, changed}}, false)
	state := func(id string) string {
		for _, s := range p.a.statusReport().GetLinks() {
			if s.Id == id {
				return s.State + " " + s.Error
			}
		}
		return ""
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(state(forged.Link.Id), "no owner's consent") || !strings.Contains(state(changed.Link.Id), "not what its owner signed") ||
		!strings.HasPrefix(state(signed.Link.Id), "listening") {
		if time.Now().After(deadline) {
			t.Fatalf("states: forged %q, changed %q, signed %q", state(forged.Link.Id), state(changed.Link.Id), state(signed.Link.Id))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if listening(forged.Link) || !listening(signed.Link) {
		t.Fatal("listening for the hub-made link, or not for the signed one")
	}
}

// The source sends only its own link's grant: a hub handing listener X the
// grant of another link Y (same owner and workers, both consented) would
// carry X's connections to Y's port (review finding).
func TestGrantMismatch(t *testing.T) {
	p := newLinkPair(t)
	x := p.link(t, "", time.Now().Add(time.Hour), time.Now().Add(time.Hour)).Links[0]
	y := p.link(t, "", time.Now().Add(time.Hour), time.Now().Add(time.Hour)).Links[0]
	if why := grantMismatch(p.a.Roots(), x.Grant, x.Link); why != "" {
		t.Fatalf("own grant refused: %s", why)
	}
	if why := grantMismatch(p.a.Roots(), y.Grant, x.Link); !strings.Contains(why, "another link") {
		t.Fatalf("another link's grant: %q", why)
	}
	if why := grantMismatch(p.a.Roots(), nil, x.Link); why == "" {
		t.Fatal("no grant accepted")
	}
}

// endorsedBundle is a worker bundle named name whose certificate carries
// user's endorsement (kind worker, name-bound).
func endorsedBundle(t *testing.T, ca *pki.CA, name string, userKey ed25519.PrivateKey) *pki.Bundle {
	t.Helper()
	_, nk, _ := ed25519.GenerateKey(rand.Reader)
	e, err := pki.Endorse(userKey, "alice", pki.KindWorker, name, nk.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, csr, err := pki.NewKeyAndCSR(name, &pki.CSRTrust{NodeKey: nk, Endorsement: e})
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignCSR(csr, pki.IssueOptions{Role: pki.RoleWorker, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	b, err := pki.ParseBundle(ca.CertPEM, certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
