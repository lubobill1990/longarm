package hub

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestConnectRevocationRace: a revocation that lands while a worker
// reconnects (after its hello, before its session is registered) still
// reaches it - with the list snapshot taken before registering, it got
// neither the broadcast nor the new list.
func TestConnectRevocationRace(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	join := func(name string) *hubclient.JoinResult {
		t.Helper()
		jc, err := pb.NewFleetClient(th.alice).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		r, err := hubclient.Join(tctx(t), h.Addr("grpc"), jc.Code, hubclient.JoinOptions{Fingerprint: pki.Fingerprint(th.ca.Cert)})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ra, _ := join("ra"), join("rb")
	h.mu.Lock()
	before := h.revVersion
	h.mu.Unlock()
	hook := func() {
		testHookConnect.Store(nil)
		if _, err := h.Revoke("test", "rb"); err != nil {
			t.Error(err)
		}
	}
	testHookConnect.Store(&hook)
	t.Cleanup(func() { testHookConnect.Store(nil) })

	b := writeTestBundle(t, h, ra)
	conn, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := pb.NewHubNodeClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Send(&pb.NodeReport{Msg: &pb.NodeReport_Hello{Hello: &pb.NodeHello{Version: "test", RevocationVersion: before}}}); err != nil {
		t.Fatal(err)
	}
	for {
		m, err := st.Recv()
		if err != nil {
			t.Fatalf("no revocation list after a revocation during the reconnect: %v", err)
		}
		if r := m.GetRevocations(); r != nil && r.Version > before {
			for _, c := range r.Certs {
				if c.Node == "rb" {
					return
				}
			}
			t.Fatalf("revocations %v lack rb", r)
		}
	}
}
