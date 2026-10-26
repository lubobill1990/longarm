package server

import (
	"context"
	"crypto/x509"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/portlink"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// fakeLinks is a LinkRegistry with links set by the test.
type fakeLinks struct {
	roots *x509.CertPool
	mu    sync.Mutex
	links map[string]*pb.PortLink
	ctxs  map[string]context.CancelFunc
	cctx  map[string]context.Context
	env   map[string]map[string]string
}

func (f *fakeLinks) Roots() *x509.CertPool { return f.roots }

func (f *fakeLinks) Incoming(id string) (*pb.PortLink, context.Context, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.links[id]
	if l == nil {
		return nil, nil, false
	}
	return l, f.cctx[id], true
}

func (f *fakeLinks) Env(owner string) map[string]string { return f.env[owner] }

func (f *fakeLinks) add(l *pb.PortLink) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	f.links[l.Id], f.cctx[l.Id], f.ctxs[l.Id] = l, ctx, cancel
}

func (f *fakeLinks) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxs[id]()
	delete(f.links, id)
}

// echoServer answers every connection with "hello " + what it reads.
func echoServer(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
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
	return ln.Addr().(*net.TCPAddr).Port
}

// TestLinkTunnel: the target side of a port link accepts a worker's
// stream only with a hub-signed grant naming the caller and this worker,
// for a link in its list; data flows; removal cuts the stream; refusals are
// audited. Jobs of the owner get the link variables.
func TestLinkTunnel(t *testing.T) {
	e := newAuthzEnv(t)
	root := t.TempDir()
	hubDir := filepath.Join(root, "hub")
	hc, hk, err := e.ca.Issue(pki.IssueOptions{Role: pki.RoleHub, Name: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteBundle(hubDir, e.ca.CertPEM, hc, hk); err != nil {
		t.Fatal(err)
	}
	hub, err := pki.LoadBundle(hubDir)
	if err != nil {
		t.Fatal(err)
	}
	links := &fakeLinks{roots: hub.Pool, links: map[string]*pb.PortLink{}, ctxs: map[string]context.CancelFunc{},
		cctx: map[string]context.Context{}, env: map[string]map[string]string{}}
	e.srv.Links = links
	port := echoServer(t)
	now := time.Now()
	link := &pb.PortLink{Id: portlink.NewID(), Owner: "alice", From: "wa", To: "w1", Port: int32(port), ListenPort: 18080,
		ExpiresAt: timestamppb.New(now.Add(time.Hour))}
	grantFor := func(l *pb.PortLink, signer *pki.Bundle) *pb.SignedPortLinkGrant {
		g, err := portlink.Sign(signer.Certificate(), &pb.PortLinkGrant{Id: l.Id, Owner: l.Owner, From: l.From, To: l.To,
			Port: l.Port, ListenPort: l.ListenPort, ExpiresUnix: l.ExpiresAt.AsTime().Unix(), IssuedUnix: now.Unix()})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	good := grantFor(link, hub)
	wa := e.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "wa"})
	wb := e.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "wb"})

	open := func(c *grpc.ClientConn, g *pb.SignedPortLinkGrant) (pb.LinkTunnel_DialClient, error) {
		st, err := pb.NewLinkTunnelClient(c).Dial(cctx(t))
		if err != nil {
			return nil, err
		}
		if err := st.Send(&pb.ForwardFrame{Grant: g}); err != nil {
			return nil, err
		}
		f, err := st.Recv()
		if err == nil && (len(f.Data) > 0 || f.Eof) {
			t.Fatalf("first answer is not the empty acceptance frame: %v", f)
		}
		return st, err
	}
	want := func(name string, err error, code codes.Code, text string) {
		t.Helper()
		if status.Code(err) != code || !strings.Contains(err.Error(), text) {
			t.Fatalf("%s: want %v %q, got %v", name, code, text, err)
		}
	}

	// not in this worker's list (yet)
	_, err = open(wa, good)
	want("unknown link", err, codes.FailedPrecondition, "not active")
	links.add(link)

	_, err = open(wa, nil)
	want("no grant", err, codes.PermissionDenied, "no link grant")
	_, err = open(wb, good)
	want("another worker uses the grant", err, codes.PermissionDenied, "not for wb")
	other := &pb.PortLink{Id: portlink.NewID(), Owner: "alice", From: "wa", To: "w9", Port: int32(port), ListenPort: 18081,
		ExpiresAt: link.ExpiresAt}
	_, err = open(wa, grantFor(other, hub))
	want("grant for another target", err, codes.PermissionDenied, "targets w9")
	waBundle := issueBundle(t, e, pki.IssueOptions{Role: pki.RoleWorker, Name: "wa"})
	_, err = open(wa, grantFor(link, waBundle))
	want("grant signed by a worker", err, codes.PermissionDenied, "not by the hub")
	// the hub's list says something else than the grant (port 22)
	evil := &pb.PortLink{Id: link.Id, Owner: "alice", From: "wa", To: "w1", Port: 22, ListenPort: 18080, ExpiresAt: link.ExpiresAt}
	_, err = open(wa, grantFor(evil, hub))
	want("grant and list disagree", err, codes.PermissionDenied, "disagree")
	// clients can't even call it
	_, err = open(e.alice, good)
	want("client", err, codes.PermissionDenied, "may not call")

	// the real thing
	st, err := open(wa, good)
	if err != nil {
		t.Fatal(err)
	}
	st.Send(&pb.ForwardFrame{Data: []byte("world")})
	var got []byte
	for !strings.Contains(string(got), "hello world") {
		f, err := st.Recv()
		if err != nil {
			t.Fatalf("recv: %v (got %q)", err, got)
		}
		got = append(got, f.Data...)
	}
	// removal cuts the open stream
	links.remove(link.Id)
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := st.Recv(); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err == io.EOF || status.Code(err) == codes.OK {
			t.Logf("stream ended: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("removing the link did not cut the stream")
	}
	_, err = open(wa, good)
	want("removed link", err, codes.FailedPrecondition, "not active")

	// a disabled owner's links are refused
	links.add(link)
	e.srv.Policy.Set(&pb.TenancyPolicy{Version: 2, Enabled: true, Users: []*pb.TenantUser{{Name: "alice", Disabled: true}}})
	_, err = open(wa, good)
	want("disabled owner", err, codes.PermissionDenied, "disabled")

	b, _ := os.ReadFile(filepath.Join(filepath.Dir(e.srv.DataDir), "audit.log"))
	audit := string(b)
	if strings.Count(audit, `"method":"/longarm.v1.LinkTunnel/Dial"`) < 8 || !strings.Contains(audit, `"who":"worker/wa"`) ||
		!strings.Contains(audit, `"denied":"link `+link.Id+` is not for wb"`) {
		t.Fatalf("audit:\n%s", audit)
	}
}

func issueBundle(t *testing.T, e *authzEnv, o pki.IssueOptions) *pki.Bundle {
	return issue(t, e.ca, filepath.Join(t.TempDir(), o.Name), o)
}

// TestLinkEnv: new jobs, sessions and exec of the owner get the link
// variables; variables the caller set win; a retried submit keeps the
// first submit's.
func TestLinkEnv(t *testing.T) {
	e := newAuthzEnv(t)
	links := &fakeLinks{env: map[string]map[string]string{"alice": portlink.Env([]portlink.Link{{To: "wb", Port: 8080, ListenPort: 18080}})}}
	e.srv.Links = links
	jobs := pb.NewJobsClient(e.alice)
	exe, _ := os.Executable()
	spec := &pb.JobSpec{Id: "LINKJOB", Argv: []string{exe}, Env: map[string]string{"LONGARM_TEST_SLEEP": "1", "LONGARM_LINK_WB_8080": "mine"}}
	j, err := jobs.Submit(cctx(t), &pb.SubmitRequest{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if j.Spec.Env[portlink.EnvLinks] != "wb:8080=127.0.0.1:18080" || j.Spec.Env["LONGARM_LINK_WB_8080"] != "mine" {
		t.Fatalf("env %v", j.Spec.Env)
	}
	// the links change; a retried submit is still the same job
	links.env["alice"] = nil
	if _, err := jobs.Submit(cctx(t), &pb.SubmitRequest{Spec: spec}); err != nil {
		t.Fatalf("retried submit: %v", err)
	}
	// bob's jobs get nothing of alice's
	jb, err := pb.NewJobsClient(e.bob).Submit(cctx(t), &pb.SubmitRequest{Spec: &pb.JobSpec{Id: "LINKJOBB", Argv: []string{exe},
		Env: map[string]string{"LONGARM_TEST_SLEEP": "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := jb.Spec.Env[portlink.EnvLinks]; ok {
		t.Fatalf("bob's env %v", jb.Spec.Env)
	}
}
