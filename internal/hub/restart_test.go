package hub

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestRestartAnnounced: a hub that stops tells its connected workers
// (HubCommand.restarting) and the event subscribers that asked for kind
// "hub" before the streams close; subscribers that did not ask get no such
// event (older clients).
func TestRestartAnnounced(t *testing.T) {
	dir := t.TempDir()
	ca, err := InitCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "off"}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	hctx, stopHub := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Serve(hctx); close(done) }()
	defer func() { stopHub(); <-done }()
	for h.Addr("grpc") == "" {
		time.Sleep(10 * time.Millisecond)
	}
	dial := func(o pki.IssueOptions) *grpc.ClientConn {
		certPEM, keyPEM, err := ca.Issue(o)
		if err != nil {
			t.Fatal(err)
		}
		bdir := filepath.Join(dir, "c-"+o.Name)
		if err := pki.WriteBundle(bdir, ca.CertPEM, certPEM, keyPEM); err != nil {
			t.Fatal(err)
		}
		b, err := pki.LoadBundle(bdir)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "alice", Create: true}); err != nil {
		t.Fatal(err)
	}
	alice := dial(pki.IssueOptions{Role: pki.RoleClient, Name: "alice"})
	jc, err := pb.NewFleetClient(alice).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Name: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := hubclient.Join(tctx(t), h.Addr("grpc"), jc.Code, hubclient.JoinOptions{Fingerprint: pki.Fingerprint(ca.Cert)})
	if err != nil {
		t.Fatal(err)
	}
	b := writeTestBundle(t, h, r)
	wconn, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
	if err != nil {
		t.Fatal(err)
	}
	defer wconn.Close()
	ctx := tctx(t)
	st, err := pb.NewHubNodeClient(wconn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Send(&pb.NodeReport{Msg: &pb.NodeReport_Hello{Hello: &pb.NodeHello{Version: "test"}}}); err != nil {
		t.Fatal(err)
	}
	if m, err := st.Recv(); err != nil || m.GetWelcome() == nil {
		t.Fatalf("no welcome: %v %v", m, err)
	}
	withHub, err := pb.NewFleetClient(alice).Events(ctx, &pb.EventsRequest{Kinds: []string{"node", "hub"}})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := pb.NewFleetClient(alice).Events(ctx, &pb.EventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// both subscriptions are registered once the hub has two subscribers
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		h.bus.mu.Lock()
		n := len(h.bus.subs)
		h.bus.mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("event subscriptions not registered")
		}
	}

	stopHub()
	var restarting *pb.HubRestarting
	for restarting == nil {
		m, err := st.Recv()
		if err != nil {
			t.Fatalf("worker session ended without a restart notice: %v", err)
		}
		restarting = m.GetRestarting()
	}
	if d := restarting.GetReconnectAfter().AsDuration(); d <= 0 || d > 10*time.Second {
		t.Fatalf("reconnect_after %s", d)
	}
	got := false
	for {
		ev, err := withHub.Recv()
		if err != nil {
			break
		}
		if ev.Kind == "hub" && ev.Message == "restarting" {
			got = true
		}
	}
	if !got {
		t.Fatal("the subscriber asking for kind hub got no restart event")
	}
	for {
		ev, err := plain.Recv()
		if err != nil {
			break
		}
		if ev.Kind == "hub" {
			t.Fatalf("a subscriber without kind hub got %v", ev)
		}
	}
	<-done
}
