package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// pinWorker is a fake worker "w1" whose node can be swapped: A, then A'
// (same name, another node key) - what a hub steering a reconnect could
// present. It records the secrets it receives.
type pinWorker struct {
	pb.UnimplementedNodeServer
	pb.UnimplementedExecServer
	mu      sync.Mutex
	got     []string // which node received a request with secrets
	current atomic.Pointer[tls.Certificate]
	name    atomic.Value
	conns   []net.Conn
}

func (w *pinWorker) record(secrets map[string]string) {
	if len(secrets) > 0 {
		w.mu.Lock()
		w.got = append(w.got, w.name.Load().(string))
		w.mu.Unlock()
	}
}

func (w *pinWorker) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{Name: "w1"}, nil
}
func (w *pinWorker) Run(_ context.Context, r *pb.ExecRequest) (*pb.ExecResponse, error) {
	w.record(r.SecretEnv)
	return &pb.ExecResponse{}, nil
}

type pinJobs struct {
	pb.UnimplementedJobsServer
	w *pinWorker
}

type pinSessions struct {
	pb.UnimplementedSessionsServer
	w *pinWorker
}

func (j pinJobs) Submit(_ context.Context, r *pb.SubmitRequest) (*pb.Job, error) {
	j.w.record(r.SecretEnv)
	return &pb.Job{Spec: r.Spec}, nil
}
func (ss pinSessions) Attach(st pb.Sessions_AttachServer) error {
	m, err := st.Recv()
	if err != nil {
		return err
	}
	ss.w.record(m.GetOpen().GetSecretEnv())
	return st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_ExitCode{ExitCode: 0}})
}

// trackingListener remembers accepted connections so a test can cut them.
type trackingListener struct {
	net.Listener
	w *pinWorker
}

func (l trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.w.mu.Lock()
		l.w.conns = append(l.w.conns, c)
		l.w.mu.Unlock()
	}
	return c, err
}

// swap makes the worker present node (cert) from now on and cuts the
// current connections, so the client reconnects.
func (w *pinWorker) swap(name string, cert *tls.Certificate) {
	w.name.Store(name)
	w.current.Store(cert)
	w.mu.Lock()
	for _, c := range w.conns {
		c.Close()
	}
	w.conns = nil
	w.mu.Unlock()
}

// nodeCert issues a w1 certificate whose trust extension carries a fresh
// node key.
func nodeCert(t *testing.T, ca *pki.CA) *tls.Certificate {
	t.Helper()
	_, nk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, csrPEM, err := pki.NewKeyAndCSR("w1", &pki.CSRTrust{NodeKey: nk})
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignCSR(csrPEM, pki.IssueOptions{Role: pki.RoleWorker, Name: "w1", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &c
}

// An operation authorized for node A (PinPeer) never sends anything over a
// reconnect that reached A' - same worker name, another node key: not an
// exec, not a job submit, not an attach open. Without a pin the client
// follows the reconnect (the behaviour pinning guards against).
func TestPinPeerRefusesAnotherNodeOnReconnect(t *testing.T) {
	e := newRaceEnv(t)
	a, a2 := nodeCert(t, e.ca), nodeCert(t, e.ca)
	w := &pinWorker{}
	w.name.Store("A")
	w.current.Store(a)
	cfg, err := e.worker.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return w.current.Load(), nil }
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
	pb.RegisterNodeServer(srv, w)
	pb.RegisterExecServer(srv, w)
	pb.RegisterJobsServer(srv, pinJobs{w: w})
	pb.RegisterSessionsServer(srv, pinSessions{w: w})
	go srv.Serve(trackingListener{ln, w})
	t.Cleanup(srv.Stop)
	worker := Worker{Name: "w1", CertName: "w1", Transport: "tcp", Address: ln.Addr().String()}
	secret := map[string]string{"COPILOT_GITHUB_TOKEN": "value"}
	ctx := func() context.Context {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		t.Cleanup(cancel)
		return c
	}

	conn, err := e.d.Dial(worker)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var pr peer.Peer
	if _, err := conn.Node().Info(ctx(), &pb.InfoRequest{}, grpc.Peer(&pr)); err != nil {
		t.Fatal(err)
	}
	if err := conn.PinPeer(pr.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec().Run(ctx(), &pb.ExecRequest{Argv: []string{"copilot"}, SecretEnv: secret}); err != nil {
		t.Fatal(err)
	}

	w.swap("A'", a2) // the reconnect reaches another node
	// (a call can still meet the cut connection: retried like the CLI does,
	// until the reconnect is refused - which is not retried)
	err = Retry(ctx(), 5*time.Second, nil, func(c context.Context, _ func()) error {
		_, err := conn.Exec().Run(c, &pb.ExecRequest{Argv: []string{"copilot"}, SecretEnv: secret})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), PeerChanged) || Retryable(err) {
		t.Fatalf("exec after the swap: %v", err)
	}
	err = Retry(ctx(), 5*time.Second, nil, func(c context.Context, _ func()) error {
		_, err := conn.Jobs().Submit(c, &pb.SubmitRequest{Spec: &pb.JobSpec{Id: "J1", Argv: []string{"copilot"}}, SecretEnv: secret})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), PeerChanged) {
		t.Fatalf("submit after the swap: %v", err)
	}
	if st, err := conn.Sessions().Attach(ctx()); err == nil {
		if st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Open{Open: &pb.AttachOpen{Name: "s", SecretEnv: secret}}}) == nil {
			if _, err := st.Recv(); err == nil { // (the open must not have reached A': checked below)
				t.Fatalf("attach after the swap: %v", err)
			}
		}
	}
	w.mu.Lock()
	got := append([]string(nil), w.got...)
	w.mu.Unlock()
	if len(got) != 1 || got[0] != "A" {
		t.Fatalf("secrets reached %v; want A only", got)
	}

	// Pinning after a reconnect to another node than the one checked fails.
	conn2, err := e.d.Dial(worker)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	var p2 peer.Peer
	if _, err := conn2.Node().Info(ctx(), &pb.InfoRequest{}, grpc.Peer(&p2)); err != nil { // sees A'
		t.Fatal(err)
	}
	checked := p2.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0]
	w.swap("A", a)
	if err := Retry(ctx(), 5*time.Second, nil, func(c context.Context, _ func()) error { // unpinned: follows to A
		_, err := conn2.Node().Info(c, &pb.InfoRequest{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := conn2.PinPeer(checked); err == nil || !strings.Contains(err.Error(), PeerChanged) {
		t.Fatalf("pin of a node the connection no longer has: %v", err)
	}
}
